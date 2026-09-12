//go:build integration

package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/testkit"
)

// Cancel coverage splits two ways. The portable semantics -- the
// tenant-scoped access rule, idempotency, and the cancelled status Get()
// reports off the intact task record -- are proven for both
// implementations by the shared conformance suite's
// "cancel_tenant_isolation_and_idempotency" subtest (driven here by
// TestAsynqQueue_ConformsToQueueContract, queue_conformance_test.go); the
// dispatch-suppression decision -- a readable cancellation marker skips
// Handle -- is pinned at the unit tier by queue/asynq's
// TestQueue_DispatchAfterMarkerRead_FailsClosedOnUnreadableMarker. What
// stays asynq-specific and integration-tier is the running-Job behavior
// below.

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
