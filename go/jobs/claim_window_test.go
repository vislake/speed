package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// This file holds the claimed-but-not-started honesty regression: a Job the
// dispatcher claimed but no worker has picked up yet must not report an
// inflated Attempts figure -- an attempt is only counted when a worker
// actually starts it (the handoff, worker.go's runAttempt), never when the
// dispatcher claims the row. Named for the behaviour it verifies, since it
// spans store.go's claimOne/markAttemptStarted and worker.go's
// dispatch/runWorker.

// TestStandaloneQueue_Get_ClaimedButNotStarted_ReportsNoInflatedAttempts is
// the deterministic end-to-end regression: a single worker is blocked
// inside a long Handle, the dispatcher claims a second Job of the same
// tenant into the (unbuffered) dispatch channel where it must wait for the
// worker -- the exact claimed-but-not-started window. Get() during that
// window must honestly report the Job as claimed (StatusRunning) but not
// started: Attempts still 0 (no Handle has been invoked for it) and
// StartedAt still nil. Pre-fix, claimOne counted the attempt and stamped
// StartedAt at claim time, so Get() reported Running with Attempts 1 for a
// Handle call that had not begun. After the worker picks the Job up and
// runs it, the honest figures catch up: final Attempts 1 with StartedAt
// set. Deterministic without wall-clock sleeps: the worker's occupancy of
// the second Job is guaranteed by the first Job's blocked Handle.
func TestStandaloneQueue_Get_ClaimedButNotStarted_ReportsNoInflatedAttempts(t *testing.T) {
	q := newTestQueue(t, WithWorkerCount(1))
	entered := make(chan JobID, 1)
	release := make(chan struct{})
	blocker := NewHandlerFunc("claim-window.block", func(_ context.Context, job *Job, _ ProgressFn) (Result, error) {
		entered <- job.ID
		<-release
		return Result{}, nil
	})
	runner := NewHandlerFunc("claim-window.fast", func(context.Context, *Job, ProgressFn) (Result, error) {
		return Result{Data: []byte("ok")}, nil
	})
	if err := q.RegisterHandler(blocker); err != nil {
		t.Fatalf("RegisterHandler(blocker) error = %v", err)
	}
	if err := q.RegisterHandler(runner); err != nil {
		t.Fatalf("RegisterHandler(runner) error = %v", err)
	}
	startQueue(t, q)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	slowID, err := q.Enqueue(context.Background(), Task{Type: "claim-window.block", TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("Enqueue(blocker) error = %v", err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the single worker to enter the blocker's Handle")
	}

	// The second Job: the dispatcher claims it (StatusRunning in the
	// database) and blocks handing it to the worker, which is still inside
	// the blocker's Handle -- the claimed-but-not-started window.
	fastID, err := q.Enqueue(context.Background(), Task{Type: "claim-window.fast", TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("Enqueue(runner) error = %v", err)
	}
	pollJob(t, q, ctx, fastID, 3*time.Second, func(j *Job) bool { return j.Status == StatusRunning })

	job, err := q.Get(ctx, fastID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if job.Attempts != 0 {
		t.Errorf("claimed-but-not-started Attempts = %d, want 0 (no Handle call has started; attempts are counted at the worker handoff, not at the claim)", job.Attempts)
	}
	if job.StartedAt != nil {
		t.Errorf("claimed-but-not-started StartedAt = %v, want nil (the attempt has not started)", job.StartedAt)
	}

	close(release)
	slowJob := waitTerminal(t, q, ctx, slowID)
	if slowJob.Status != StatusSucceeded {
		t.Fatalf("blocker job Status = %v, want %v", slowJob.Status, StatusSucceeded)
	}
	job = waitTerminal(t, q, ctx, fastID)
	if job.Status != StatusSucceeded {
		t.Fatalf("runner job Status = %v, want %v", job.Status, StatusSucceeded)
	}
	if job.Attempts != 1 {
		t.Errorf("runner job final Attempts = %d, want 1 (exactly one Handle call ran, counted at its handoff)", job.Attempts)
	}
	if job.StartedAt == nil {
		t.Error("runner job final StartedAt = nil, want set once the worker actually started the attempt")
	}
}
