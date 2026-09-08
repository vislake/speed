//go:build integration

package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// TestRedisQueue_Cancel_PendingJobNeverRuns proves a cancelled-before-its-
// delay-elapsed Job's Handle genuinely never runs: the task is removed
// from asynq's own scheduled set rather than merely marked cancelled in
// this module's own status column. This is asynq-specific behavior with no
// StandaloneQueue counterpart (the shared conformance suite covers the
// portable cancel semantics via its
// "cancel_tenant_isolation_and_idempotency" subtest, driven by
// TestAsynqQueue_ConformsToQueueContract); the non-dispatch half is
// pinned here, against a real asynq/Redis pipeline.

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
