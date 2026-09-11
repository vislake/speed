//go:build integration

package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore/testkit"
)

// failureHookOrderingTimeout bounds how long this file's test waits for
// OnFailure to fire and for the Job to reach its terminal state, matching
// the 5-second bound this package's other tests (cancel_test.go,
// progress_test.go, tenant_concurrency_test.go) already use against a real
// asynq/Redis backend.
const failureHookOrderingTimeout = 5 * time.Second

// onFailureOrderingHandler always fails, and its OnFailure reads the very
// Job it was just invoked for back through the same Queue -- the exact
// read-back a FailureHook author might reasonably attempt, trusting
// handler.go's FailureHook doc comment ("OnFailure runs at most once per
// Job, strictly after a worker has actually persisted job's Status as
// StatusDeadLetter"). It reports whatever Status that read observes on
// observed so the test can assert on it.
type onFailureOrderingHandler struct {
	q         jobs.Queue
	observed  chan jobs.Status
	getErrors chan error
}

func (*onFailureOrderingHandler) Type() string { return "onfailure-ordering" }

func (*onFailureOrderingHandler) Handle(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
	return jobs.Result{}, errors.New("permanent failure")
}

func (h *onFailureOrderingHandler) OnFailure(ctx context.Context, job *jobs.Job, _ error) {
	got, err := h.q.Get(ctx, job.ID)
	if err != nil {
		h.getErrors <- err
		return
	}
	h.observed <- got.Status
}

// TestAsynqQueue_OnFailure_ObservesTaskNotYetArchived pins the ordering
// divergence the FailureHook contract documents: go/jobs/queue/asynq's
// Queue does NOT satisfy handler.go's literal claim the way StandaloneQueue
// does -- a FailureHook that reads its own job back through Get() from
// inside OnFailure observes StatusRunning, never StatusDeadLetter, because
// asynq's own archival write (broker.Archive, processor.go's
// handleFailedMessage) has not happened yet at the point
// OnFailure runs: handleFailedMessage invokes the registered ErrorHandler
// (this package's handleError -> handleErrorAttempt -> OnFailure)
// unconditionally BEFORE the switch statement that decides retry-vs-archive,
// and asynq offers no separate post-archive hook anywhere in the library
// (confirmed against the pinned github.com/hibiken/asynq@v0.26.0 source,
// not assumed).
//
// This is a documentation-conformance test, not a classic bug-fix
// regression: it pins the honest, weaker distributed-mode contract (the
// ordering is asynq's own, with no post-archive hook to move the call
// after) so it cannot silently drift back out of sync with what the code
// actually does.
func TestAsynqQueue_OnFailure_ObservesTaskNotYetArchived(t *testing.T) {
	ctx := context.Background()
	q := startTestAsynqQueue(t, ctx)

	h := &onFailureOrderingHandler{
		q:         q,
		observed:  make(chan jobs.Status, 1),
		getErrors: make(chan error, 1),
	}
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	// MaxRetries(0): the very first failed attempt is already the terminal
	// one, so OnFailure fires (and this test observes) as early as possible.
	id, err := q.Enqueue(ctx, jobs.Task{Type: "onfailure-ordering", TenantID: "tenant-a"}, jobs.WithMaxRetries(0))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	var observed jobs.Status
	select {
	case observed = <-h.observed:
	case getErr := <-h.getErrors:
		t.Fatalf("Get() from inside OnFailure error = %v", getErr)
	case <-time.After(failureHookOrderingTimeout):
		t.Fatal("FailureHook.OnFailure was never called")
	}

	if observed != jobs.StatusRunning {
		t.Errorf("job Status observed from inside OnFailure = %v, want %v (task not yet archived at OnFailure time) -- "+
			"if this now reports %v, asynq's archival ordering changed underneath us and handler.go's "+
			"FailureHook doc comment plus this test both need revisiting",
			observed, jobs.StatusRunning, jobs.StatusDeadLetter)
	}

	// The Job DOES reach StatusDeadLetter -- just strictly after OnFailure
	// already ran, never before, which is the whole ordering this test
	// pins.
	final := waitForTerminal(t, testkit.TenantCtx("tenant-a"), q, id, failureHookOrderingTimeout)
	if final.Status != jobs.StatusDeadLetter {
		t.Fatalf("final Status = %v, want %v", final.Status, jobs.StatusDeadLetter)
	}
}
