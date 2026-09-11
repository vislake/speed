package jobs

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// testDBComponent is the host component that provides the *gorm.DB the
// queue.standalone component requires: the product is the connection itself
// (dbtest's own cleanup owns it), and the name is outside the built-in
// namespace.
func testDBComponent(db *gorm.DB) pkgcore.Component {
	return pkgcore.Component{
		Name:     "db.test",
		Provides: []any{(*gorm.DB)(nil)},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return db, nil
		},
	}
}

// handlerDeclarer is the host component that declares one job handler on the
// Jobs seat during Init -- the declaration the queue component's Start wires
// onto the queue once every Init callback has run.
func handlerDeclarer(jobType string, ran chan<- JobID) pkgcore.Component {
	return pkgcore.Component{
		Name: "handler.declarer",
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &struct{}{}, nil
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
			return reg.Jobs.Handle(jobType, NewHandlerFunc(jobType, func(_ context.Context, job *Job, _ ProgressFn) (Result, error) {
				ran <- job.ID
				return Result{}, nil
			}))
		},
	}
}

// TestQueueStandaloneComponent_WellFormed runs the descriptor through the
// component contract: the naming convention, the decodable schema, the
// typed tokens.
func TestQueueStandaloneComponent_WellFormed(t *testing.T) {
	t.Parallel()
	componenttest.AssertWellFormed(t, queueStandaloneComponent)
}

// TestQueueStandaloneComponent_DeclaresCapabilities pins the declaration: a
// shared-database queue is MultiReplicaSafe (its state lives in the database
// every replica shares, and the single-writer gate fails closed rather than
// splitting work) and SurvivesRestart (a restarted replica's jobs come back
// from the database, with interrupted claims recovered).
func TestQueueStandaloneComponent_DeclaresCapabilities(t *testing.T) {
	t.Parallel()
	if want := pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart; queueStandaloneComponent.Capabilities != want {
		t.Errorf("queue.standalone capabilities = %v, want %v", queueStandaloneComponent.Capabilities, want)
	}
}

// TestQueueStandaloneComponent_StartsWiresAndDrains drives the whole
// component lifecycle: Init declares a handler on the Jobs seat, Start
// wires it onto the queue and starts the worker pool, an enqueued job runs
// through the declared handler, Stop signals, and Close drains.
func TestQueueStandaloneComponent_StartsWiresAndDrains(t *testing.T) {
	ctx := context.Background()
	db := dbtest.NewSQLite(t)

	ran := make(chan JobID, 1)
	reg := pkgcore.NewComponentRegistry()
	for _, c := range []pkgcore.Component{
		testDBComponent(db),
		handlerDeclarer("component.queue.test", ran),
	} {
		if err := reg.Register(c); err != nil {
			t.Fatalf("Register(%q) error = %v", c.Name, err)
		}
	}
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{
			"db.test":          nil,
			"handler.declarer": nil,
			"queue.standalone": map[string]any{
				"poll_interval": "10ms",
				"backoff_base":  "10ms",
				"backoff_max":   "50ms",
			},
		},
	}))

	for _, stage := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"prepare", reg.Prepare},
		{"construct", reg.Construct},
		{"verify", reg.Verify},
		{"init", reg.Init},
		{"start", reg.Start},
	} {
		if err := stage.run(ctx); err != nil {
			t.Fatalf("%s stage error = %v", stage.name, err)
		}
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := reg.Close(closeCtx); err != nil {
			t.Errorf("Close() error = %v, want the queue drained", err)
		}
	})

	queue, err := pkgcore.Get[Queue](reg)
	if err != nil {
		t.Fatalf("Get[Queue] error = %v, want the constructed queue", err)
	}
	id, err := queue.Enqueue(ctx, Task{Type: "component.queue.test", TenantID: "component-tenant"})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	select {
	case got := <-ran:
		if got != id {
			t.Errorf("the declared handler ran job %q, want %q", got, id)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the declared handler never ran job %q: Start must wire the Jobs seat and start the worker pool", id)
	}

	// Stop is the non-blocking signal; the Close in the cleanup drains.
	if err := reg.Stop(ctx); err != nil {
		t.Errorf("Stop() error = %v, want nil", err)
	}
}

// TestQueueStandaloneComponent_WorkerDisabled pins the worker:false
// expression: Start still wires the queue (Enqueue finds its table), but no
// worker runs, so an enqueued job stays pending.
func TestQueueStandaloneComponent_WorkerDisabled(t *testing.T) {
	ctx := context.Background()
	db := dbtest.NewSQLite(t)

	ran := make(chan JobID, 1)
	reg := pkgcore.NewComponentRegistry()
	for _, c := range []pkgcore.Component{
		testDBComponent(db),
		handlerDeclarer("component.queue.disabled", ran),
	} {
		if err := reg.Register(c); err != nil {
			t.Fatalf("Register(%q) error = %v", c.Name, err)
		}
	}
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{
			"db.test":          nil,
			"handler.declarer": nil,
			"queue.standalone": map[string]any{
				"worker":        false,
				"poll_interval": "10ms",
			},
		},
	}))

	for _, stage := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"prepare", reg.Prepare},
		{"construct", reg.Construct},
		{"verify", reg.Verify},
		{"init", reg.Init},
		{"start", reg.Start},
	} {
		if err := stage.run(ctx); err != nil {
			t.Fatalf("%s stage error = %v", stage.name, err)
		}
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := reg.Close(closeCtx); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	queue, err := pkgcore.Get[Queue](reg)
	if err != nil {
		t.Fatalf("Get[Queue] error = %v, want the constructed queue", err)
	}
	// Wiring happened even with the worker disabled: the table exists, so
	// the enqueue lands.
	id, err := queue.Enqueue(ctx, Task{Type: "component.queue.disabled", TenantID: "component-tenant"})
	if err != nil {
		t.Fatalf("Enqueue() error = %v, want the wired table to accept it", err)
	}

	// No worker exists at all, so the job cannot run: a wait generous
	// against the poll cadence observes it staying pending.
	select {
	case got := <-ran:
		t.Fatalf("the declared handler ran job %q despite worker:false", got)
	case <-time.After(200 * time.Millisecond):
	}

	tenantCtx := pkgcore.WithTenant(ctx, "component-tenant")
	job, err := queue.Get(tenantCtx, id)
	if err != nil {
		t.Fatalf("Get(%q) error = %v", id, err)
	}
	if job.Status != StatusPending {
		t.Errorf("job status = %v, want %v: a worker-disabled replica must not claim it", job.Status, StatusPending)
	}
}
