//go:build integration

package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// widgetFixture is a minimal tenant-scoped fixture used only to prove
// TestRedisQueue_RebuildsTenantContext against a real dbkit.Repository[T] --
// the same "define a small fixture directly" precedent go/jobs's own
// standalone_queue_test.go (parent package) and go/tenancy/tenancytest's sprocket
// fixture both establish (dbkit's own tenant-scoped test fixture lives in
// an unexported internal package this module cannot reach).
type widgetFixture struct {
	ID       string `gorm:"column:id;primaryKey;size:64"`
	TenantID string `gorm:"column:tenant_id;primaryKey;size:64"`
	Name     string `gorm:"column:name;size:255"`
}

// GetTenantID satisfies dbkit.TenantScoped.
func (w widgetFixture) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(w.TenantID) }

var _ dbkit.TenantScoped = widgetFixture{}

const createWidgetFixtureTableSQL = `CREATE TABLE widget_fixtures (
	id VARCHAR(64) NOT NULL,
	tenant_id VARCHAR(64) NOT NULL,
	name VARCHAR(255) NOT NULL DEFAULT '',
	PRIMARY KEY (tenant_id, id)
)`

// TestRedisQueue_RebuildsTenantContext is this suite's proof of "the
// tenant-context-rebuild trap applies identically here: a worker consuming
// from Redis must reconstruct pkgcore.WithTenant from the persisted task
// payload before calling Handle" (this task's own instructions) -- the
// distributed deployment mode's counterpart of standalone_queue_test.go's
// TestStandaloneQueue_RebuildsTenantContext_HandlerUsesOnlyJobTenant, run here
// against a REAL asynq.Server/worker goroutine dequeuing from REAL Redis,
// not StandaloneQueue's in-process dispatcher.
//
// Like its StandaloneQueue counterpart, the Handler performs a genuine
// tenant-scoped dbkit.Repository[T] call, and the Task is enqueued from a
// context.Background() carrying NO tenant at all: if Queue's worker
// ever regressed to calling Handle with asynq's own bare per-task context
// instead of pkgcore.WithTenant(ctx, tenantID) built from the Task's own
// stored tenant_id header (go/jobs/queue/asynq's worker.go's processTask), this
// Repository[T] call would fail closed with pkgcore.ErrNoTenant instead of
// finding the seeded row, and this test would fail with that error
// surfacing as the Job's own Error field -- exactly like the StandaloneQueue
// proof, just through a completely different, Redis-backed pipeline.
func TestRedisQueue_RebuildsTenantContext(t *testing.T) {
	ctx := context.Background()
	q := startTestAsynqQueue(t, ctx)

	db := dbtest.NewSQLite(t)
	if err := db.Exec(createWidgetFixtureTableSQL).Error; err != nil {
		t.Fatalf("create widget_fixtures table: %v", err)
	}
	repo := dbkit.NewRepository[widgetFixture](db)

	const widgetTenant = pkgcore.TenantID("widget-tenant")
	seedCtx := pkgcore.WithTenant(context.Background(), widgetTenant)
	if err := repo.Create(seedCtx, &widgetFixture{ID: "w1", Name: "gizmo"}); err != nil {
		t.Fatalf("seed widget: %v", err)
	}

	h := jobs.NewHandlerFunc("widget.lookup", func(ctx context.Context, _ *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
		// ctx here comes ONLY from Queue's own worker rebuild --
		// Repository[T].FindByID fails closed with pkgcore.ErrNoTenant if
		// that rebuild is ever skipped, which is exactly what makes this a
		// meaningful proof rather than a tautology.
		w, err := repo.FindByID(ctx, "w1")
		if err != nil {
			return jobs.Result{}, err
		}
		return jobs.Result{Data: []byte(w.Name)}, nil
	})
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	// Enqueued from a context carrying NO tenant at all: if the eventual
	// success below depended on some ambient tenant leaking through
	// instead of the worker's own rebuild, there would be no tenant here
	// for it to leak from.
	id, err := q.Enqueue(context.Background(), jobs.Task{Type: "widget.lookup", TenantID: widgetTenant})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	job := waitForTerminal(t, pkgcore.WithTenant(context.Background(), widgetTenant), q, id, 10*time.Second)

	if job.Status != jobs.StatusSucceeded {
		t.Fatalf("Status = %v, want %v (job: %+v)", job.Status, jobs.StatusSucceeded, job)
	}
	if job.Result == nil || string(job.Result.Data) != "gizmo" {
		t.Errorf("Result = %+v, want Data = %q", job.Result, "gizmo")
	}
}
