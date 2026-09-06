package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// TestDispatchOnce_CandidateWindowDoesNotStarveOtherTenants is Finding
// P2-6's regression test: a single tenant with a backlog at or beyond
// claimBatchSize truly-eligible rows, every one of them older than another
// tenant's own single eligible row, must not prevent that other tenant's
// row from ever being selected into a dispatch tick's candidate window.
//
// worker.go's own dispatchOnce doc comment and go/jobs/AGENTS.md's
// per-tenant concurrency-limiting claim both used to describe only
// CONCURRENCY-admission fairness (a flooding tenant's excess Jobs bounce
// back immediately rather than blocking another tenant's dequeue, proven by
// TestPerTenantConcurrencyLimiting above) -- a distinct, narrower property
// than what this test proves: that claimCandidates' own SELECTION (its
// SQL's WHERE/ORDER BY/LIMIT shape, store.go) cannot let one tenant's
// backlog fill the entire claimBatchSize window every tick and starve
// another tenant's row out of ever being read back from the database in
// the first place, independent of concurrency admission entirely.
func TestDispatchOnce_CandidateWindowDoesNotStarveOtherTenants(t *testing.T) {
	q := newTestQueue(t, WithWorkerCount(4), WithTenantConcurrencyLimit(2))

	flood := &blockingHandler{startedCh: make(chan JobID, claimBatchSize+8), releaseCh: make(chan struct{})}
	if err := q.RegisterHandler(flood); err != nil {
		t.Fatalf("RegisterHandler(flood) error = %v", err)
	}
	quickDone := make(chan JobID, 1)
	if err := q.RegisterHandler(NewHandlerFunc("quick", func(_ context.Context, job *Job, _ ProgressFn) (Result, error) {
		quickDone <- job.ID
		return Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler(quick) error = %v", err)
	}

	base := time.Now()
	// A backlog well past claimBatchSize, every row deliberately scheduled
	// an hour in the past so it is both immediately eligible and strictly
	// older than tenant-starved's own job enqueued below -- if
	// claimCandidates has no per-tenant fairness dimension, every single
	// tick's top-claimBatchSize window (ordered "priority DESC,
	// scheduled_at ASC") is filled entirely from this backlog, and
	// tenant-starved's row is never even read back from the database, let
	// alone claimed.
	for i := 0; i < claimBatchSize+50; i++ {
		if _, err := q.Enqueue(context.Background(),
			Task{Type: "flood", TenantID: "tenant-flood"},
			WithScheduledAt(base.Add(-time.Hour)),
		); err != nil {
			t.Fatalf("Enqueue(flood %d) error = %v", i, err)
		}
	}
	quickID, err := q.Enqueue(context.Background(),
		Task{Type: "quick", TenantID: "tenant-starved"},
		WithScheduledAt(base),
	)
	if err != nil {
		t.Fatalf("Enqueue(quick) error = %v", err)
	}

	startQueue(t, q)
	t.Cleanup(func() {
		// Release every flood Job actually claimed (blocked or not yet
		// dispatched) so Close (registered by startQueue) never hangs
		// waiting on a worker goroutine still stuck in Handle.
		close(flood.releaseCh)
	})

	// tenant-flood's own per-tenant concurrency limit (2) means at most 2
	// of its Jobs ever run at once; each one blocks until releaseCh fires,
	// which keeps tenant-flood permanently saturated -- and its remaining
	// backlog permanently >= claimBatchSize -- for as long as this test
	// needs, with no need to ever actually drain it.
	for i := 0; i < 2; i++ {
		select {
		case <-flood.startedCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("flood job %d never started", i)
		}
	}

	// tenant-starved's single Job must still be claimed and run within a
	// bounded number of dispatch ticks (newTestQueue's poll interval is
	// 15ms; 2s is generous slack for the dispatcher goroutine to actually
	// run several times over), even though tenant-flood's backlog never
	// drops below claimBatchSize for the rest of the test.
	select {
	case id := <-quickDone:
		if id != quickID {
			t.Fatalf("quick handler ran Job %q, want %q", id, quickID)
		}
	case <-time.After(2 * time.Second):
		ctx := pkgcore.WithTenant(context.Background(), "tenant-starved")
		job, getErr := q.Get(ctx, quickID)
		if getErr != nil {
			t.Fatalf("tenant-starved's Job was never claimed within 2s, and Get() also failed: %v", getErr)
		}
		t.Fatalf("tenant-starved's Job was never claimed within 2s despite tenant-flood sitting at its concurrency limit the whole time (last observed status: %v) -- a single tenant's backlog starved another tenant out of the candidate-selection window", job.Status)
	}
}
