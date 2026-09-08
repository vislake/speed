//go:build integration

package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// cancelDuringFinalAttemptHandler always fails from Handle and records every
// OnFailure call it receives on onFailureCh. Handle blocks on started/release
// so the test can Cancel the Job mid-attempt; it deliberately treats ctx
// cancellation as irrelevant -- select with ctx.Done mirrors what a Handler
// that does NOT honor cancellation still looks like to asynq's dispatch loop,
// and in any case asynq itself routes a ctx-cancelled terminal attempt
// through handleFailedMessage and this package's ErrorHandler hook (see the
// test's own doc comment) whether or not the Handler cooperates.
type cancelDuringFinalAttemptHandler struct {
	startedCh   chan struct{}
	releaseCh   chan struct{}
	onFailureCh chan struct{}
}

func (*cancelDuringFinalAttemptHandler) Type() string { return "cancel-final-attempt" }

func (h *cancelDuringFinalAttemptHandler) Handle(ctx context.Context, _ *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
	close(h.startedCh)
	select {
	case <-h.releaseCh:
	case <-ctx.Done():
		// Queue.Cancel's best-effort CancelProcessing signal may cancel this
		// ctx before the test releases the attempt -- the dispatch loop then
		// fails the attempt on its own; either path is acceptable, what this
		// test asserts is that no OnFailure follows in either interleaving.
	}
	return jobs.Result{}, errors.New("permanent failure")
}

func (h *cancelDuringFinalAttemptHandler) OnFailure(context.Context, *jobs.Job, error) {
	h.onFailureCh <- struct{}{}
}

// TestRedisQueue_CancelDuringFinalAttempt_SkipsOnFailureHook pins the
// cancel-wins delivery rule on the asynq-backed Queue: when a concurrent
// Cancel has settled the Job as StatusCancelled while its final attempt is
// still executing, a FailureHook's OnFailure must not run -- business
// compensation for a Job the caller deliberately cancelled would violate
// the same boundary handler.go's FailureHook doc comment promises and
// StandaloneQueue's completeDeadLetter transition report enforces (jobs'
// worker.go). The terminal-attempt failure path consults the cancellation
// marker before invoking OnFailure. With MaxRetries(0) the very first
// attempt is terminal, so Cancel landing mid-attempt is by construction
// "while that final attempt was executing".
//
// The interleaving is deterministic in Cancel's favor: Cancel returns only
// after the cancellation marker is durably written -- and writes it BEFORE
// sending its best-effort CancelProcessing signal -- so by the time asynq's
// dispatch loop processes this attempt's failure through handleFailedMessage
// (in whichever of its interleavings fires: the handler's own returned
// error, or the ctx cancellation the CancelProcessing signal itself caused,
// which handleFailedMessage turns into a failure of the very same terminal
// attempt), this package's ErrorHandler sees the marker and skips OnFailure;
// OnFailure firing regardless of the marker would fail this test.
func TestRedisQueue_CancelDuringFinalAttempt_SkipsOnFailureHook(t *testing.T) {
	ctx := context.Background()
	q := startTestAsynqQueue(t, ctx)

	h := &cancelDuringFinalAttemptHandler{
		startedCh:   make(chan struct{}),
		releaseCh:   make(chan struct{}),
		onFailureCh: make(chan struct{}, 1),
	}
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const tenant = pkgcore.TenantID("tenant-a")
	id, err := q.Enqueue(context.Background(), jobs.Task{Type: h.Type(), TenantID: tenant}, jobs.WithMaxRetries(0))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	select {
	case <-h.startedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Handle never started")
	}

	tctx := tenantCtx(tenant)
	if cancelErr := q.Cancel(tctx, id); cancelErr != nil {
		t.Fatalf("Cancel() error = %v", cancelErr)
	}
	close(h.releaseCh)

	// OnFailure must never fire: the Job's terminal state is StatusCancelled
	// (Get reports it from the marker immediately, well before asynq's own
	// dispatch loop has even finished processing the interrupted attempt),
	// and the failure-processing that attempt goes through is exactly what
	// the delivery gate keys on the marker. The 5s window -- the same bound
	// the other
	// integration tests here use -- is far wider than the sub-second
	// localhost round trips in which an unguarded OnFailure could fire, and
	// nothing can fire it later: a cancelled Job is never dispatched again
	// (processTask's own marker check), so no later attempt exists to fail.
	select {
	case <-h.onFailureCh:
		t.Fatal("OnFailure ran for a Job a concurrent Cancel already settled as StatusCancelled; the cancellation wins over the final attempt's failure and no compensation may run")
	case <-time.After(5 * time.Second):
	}

	job, err := q.Get(tctx, id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if job.Status != jobs.StatusCancelled {
		t.Fatalf("Status = %v, want %v (the cancellation marker is the authoritative terminal state)", job.Status, jobs.StatusCancelled)
	}
}
