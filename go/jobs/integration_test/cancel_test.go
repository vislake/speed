//go:build integration

package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// TestRedisQueue_Cancel_PendingJobNeverRuns proves Cancel on a not-yet-
// dispatched Job both marks it StatusCancelled (Get()'s authoritative,
// guaranteed effect -- AGENTS.md's Cancel section) and actually removes it
// from asynq's own pending set (Inspector.DeleteTask), so it is never
// handed to Handle at all -- the production-profile counterpart of
// demo_queue_test.go's TestCancel_TenantIsolation_And_Idempotency.
func TestRedisQueue_Cancel_PendingJobNeverRuns(t *testing.T) {
	ctx := context.Background()
	q := startTestAsynqQueue(t, ctx)

	handleCalled := make(chan struct{}, 1)
	h := jobs.NewHandlerFunc("never-should-run", func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
		handleCalled <- struct{}{}
		return jobs.Result{}, nil
	})
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const tenant = pkgcore.TenantID("tenant-a")
	id, err := q.Enqueue(context.Background(), jobs.Task{Type: "never-should-run", TenantID: tenant}, jobs.WithDelay(2*time.Second))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	tctx := tenantCtx(tenant)
	if cancelErr := q.Cancel(tctx, id); cancelErr != nil {
		t.Fatalf("Cancel() error = %v", cancelErr)
	}

	job, err := q.Get(tctx, id)
	if err != nil {
		t.Fatalf("Get() after Cancel error = %v", err)
	}
	if job.Status != jobs.StatusCancelled {
		t.Fatalf("Status = %v, want %v", job.Status, jobs.StatusCancelled)
	}

	// Cancel is idempotent.
	if secondCancelErr := q.Cancel(tctx, id); secondCancelErr != nil {
		t.Errorf("second Cancel() on an already-cancelled job error = %v, want nil", secondCancelErr)
	}

	select {
	case <-handleCalled:
		t.Fatal("Handle ran for a Job cancelled before its delay elapsed")
	case <-time.After(3 * time.Second):
		// Outlives the 2s delay: if the cancellation hadn't removed the
		// task from asynq's own scheduled set, Handle would have run by
		// now.
	}
}

// TestRedisQueue_Cancel_RunningJob proves Cancel on a StatusRunning Job
// marks it StatusCancelled immediately -- without waiting for the
// in-flight Handle call to return -- and best-effort signals asynq's own
// Inspector.CancelProcessing, matching AGENTS.md's documented
// strictly-better-than-DemoQueue's-own-"does not preempt" limitation
// (Queue.Cancel's doc comment only ever promises a running Job "is allowed
// to" keep executing, never that it is guaranteed to).
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
			// AsynqQueue.Cancel's best-effort CancelProcessing signal may
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

	tctx := tenantCtx(tenant)
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
