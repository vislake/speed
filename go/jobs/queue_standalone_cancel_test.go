package jobs

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// controlledFailureHandler blocks inside Handle until the test releases it,
// then always fails; every OnFailure call is recorded on onFailureCh. It is
// the live-worker counterpart of worker_test.go's
// cancelledBeforeDeadLetterHandler: Handle stays in flight long enough for
// the test to Cancel the Job mid-attempt, so the cancel-vs-final-failure
// race resolves deterministically in Cancel's favor.
type controlledFailureHandler struct {
	startedCh   chan JobID
	releaseCh   chan struct{}
	onFailureCh chan struct{}
}

func (*controlledFailureHandler) Type() string { return "cancel-race.live" }

func (h *controlledFailureHandler) Handle(_ context.Context, job *Job, _ ProgressFn) (Result, error) {
	h.startedCh <- job.ID
	<-h.releaseCh
	return Result{}, errors.New("permanent failure")
}

func (h *controlledFailureHandler) OnFailure(context.Context, *Job, error) {
	h.onFailureCh <- struct{}{}
}

var (
	_ Handler     = (*controlledFailureHandler)(nil)
	_ FailureHook = (*controlledFailureHandler)(nil)
)

// TestStandaloneQueue_CancelBeatsFinalFailure_NoOnFailure_DeadLetterNeverPersisted
// is the end-to-end, live-worker half of the same race worker_test.go's
// TestExecute_FinalFailureAfterCancel_DoesNotRunOnFailure proves at the
// execute level: a Job whose final attempt fails while its row is already
// StatusCancelled must not run OnFailure (no compensation for a
// deliberately cancelled Job) and must stay StatusCancelled -- never
// StatusDeadLetter. Deterministic without wall-clock sleeps: a single
// worker runs execute serially, and a sentinel Job's completion can only
// be observed after the cancelled Job's execute (dead-letter decision
// included) has fully returned. Running OnFailure for the cancelled Job
// would fail this test.
func TestStandaloneQueue_CancelBeatsFinalFailure_NoOnFailure_DeadLetterNeverPersisted(t *testing.T) {
	q := newTestQueue(t, WithWorkerCount(1))
	failer := &controlledFailureHandler{
		startedCh:   make(chan JobID, 1),
		releaseCh:   make(chan struct{}),
		onFailureCh: make(chan struct{}, 1),
	}
	if err := q.RegisterHandler(failer); err != nil {
		t.Fatalf("RegisterHandler(failer) error = %v", err)
	}
	sentinelDone := make(chan struct{})
	if err := q.RegisterHandler(NewHandlerFunc("cancel-race.sentinel", func(context.Context, *Job, ProgressFn) (Result, error) {
		close(sentinelDone)
		return Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler(sentinel) error = %v", err)
	}
	startQueue(t, q)
	// Registered after startQueue's Close cleanup, so cleaning up in LIFO
	// order releases the blocked Handle before the drain waits: an early
	// failure must not leave the worker mid-Handle, its attempt in flight,
	// past the end of the test.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(failer.releaseCh) }) }
	t.Cleanup(release)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	id, err := q.Enqueue(ctx, Task{Type: "cancel-race.live", TenantID: "tenant-a"}, WithMaxRetries(0))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	// Wait until the final attempt is actually in flight, then Cancel it
	// mid-attempt -- the row is now StatusCancelled before the attempt can
	// fail -- and only then release the attempt to fail.
	waitSignal(t, failer.startedCh, "the final attempt to start")
	if err = q.Cancel(ctx, id); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	release()

	// With one worker, the sentinel Job cannot run until the cancelled
	// Job's execute has returned -- so sentinelDone doubles as the
	// guarantee that the dead-letter decision is already made by the time
	// the assertions below run.
	if _, err = q.Enqueue(ctx, Task{Type: "cancel-race.sentinel", TenantID: "tenant-a"}); err != nil {
		t.Fatalf("Enqueue(sentinel) error = %v", err)
	}
	waitSignal(t, sentinelDone, "the sentinel job to run")

	select {
	case <-failer.onFailureCh:
		t.Fatal("OnFailure ran for a Job cancelled mid-attempt; the failure outcome of a cancelled Job must be discarded, never compensated")
	default:
	}

	got, err := q.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != StatusCancelled {
		t.Errorf("Status = %v, want %v (Cancel wins over the concurrent final failure; no dead-letter may be persisted)", got.Status, StatusCancelled)
	}
}
