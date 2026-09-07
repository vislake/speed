//go:build integration

package jobs_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	asynqlib "github.com/hibiken/asynq"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/jobs/queue/asynq"
	"github.com/vislake/speed/go/pkgcore"
)

// This file proves, against a real asynq.Server dequeuing from a real
// Redis, the archive-compensation rule for tenant-concurrency bounces
// landing on a Job's FINAL allowed attempt: asynq's own dispatch loop
// archives a terminal attempt whatever error it returned (processor.go's
// handleFailedMessage archives whenever retried >= maxRetry, isFailure not
// consulted), so the bounce must not dead-letter the Job with zero
// compensation -- the FailureHook fires, exactly as for a genuine terminal
// failure (see queue/asynq/worker.go's handleErrorAttempt doc comment and
// AGENTS.md's "Per-tenant concurrency limiting" section).

// capacityHolderHandler occupies a tenant's single concurrency slot for as
// long as the test needs, blocking inside Handle.
type capacityHolderHandler struct {
	entered chan struct{}
	release chan struct{}
}

func (*capacityHolderHandler) Type() string { return "capacity-holder" }

func (h *capacityHolderHandler) Handle(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
	h.entered <- struct{}{}
	<-h.release
	return jobs.Result{}, nil
}

// terminalBounceFailer always fails Handle with a genuine business error
// and records every OnFailure call it receives -- the Job whose terminal
// attempt will bounce.
type terminalBounceFailer struct {
	calls atomic.Int32
	hook  chan *jobs.Job
}

func (*terminalBounceFailer) Type() string { return "terminal-bounce" }

func (h *terminalBounceFailer) Handle(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
	h.calls.Add(1)
	return jobs.Result{}, errors.New("genuine business failure")
}

func (h *terminalBounceFailer) OnFailure(_ context.Context, job *jobs.Job, _ error) {
	h.hook <- job
}

var (
	_ jobs.Handler     = (*capacityHolderHandler)(nil)
	_ jobs.Handler     = (*terminalBounceFailer)(nil)
	_ jobs.FailureHook = (*terminalBounceFailer)(nil)
)

// TestRedisQueue_TerminalAttemptTenantBounce_ArchivesAndFiresFailureHook is
// the P0-5 regression, orchestrated deterministically on a real asynq
// server:
//
//  1. The always-failing Job B (MaxRetries=1, so MaxRetry=1) runs its FIRST
//     attempt while no other tenant-a Job is running: a genuine business
//     failure, which consumes the one retry (Retried becomes 1) and
//     schedules attempt 2.
//  2. The holder Job A (same tenant, tenant-concurrency limit 1) then
//     occupies the tenant's single slot, blocking inside Handle -- proven
//     via its entered channel before anything else happens.
//  3. B's second (FINAL, retried == maxRetry) attempt dequeues while A
//     still holds the slot: processTask bounces it (errTenantAtCapacity),
//     and asynq's own dispatch loop archives the task right after this
//     package's ErrorHandler returns.
//
// The money assertion: that archive must fire B's FailureHook.OnFailure
// exactly once (pre-fix it archived with zero compensation and zero log),
// and B's archived record must report the terminal state honestly.
// B's genuine retry is stretched to one second so step 3 cannot race step
// 2 -- A is provably holding the slot long before B's final attempt
// arrives.
func TestRedisQueue_TerminalAttemptTenantBounce_ArchivesAndFiresFailureHook(t *testing.T) {
	ctx := context.Background()
	const tenant = pkgcore.TenantID("tenant-a")

	q := startTestAsynqQueue(t, ctx,
		asynq.WithConcurrency(4),
		asynq.WithTenantConcurrencyLimit(1),
		asynq.WithRetryDelayFunc(func(n int, err error, task *asynqlib.Task) time.Duration {
			return time.Second
		}),
	)

	holder := &capacityHolderHandler{entered: make(chan struct{}, 1), release: make(chan struct{})}
	if err := q.RegisterHandler(holder); err != nil {
		t.Fatalf("RegisterHandler(holder) error = %v", err)
	}
	failer := &terminalBounceFailer{hook: make(chan *jobs.Job, 1)}
	if err := q.RegisterHandler(failer); err != nil {
		t.Fatalf("RegisterHandler(failer) error = %v", err)
	}
	ctxT := tenantCtx(tenant)

	// Step 1: B's first attempt fails genuinely -- wait until the Job is
	// Retrying to prove Retried was consumed (retried == maxRetry == 1)
	// before A ever starts.
	bID, err := q.Enqueue(ctx, jobs.Task{Type: "terminal-bounce", TenantID: tenant}, jobs.WithMaxRetries(1))
	if err != nil {
		t.Fatalf("Enqueue(B) error = %v", err)
	}
	bJob := pollUntil(t, ctxT, q, bID, 10*time.Second, func(j *jobs.Job) bool { return j.Status == jobs.StatusRetrying })
	if bJob.Attempts != 1 {
		t.Fatalf("B Attempts after its first genuine failure = %d, want 1 (MaxRetries=1: one retry consumed)", bJob.Attempts)
	}

	// Step 2: A occupies the tenant's single slot, blocked in Handle.
	aID, err := q.Enqueue(ctx, jobs.Task{Type: "capacity-holder", TenantID: tenant})
	if err != nil {
		t.Fatalf("Enqueue(A) error = %v", err)
	}
	select {
	case <-holder.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for A to enter Handle and hold the tenant slot")
	}

	// Step 3: B's final attempt bounces (tenant at capacity) around one
	// second after its first failure; asynq archives it. The hook must fire
	// exactly once.
	select {
	case hookJob := <-failer.hook:
		if hookJob.ID != bID {
			t.Errorf("OnFailure job.ID = %q, want %q", hookJob.ID, bID)
		}
		if hookJob.Status != jobs.StatusDeadLetter {
			t.Errorf("OnFailure job.Status = %v, want %v", hookJob.Status, jobs.StatusDeadLetter)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("FailureHook.OnFailure never fired: the terminal-attempt tenant bounce archived B with zero compensation")
	}

	terminal := waitForTerminal(t, ctxT, q, bID, 10*time.Second)
	if terminal.Status != jobs.StatusDeadLetter {
		t.Fatalf("B Status = %v, want %v (the terminal bounce archives -- and that archive is exactly why OnFailure had to fire)", terminal.Status, jobs.StatusDeadLetter)
	}
	if terminal.Attempts != 2 {
		t.Errorf("B Attempts = %d, want 2 (one genuine failure + the archived terminal attempt)", terminal.Attempts)
	}
	if got := failer.calls.Load(); got != 1 {
		t.Errorf("B Handle ran %d times, want exactly 1 (only the first attempt ran; the terminal attempt bounced before Handle)", got)
	}

	// Release A and let it finish so the queue closes cleanly.
	close(holder.release)
	if job := waitForTerminal(t, ctxT, q, aID, 10*time.Second); job.Status != jobs.StatusSucceeded {
		t.Fatalf("A Status = %v, want %v", job.Status, jobs.StatusSucceeded)
	}
}
