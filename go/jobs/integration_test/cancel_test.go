//go:build integration

package jobs_test

import (
	"context"
	"sync"
	"testing"
	"time"

	asynqlib "github.com/hibiken/asynq"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/jobs/queue/asynq"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/testkit"
)

// The two Cancel scenarios specific to the asynq-backed Queue, pinned here
// end to end against a real asynq/Redis pipeline. The portable cancel
// semantics -- the tenant-scoped access rule, idempotency, and the
// cancelled status Get() reports off the intact task record -- are proven
// for both Queue implementations by the shared conformance suite's
// "cancel_tenant_isolation_and_idempotency" subtest (driven here by
// TestAsynqQueue_ConformsToQueueContract, queue_conformance_test.go).
//
//   - A Job cancelled while still Scheduled must not run when its due
//     time arrives: Queue.Cancel leaves the task's asynq record alone and
//     writes the marker, which processTask checks before ever calling
//     Handle on that first dispatch (on asynq's own cycle the scheduled
//     task is forwarded to pending at its due time, dequeued, and the
//     skip is what records it Completed).
//     TestRedisQueue_Cancel_ScheduledJob_SuppressedAtDueTime pins that.
//
//   - A Job cancelled while StatusRunning is marked StatusCancelled
//     immediately, without waiting for the in-flight Handle call:
//     TestRedisQueue_Cancel_RunningJob pins that.
//
// The dispatch-suppression decision itself (marker present -> skip; marker
// unreadable -> refuse, fail closed) is pinned at the unit tier against a
// bare Queue by queue/asynq's marker_fail_closed_test.go's
// TestQueue_DispatchAfterMarkerRead_FailsClosedOnUnreadableMarker. What
// this file proves on asynq's own delivery path -- scheduler, forwarder
// and processor -- is the skip half: a marker present suppresses the run.
// The refuse half on that same path is the sibling
// marker_read_fail_closed_test.go's.

// TestRedisQueue_Cancel_ScheduledJob_SuppressedAtDueTime proves the
// marker-suppression path end to end: a Job cancelled while still
// Scheduled, whose due time then arrives, is dispatched by asynq's own
// forwarder and processor -- and processTask's marker check skips Handle.
// The wait is the constructive one: asynq's Inspector reports a task
// Completed only after its dispatch cycle concluded, so observing that
// state for the cancelled Job is the evidence that its due window fired
// and the pipeline ran, with the Handle call count as the assertion --
// never a fixed wall-clock sleep.
//
// The control Job makes the suppression a differential rather than a bare
// zero: an identical Job, at the same delay, NOT cancelled, must reach
// Handle exactly once -- proving the dispatch pipeline genuinely runs
// Handle at this due window, in this very test, so the cancelled Job's
// zero cannot be the pipeline's silence. Kill the marker check in
// processTask (dispatchAfterMarkerRead decides the skip) and the
// cancelled Job's counter climbs to exactly the control's -- this test
// fails on the difference.
func TestRedisQueue_Cancel_ScheduledJob_SuppressedAtDueTime(t *testing.T) {
	ctx := context.Background()
	connOpt := startRedisContainer(t, ctx)
	q := asynq.NewQueue(connOpt, testAsynqDefaultOpts...)
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Queue.Start() error = %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := q.Close(closeCtx); err != nil {
			t.Errorf("Queue.Close() error = %v", err)
		}
	})

	var mu sync.Mutex
	handled := make(map[jobs.JobID]int)
	if err := q.RegisterHandler(jobs.NewHandlerFunc("due-probe", func(_ context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
		mu.Lock()
		handled[job.ID]++
		mu.Unlock()
		return jobs.Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const tenant = pkgcore.TenantID("tenant-a")
	tctx := testkit.TenantCtx(tenant)

	// Long enough that the Cancel below lands while the task is still
	// Scheduled (Cancel's own round trips are milliseconds), short enough
	// that the constructive wait below stays fast.
	const delay = 500 * time.Millisecond

	cancelledID, err := q.Enqueue(ctx, jobs.Task{Type: "due-probe", TenantID: tenant}, jobs.WithDelay(delay))
	if err != nil {
		t.Fatalf("Enqueue(cancelled) error = %v", err)
	}
	if err := q.Cancel(tctx, cancelledID); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	// Premise check, before the due time: the marker is live and Get
	// reports the cancellation from it.
	if job, err := q.Get(tctx, cancelledID); err != nil {
		t.Fatalf("Get() after Cancel error = %v", err)
	} else if job.Status != jobs.StatusCancelled {
		t.Fatalf("Status after Cancel = %v, want %v", job.Status, jobs.StatusCancelled)
	}

	controlID, err := q.Enqueue(ctx, jobs.Task{Type: "due-probe", TenantID: tenant}, jobs.WithDelay(delay))
	if err != nil {
		t.Fatalf("Enqueue(control) error = %v", err)
	}

	// Constructive waits: the control reaches Completed only once its due
	// time passed and Handle ran; the cancelled Job reaches Completed only
	// once its own due time passed, it was dequeued, and processTask's
	// marker check skipped Handle -- the skip is what asynq records as this
	// task's completed attempt. Both polls run through asynq's own
	// Inspector on the same Redis (queue "default": neither Enqueue passes
	// a priority).
	insp := asynqlib.NewInspector(connOpt)
	pollInspectorState(t, insp, controlID, asynqlib.TaskStateCompleted)
	pollInspectorState(t, insp, cancelledID, asynqlib.TaskStateCompleted)

	mu.Lock()
	defer mu.Unlock()
	if got := handled[controlID]; got != 1 {
		t.Errorf("control Job's Handle ran %d times at the due window, want exactly 1 -- the dispatch pipeline must genuinely run Handle at the delay under test, or the cancelled Job's zero below proves nothing", got)
	}
	if got := handled[cancelledID]; got != 0 {
		t.Errorf("Handle ran %d times for a Job cancelled while still Scheduled, want 0: Queue.Cancel's marker is what processTask checks before Handle on the task's first dispatch, and the due time arriving must not bypass it", got)
	}
}

// TestRedisQueue_Cancel_RunningJob proves Cancel on a StatusRunning Job
// marks it StatusCancelled immediately -- without waiting for the
// in-flight Handle call to return -- and best-effort signals asynq's own
// Inspector.CancelProcessing: strictly better than the standalone
// implementation's own "does not preempt" limitation (Queue.Cancel's doc
// comment only ever promises a running Job "is allowed to" keep executing,
// never that it is guaranteed to).
func TestRedisQueue_Cancel_RunningJob(t *testing.T) {
	ctx := context.Background()
	q := startTestAsynqQueue(t, ctx)

	started := make(chan struct{})
	release := make(chan struct{})
	h := jobs.NewHandlerFunc("slow", func(ctx context.Context, _ *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			// Queue.Cancel's best-effort CancelProcessing signal may
			// interrupt this ctx before the test releases it -- either
			// path is acceptable; what this test actually asserts is
			// Get()'s reported Status, not how Handle itself exits.
		}
		return jobs.Result{}, ctx.Err()
	})
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const tenant = pkgcore.TenantID("tenant-a")
	id, err := q.Enqueue(context.Background(), jobs.Task{Type: "slow", TenantID: tenant})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("Handle never started")
	}

	tctx := testkit.TenantCtx(tenant)
	if cancelErr := q.Cancel(tctx, id); cancelErr != nil {
		t.Fatalf("Cancel() error = %v", cancelErr)
	}

	job, err := q.Get(tctx, id)
	if err != nil {
		t.Fatalf("Get() immediately after Cancel error = %v", err)
	}
	if job.Status != jobs.StatusCancelled {
		t.Fatalf("Status immediately after Cancel = %v, want %v (Cancel must not wait for the in-flight Handle call)", job.Status, jobs.StatusCancelled)
	}

	close(release)
}
