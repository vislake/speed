//go:build integration

package jobs_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/jobs/queue/asynq"
	"github.com/vislake/speed/go/pkgcore"
)

// This file proves, against a real asynq.Server dequeuing from a real
// Redis, that the cancellation-marker read guarding every attempt FAILS
// CLOSED: when the marker cannot be read, a possibly-cancelled Job must
// not execute (queue/asynq/worker.go's dispatchAfterMarkerRead). The
// unreadable state is manufactured deterministically: the marker key is
// overwritten with a Redis LIST, so every GET against it answers a
// WRONGTYPE error instead of the marker value -- a real read failure on a
// real Redis, not a mock.

// TestRedisQueue_UnreadableCancellationMarker_RefusesToRunUntilReadableAgain
// orchestrates the fail-closed contract end to end:
//
//  1. A Job is enqueued with a delay (so it is Scheduled, not yet
//     dispatched) and Cancelled -- the marker exists, the Handle must
//     never run.
//  2. The marker key is sabotaged into a LIST, so the marker read at
//     dispatch time fails. Pre-fix, processTask logged the failure and ran
//     Handle anyway: a cancelled Job executed. Post-fix, the attempt is
//     refused (errCancelMarkerUnreadable, bounce-class: no retry budget
//     consumed) and the Job stays retryable, Handle never invoked.
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
	// fails and the attempt is refused, bouncing the Job into Retry -- with
	// zero Handle invocations. Pre-fix, the first dispatch ran Handle and
	// the Job succeeded, so this poll times out (red) or the calls
	// assertion below fails.
	pollUntil(t, tenantCtx(tenant), q, id, 10*time.Second, func(j *jobs.Job) bool { return j.Status == jobs.StatusRetrying })
	if got := calls.Load(); got != 0 {
		t.Fatalf("Handle ran %d times while the cancellation marker was unreadable, want 0: a Job whose cancellation state cannot be verified must never execute", got)
	}
	job, err := q.Get(tenantCtx(tenant), id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if job.Attempts != 0 {
		t.Errorf("Attempts = %d, want 0 (the refusals are bounce-class: no retry budget consumed)", job.Attempts)
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
}
