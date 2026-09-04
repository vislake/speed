//go:build integration

package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/jobs/queue/asynq"
	"github.com/vislake/speed/go/pkgcore"
)

// blockingHandler signals startedCh with its Job's id, then blocks until
// the test sends on releaseCh -- mirrors standalone_queue_test.go's
// identically-named type in the parent package.
type blockingHandler struct {
	jobType   string
	startedCh chan jobs.JobID
	releaseCh chan struct{}
}

func (h *blockingHandler) Type() string { return h.jobType }

func (h *blockingHandler) Handle(_ context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
	h.startedCh <- job.ID
	<-h.releaseCh
	return jobs.Result{}, nil
}

// TestRedisQueue_PerTenantConcurrencyLimiting is the distributed
// deployment mode's counterpart of standalone_queue_test.go's TestPerTenantConcurrencyLimiting:
// proof that one tenant's backlog cannot starve another tenant's Jobs, and
// that go/jobs/queue/asynq's Queue's own admission gate (its worker.go's
// tryReserveTenantSlot, layered on top of asynq -- see AGENTS.md's
// "Per-tenant concurrency limiting" section for why asynq offers nothing
// equivalent natively) is actually enforced against a real asynqlib.Server
// dequeuing from real Redis, not merely documented.
//
// Concurrency is deliberately small (2) relative to the flood size (3) so
// that, exactly as in the standalone deployment mode's own proof, more
// than one of tenant-a's jobs is available to be dequeued at once -- the
// scenario that would starve
// tenant-b if Queue bounced an over-limit job by blocking inside
// processTask instead of returning errTenantAtCapacity immediately (see
// errTenantAtCapacity's own doc comment).
func TestRedisQueue_PerTenantConcurrencyLimiting(t *testing.T) {
	ctx := context.Background()
	q := startTestAsynqQueue(t, ctx,
		asynq.WithConcurrency(2),
		asynq.WithTenantConcurrencyLimit(1),
	)

	flood := &blockingHandler{jobType: "flood", startedCh: make(chan jobs.JobID, 8), releaseCh: make(chan struct{})}
	if err := q.RegisterHandler(flood); err != nil {
		t.Fatalf("RegisterHandler(flood) error = %v", err)
	}
	quickDone := make(chan struct{})
	quick := jobs.NewHandlerFunc("quick", func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
		close(quickDone)
		return jobs.Result{}, nil
	})
	if err := q.RegisterHandler(quick); err != nil {
		t.Fatalf("RegisterHandler(quick) error = %v", err)
	}

	const tenantA = pkgcore.TenantID("tenant-a")
	const tenantB = pkgcore.TenantID("tenant-b")

	var floodIDs []jobs.JobID
	for i := 0; i < 3; i++ {
		id, err := q.Enqueue(context.Background(), jobs.Task{Type: "flood", TenantID: tenantA})
		if err != nil {
			t.Fatalf("Enqueue(flood %d) error = %v", i, err)
		}
		floodIDs = append(floodIDs, id)
	}
	if _, err := q.Enqueue(context.Background(), jobs.Task{Type: "quick", TenantID: tenantB}); err != nil {
		t.Fatalf("Enqueue(quick) error = %v", err)
	}

	var firstStarted jobs.JobID
	select {
	case firstStarted = <-flood.startedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("no flood job started at all")
	}

	// tenant-b's job completes promptly despite tenant-a's flood already
	// occupying a worker -- the core proof that one tenant cannot starve
	// another.
	select {
	case <-quickDone:
	case <-time.After(5 * time.Second):
		t.Fatal("tenant-b's job never completed while tenant-a's flood held the queue")
	}

	// A second tenant-a job must NOT start while the first is still
	// running: tenant-a is at its concurrency limit of 1. This window
	// (500ms) is generous relative to asynq.WithThrottleRetryDelay's 20ms
	// base configured by newTestAsynqQueue -- long enough that a bounced
	// job would have been redelivered and re-bounced several times over if
	// the gate were not actually holding.
	select {
	case second := <-flood.startedCh:
		t.Fatalf("a second tenant-a job (%q) started while the first (%q) was still running; the per-tenant concurrency limit was not enforced", second, firstStarted)
	case <-time.After(500 * time.Millisecond):
	}

	flood.releaseCh <- struct{}{}
	select {
	case <-flood.startedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("second flood job never started after the first was released")
	}
	flood.releaseCh <- struct{}{}
	select {
	case <-flood.startedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("third flood job never started after the second was released")
	}
	flood.releaseCh <- struct{}{}

	tenantACtx := tenantCtx(tenantA)
	for _, id := range floodIDs {
		waitForTerminal(t, tenantACtx, q, id, 10*time.Second)
	}
}
