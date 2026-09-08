//go:build integration

package jobs_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// This file proves, against a real asynq.Server dequeuing from a real
// Redis, that a panicking FailureHook cannot crash the worker process:
// the recovery queue/asynq/worker.go's invokeOnFailure adds around the
// OnFailure call keeps the panic inside the failed Job's own
// failure-processing path, so the process survives, the dead-letter still
// archives, and a later Job still executes. The containment is not
// something asynq's own library provides -- asynq's processor.perform
// recovers panics from the handler call only, while its ErrorHandler
// (this package's handleError, which invokes OnFailure) runs strictly
// outside that recover in handleFailedMessage on the worker goroutine
// (pinned v0.26.0 source) -- so without a recovery a panicking hook kills
// the whole process mid-suite. That failure mode cannot be demonstrated
// in-process as a clean test failure (the process dies), so the
// deterministic fail-before proof lives in the unit tier
// (queue/asynq/worker_test.go's panicking-FailureHook subtest of
// TestQueue_HandleErrorAttempt, where the escaping panic fails the test);
// this leg is the pass-after real-run proof that the recovery holds end to
// end over a real worker.
func TestRedisQueue_PanickingFailureHook_WorkerSurvivesAndLaterJobsRun(t *testing.T) {
	ctx := context.Background()
	q := startTestAsynqQueue(t, ctx)

	// The panicking hook: the first Job of this type fails permanently
	// (MaxRetries 0) and its terminal-attempt OnFailure panics.
	panicking := &integrationPanickingFailureHook{}
	if err := q.RegisterHandler(panicking); err != nil {
		t.Fatalf("RegisterHandler(panicking) error = %v", err)
	}
	// A second, healthy handler proves the process is still alive and
	// dequeuing after the panic.
	healthy := &integrationCountingHandler{}
	if err := q.RegisterHandler(healthy); err != nil {
		t.Fatalf("RegisterHandler(healthy) error = %v", err)
	}

	tenant := pkgcore.TenantID("tenant-a")
	panicID, err := q.Enqueue(ctx, jobs.Task{
		Type:     panicking.Type(),
		TenantID: tenant,
	}, jobs.WithMaxRetries(0))
	if err != nil {
		t.Fatalf("Enqueue(panicking) error = %v", err)
	}

	// The terminal failure fires OnFailure (which panics, and is recovered),
	// and asynq archives the task. waitForTerminal answers StatusDeadLetter
	// only if the whole chain survived the panic -- an unrecovered panic
	// would have killed the process (and the test binary) before the archive
	// write ever landed.
	panicCtx := pkgcore.WithTenant(ctx, tenant)
	job := waitForTerminal(t, panicCtx, q, panicID, 30*time.Second)
	if job.Status != jobs.StatusDeadLetter {
		t.Fatalf("panicking job status = %v, want StatusDeadLetter (the panic must not prevent the archive)", job.Status)
	}
	if panicking.calls.Load() != 1 {
		t.Errorf("panicking hook OnFailure calls = %d, want 1", panicking.calls.Load())
	}

	// The process survived: a second, healthy Job of another type still
	// dequeues and completes on the same queue.
	healthyID, err := q.Enqueue(ctx, jobs.Task{
		Type:     healthy.Type(),
		TenantID: tenant,
	}, jobs.WithMaxRetries(0))
	if err != nil {
		t.Fatalf("Enqueue(healthy) error = %v", err)
	}
	after := waitForTerminal(t, panicCtx, q, healthyID, 30*time.Second)
	if after.Status != jobs.StatusSucceeded {
		t.Fatalf("healthy job after the recovered panic = %v, want StatusSucceeded -- the worker must survive a panicking failure hook", after.Status)
	}
	if healthy.calls.Load() != 1 {
		t.Errorf("healthy handler calls = %d, want 1", healthy.calls.Load())
	}
}

// integrationPanickingFailureHook always fails Handle and panics inside
// OnFailure -- the hook-author bug invokeOnFailure contains.
type integrationPanickingFailureHook struct {
	calls atomic.Int32
}

func (*integrationPanickingFailureHook) Type() string { return "panics-on-failure" }

func (*integrationPanickingFailureHook) Handle(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
	return jobs.Result{}, errors.New("permanent failure")
}

func (h *integrationPanickingFailureHook) OnFailure(context.Context, *jobs.Job, error) {
	h.calls.Add(1)
	panic("on-failure hook panicked")
}

// integrationCountingHandler succeeds on every Handle and counts the
// calls.
type integrationCountingHandler struct {
	calls atomic.Int32
}

func (*integrationCountingHandler) Type() string { return "counts-runs" }

func (h *integrationCountingHandler) Handle(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
	h.calls.Add(1)
	return jobs.Result{}, nil
}

var (
	_ jobs.Handler     = (*integrationPanickingFailureHook)(nil)
	_ jobs.FailureHook = (*integrationPanickingFailureHook)(nil)
	_ jobs.Handler     = (*integrationCountingHandler)(nil)
)
