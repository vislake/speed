//go:build integration

package jobs_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	asynqlib "github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/jobs/queue/asynq"
	"github.com/vislake/speed/go/pkgcore"
)

// This file proves, against a real asynq.Server dequeuing from a real
// Redis, that the cancellation-marker read fails CLOSED on both sides of
// the marker's two consumers: a possibly-cancelled Job must not EXECUTE
// when the marker cannot be read (queue/asynq/worker.go's
// dispatchAfterMarkerRead), and must never be REPORTED as its natural
// asynq state either (queue/asynq/queue.go's Get and DeadLetterJobs -- a
// skipped-run Cancelled Job's underlying Completed record would otherwise
// read as StatusSucceeded with an empty Result). The unreadable state is
// manufactured deterministically: the marker key is overwritten with a
// Redis LIST, so every GET against it answers a WRONGTYPE error instead
// of the marker value -- a real read failure on a real Redis, not a mock.
// Where an observation must see the underlying asynq state rather than
// the queue's own Get -- which fails closed during the outage, and whose
// cancellation overlay hides the underlying state while the marker reads
// fine -- the tests probe asynq's own public Inspector on the same Redis
// instead.

// TestRedisQueue_UnreadableCancellationMarker_RefusesToRunUntilReadableAgain
// orchestrates the fail-closed contract end to end:
//
//  1. A Job is enqueued with a delay (so it is Scheduled, not yet
//     dispatched) and Cancelled -- the marker exists, the Handle must
//     never run.
//  2. The marker key is sabotaged into a LIST, so the marker read at
//     dispatch time fails. The attempt must be refused
//     (errCancelMarkerUnreadable, bounce-class: no retry budget consumed)
//     and the Job stays retryable, Handle never invoked -- a dispatch that
//     logged the failure and ran Handle anyway would execute a cancelled
//     Job.
//  3. The sabotage is removed: the next refusal cycle reads a clean "no
//     marker" answer and the Job runs normally -- proving the refusal was
//     the transient marker outage's, not a wedged Job.
func TestRedisQueue_UnreadableCancellationMarker_RefusesToRunUntilReadableAgain(t *testing.T) {
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

	var calls atomic.Int32
	if err := q.RegisterHandler(jobs.NewHandlerFunc("marker-probe", func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
		calls.Add(1)
		return jobs.Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	// A raw client on the SAME Redis instance, for the sabotage: the queue's
	// own rdb is private (and must stay so).
	raw, ok := connOpt.MakeRedisClient().(redis.UniversalClient)
	if !ok {
		t.Fatalf("MakeRedisClient() = %T, want a redis.UniversalClient", connOpt.MakeRedisClient())
	}
	defer raw.Close()

	const tenant = pkgcore.TenantID("tenant-a")
	id, err := q.Enqueue(ctx, jobs.Task{Type: "marker-probe", TenantID: tenant}, jobs.WithDelay(800*time.Millisecond))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if err := q.Cancel(tenantCtx(tenant), id); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}

	// Sabotage: replace the marker (a string) with a LIST, so the marker
	// read answers WRONGTYPE instead of the marker. The key derivation is
	// queue/asynq/queue.go's cancelMarkerKey, reproduced here verbatim; if
	// it ever drifts, the sabotage stops failing the read and the
	// poll-until-Retrying below times out -- a loud failure, not a silent
	// false pass.
	markerKey := "asynqjobs:cancelled:" + string(id)
	if err := raw.Del(ctx, markerKey).Err(); err != nil {
		t.Fatalf("sabotage (del marker): %v", err)
	}
	if err := raw.RPush(ctx, markerKey, "sabotage").Err(); err != nil {
		t.Fatalf("sabotage (list marker): %v", err)
	}

	// The Job becomes dispatchable at +800ms; every dispatch's marker read
	// fails and the attempt is refused, bouncing the Job into asynq's Retry
	// state -- with zero Handle invocations. A dispatch path that ran
	// Handle despite the unreadable marker would let the Job succeed, so
	// this poll would time out (red) or the calls assertion below would
	// fail. The bounce is observed through asynq's own Inspector, not the
	// queue's Get: Get fails closed while the marker is unreadable (the
	// reporting-side rule the two tests below pin), so it can no longer be
	// the mid-outage probe -- which the very next assertion checks.
	insp := asynqlib.NewInspector(connOpt)
	pollInspectorState(t, insp, id, asynqlib.TaskStateRetry)
	if got := calls.Load(); got != 0 {
		t.Fatalf("Handle ran %d times while the cancellation marker was unreadable, want 0: a Job whose cancellation state cannot be verified must never execute", got)
	}
	if _, err := q.Get(tenantCtx(tenant), id); err == nil {
		t.Fatal("Get() succeeded while the cancellation marker was unreadable, want an error: a Job whose cancellation state cannot be verified must never be reported as its natural state (here StatusRetrying)")
	}

	// Repair: remove the sabotage. The next refusal cycle reads a clean
	// "no marker" answer and runs the Job normally -- the refusal was the
	// outage's, and the Job recovered by itself.
	if err := raw.Del(ctx, markerKey).Err(); err != nil {
		t.Fatalf("repair (del sabotage): %v", err)
	}
	terminal := waitForTerminal(t, tenantCtx(tenant), q, id, 10*time.Second)
	if terminal.Status != jobs.StatusSucceeded {
		t.Fatalf("Status = %v, want %v once the marker read works again", terminal.Status, jobs.StatusSucceeded)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("Handle ran %d times, want exactly 1 (once, after the sabotage was removed)", got)
	}
	// The refusals consumed no retry budget: a genuine failure would have
	// burned a retry per attempt, so the single successful run reports
	// Attempts 1, never more.
	if terminal.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (the refusals were bounce-class: no retry budget consumed)", terminal.Attempts)
	}
}

// TestRedisQueue_Get_UnreadableCancellationMarker_NeverReportsSucceeded is
// the reporting-side regression the dispatch refusal above protects: a Job
// that was Cancelled and whose run the worker then skipped (asynq records
// the skip as an ordinary Completed, retained like a real success) must
// never be REPORTED as StatusSucceeded when the cancellation marker can no
// longer be read: Get must fail closed rather than report the Job's
// natural asynq state -- StatusSucceeded with an empty Result is the exact
// answer the poll-then-unmarshal consumer pattern (ai-gateway's
// image_gateway.go) treats as a successful completion and then fails on as
// "unexpected end of JSON input" when it decodes the empty Result.
func TestRedisQueue_Get_UnreadableCancellationMarker_NeverReportsSucceeded(t *testing.T) {
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

	var calls atomic.Int32
	if err := q.RegisterHandler(jobs.NewHandlerFunc("report-probe", func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
		calls.Add(1)
		return jobs.Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	// A raw client on the SAME Redis instance, for the sabotage: the queue's
	// own rdb is private (and must stay so).
	raw, ok := connOpt.MakeRedisClient().(redis.UniversalClient)
	if !ok {
		t.Fatalf("MakeRedisClient() = %T, want a redis.UniversalClient", connOpt.MakeRedisClient())
	}
	defer raw.Close()

	const tenant = pkgcore.TenantID("tenant-a")
	id, err := q.Enqueue(ctx, jobs.Task{Type: "report-probe", TenantID: tenant}, jobs.WithDelay(400*time.Millisecond))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if err := q.Cancel(tenantCtx(tenant), id); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}

	// Wait for the worker to dequeue the cancelled Job and skip Handle,
	// which asynq records as an ordinary Completed -- the underlying state
	// the reporting overlay must keep visible. Observed through asynq's own
	// Inspector because Get's overlay reports StatusCancelled from the
	// moment Cancel returned, hiding the underlying state; the Get right
	// after confirms the overlay is live over that Completed record (calls
	// still 0: the skip is what Completed the task, never a Handle run).
	insp := asynqlib.NewInspector(connOpt)
	pollInspectorState(t, insp, id, asynqlib.TaskStateCompleted)
	if got := calls.Load(); got != 0 {
		t.Fatalf("Handle ran %d times for a Cancelled Job, want 0 (the Completed record is the skip's)", got)
	}
	if job, err := q.Get(tenantCtx(tenant), id); err != nil {
		t.Fatalf("Get() error = %v", err)
	} else if job.Status != jobs.StatusCancelled {
		t.Fatalf("Status = %v, want %v (the cancellation overlay over the skipped run's Completed record)", job.Status, jobs.StatusCancelled)
	}

	// Sabotage: replace the marker (a string) with a LIST, so Get's marker
	// read answers WRONGTYPE instead of the marker. The key derivation is
	// queue/asynq/queue.go's cancelMarkerKey, reproduced verbatim here as
	// in the dispatch test above.
	markerKey := "asynqjobs:cancelled:" + string(id)
	if err := raw.Del(ctx, markerKey).Err(); err != nil {
		t.Fatalf("sabotage (del marker): %v", err)
	}
	if err := raw.RPush(ctx, markerKey, "sabotage").Err(); err != nil {
		t.Fatalf("sabotage (list marker): %v", err)
	}

	// The regression: Get must fail closed on the unreadable marker --
	// reporting the underlying Completed record's natural state
	// (StatusSucceeded with an empty Result) after only logging the read
	// failure would serve a caller a success that never happened; the
	// error is returned instead, for the caller to retry.
	if job, err := q.Get(tenantCtx(tenant), id); err == nil {
		t.Fatalf("Get() = Status %v with an empty Result after a marker-read failure, want an error: a possibly-cancelled Job must never be reported as succeeded (job: %+v)", job.Status, job)
	}

	// Repair: remove the sabotage and restore the marker record the
	// sabotage itself destroyed (a Redis value's type cannot be swapped in
	// place). Get works again and reports StatusCancelled from the restored
	// marker -- the outage delayed the report, it did not lose the
	// cancellation.
	if err := raw.Del(ctx, markerKey).Err(); err != nil {
		t.Fatalf("repair (del sabotage): %v", err)
	}
	if err := raw.Set(ctx, markerKey, time.Now().UTC().Format(time.RFC3339Nano), 0).Err(); err != nil {
		t.Fatalf("repair (restore marker): %v", err)
	}
	job, err := q.Get(tenantCtx(tenant), id)
	if err != nil {
		t.Fatalf("Get() error after repair = %v, want nil", err)
	}
	if job.Status != jobs.StatusCancelled {
		t.Fatalf("Status after repair = %v, want %v (the restored marker's overlay)", job.Status, jobs.StatusCancelled)
	}
}

// TestRedisQueue_DeadLetterJobs_UnreadableCancellationMarker_FailsClosed
// pins the same fail-closed rule on DeadLetterJobs, the marker's other
// reporting consumer: an archived Job can genuinely carry a cancellation
// marker -- a Cancel that lands while its terminal attempt is executing
// (cancel_failure_hook_test.go's scenario) leaves the marker intact while
// asynq's own dispatch loop archives the task regardless, and
// DeadLetterJobs' overlay would then report StatusCancelled -- so a
// listing whose marker read fails must error rather than report such a
// Job as its natural StatusDeadLetter. Swallowing the read failure --
// without even the Warn Get emits -- would fail this test.
func TestRedisQueue_DeadLetterJobs_UnreadableCancellationMarker_FailsClosed(t *testing.T) {
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

	if err := q.RegisterHandler(jobs.NewHandlerFunc("deadletter-probe", func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
		return jobs.Result{}, errors.New("permanent failure")
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	// A raw client on the SAME Redis instance, for the sabotage: the queue's
	// own rdb is private (and must stay so).
	raw, ok := connOpt.MakeRedisClient().(redis.UniversalClient)
	if !ok {
		t.Fatalf("MakeRedisClient() = %T, want a redis.UniversalClient", connOpt.MakeRedisClient())
	}
	defer raw.Close()

	const tenant = pkgcore.TenantID("tenant-a")
	id, err := q.Enqueue(ctx, jobs.Task{Type: "deadletter-probe", TenantID: tenant}, jobs.WithMaxRetries(0))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if job := waitForTerminal(t, tenantCtx(tenant), q, id, 10*time.Second); job.Status != jobs.StatusDeadLetter {
		t.Fatalf("Status = %v, want %v", job.Status, jobs.StatusDeadLetter)
	}

	// Sabotage the Job's marker key (no Cancel ever wrote a marker here --
	// the read itself is what must fail closed, whichever way the marker
	// would have answered): replace it with a LIST so the marker read
	// answers WRONGTYPE, key derivation verbatim as above.
	markerKey := "asynqjobs:cancelled:" + string(id)
	if err := raw.Del(ctx, markerKey).Err(); err != nil {
		t.Fatalf("sabotage (del marker): %v", err)
	}
	if err := raw.RPush(ctx, markerKey, "sabotage").Err(); err != nil {
		t.Fatalf("sabotage (list marker): %v", err)
	}

	// The regression: the whole listing fails closed on the unreadable
	// marker -- reporting the archived Job as its natural StatusDeadLetter
	// would fail this test.
	if got, err := q.DeadLetterJobs(tenantCtx(tenant)); err == nil {
		t.Fatalf("DeadLetterJobs() returned %d job(s) after a marker-read failure, want an error: an archived Job a concurrent Cancel may have settled as StatusCancelled must never be reported as StatusDeadLetter while its cancellation state cannot be read", len(got))
	}

	// Repair: remove the sabotage. The listing works again and still shows
	// the Job.
	if err := raw.Del(ctx, markerKey).Err(); err != nil {
		t.Fatalf("repair (del sabotage): %v", err)
	}
	got, err := q.DeadLetterJobs(tenantCtx(tenant))
	if err != nil {
		t.Fatalf("DeadLetterJobs() error after repair = %v, want nil", err)
	}
	found := false
	for _, j := range got {
		if j.ID == id {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("DeadLetterJobs() after repair does not list job %q (listed %d jobs)", id, len(got))
	}
}

// pollInspectorState polls asynq's own Inspector until id's task sits in
// wantState or the deadline passes, failing the test on timeout. It is the
// mid-outage probe in this file wherever Get cannot be: Get fails closed
// while a marker read fails, and its cancellation overlay reports
// StatusCancelled from the marker alone while the read works -- hiding the
// underlying asynq state (the skip that Completed the task, or the bounce
// that Retried it) either way.
func pollInspectorState(t *testing.T, insp *asynqlib.Inspector, id jobs.JobID, wantState asynqlib.TaskState) {
	t.Helper()
	// Every Enqueue in this file passes no WithPriority, so the task lands
	// in the "default" priority queue -- store.go's queueDefault, which is
	// asynq's own default queue name, reproduced verbatim exactly like the
	// sabotage's marker key above; if it ever drifts, this poll times out
	// loudly rather than passing silently.
	deadline := time.Now().Add(10 * time.Second)
	for {
		info, err := insp.GetTaskInfo("default", string(id))
		if err == nil && info.State == wantState {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after 10s waiting for job %q to reach asynq state %v; last error = %v", id, wantState, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
