//go:build integration

package jobs_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/jobs/internal/testutil"
	"github.com/vislake/speed/go/jobs/queue/asynq"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/testkit"
)

// This file proves, against a real asynq.Server dequeuing from a real
// Redis, the two asynq.Queue terminal-signal publish points the unit tier
// cannot reach -- the success site (processTaskUncancelled needs a real
// ResultWriter) and Cancel's site (the marker write and the task lookup
// need Redis) -- plus the publish-before-archive ordering the dead-letter
// site's contract documents, at the one place it is observable: a read-back
// made from inside the publish hook. The dead-letter site's branches
// themselves (retryable and cancel-silenced staying silent included) are
// pinned by queue/asynq/terminal_event_publish_test.go's ctx-free tests,
// the same unit/integration division the outcome metrics established
// (job_metrics_test.go's own header).

// terminalEventHandler succeeds from Handle -- the Job whose success
// terminates at the publish point under test.
type terminalEventHandler struct{}

func (*terminalEventHandler) Type() string { return "terminal-ok" }

func (*terminalEventHandler) Handle(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
	return jobs.Result{Data: []byte("done")}, nil
}

// terminalFailingHandler always fails -- the Job whose exhausted retries
// reach the archive-bound dead-letter publish point.
type terminalFailingHandler struct{}

func (*terminalFailingHandler) Type() string { return "terminal-fail" }

func (*terminalFailingHandler) Handle(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
	return jobs.Result{}, errors.New("genuine business failure")
}

// TestRedisQueue_TerminalEvent_SucceededJob pins the success publish point
// end to end: a Job that really succeeded through a real asynq worker
// publishes exactly one jobs.job.terminal event, carrying the task's own
// tenant, the succeeded status, no error and the attempt count.
func TestRedisQueue_TerminalEvent_SucceededJob(t *testing.T) {
	ctx := context.Background()
	bus := testutil.NewRecordingBus()
	q := startTestAsynqQueue(t, ctx, asynq.WithEventBus(bus))
	if err := q.RegisterHandler(&terminalEventHandler{}); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const tenant = pkgcore.TenantID("tenant-a")
	id, err := q.Enqueue(ctx, jobs.Task{Type: "terminal-ok", TenantID: tenant})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	ctxT := testkit.TenantCtx(tenant)
	if job := waitForTerminal(t, ctxT, q, id, 10*time.Second); job.Status != jobs.StatusSucceeded {
		t.Fatalf("Job status = %v, want %v", job.Status, jobs.StatusSucceeded)
	}

	evt := bus.WaitForPublish(t, 10*time.Second, "the succeeded terminal event", func(evt pkgcore.Event) bool {
		payload, ok := evt.Payload.(jobs.JobTerminalEvent)
		return ok && payload.JobID == id
	})
	if evt.Type != jobs.EventJobTerminal {
		t.Errorf("event type = %q, want %q", evt.Type, jobs.EventJobTerminal)
	}
	if evt.TenantID != tenant {
		t.Errorf("event TenantID = %q, want %q", evt.TenantID, tenant)
	}
	payload := evt.Payload.(jobs.JobTerminalEvent)
	if payload.Status != jobs.StatusSucceeded {
		t.Errorf("payload.Status = %q, want %q", payload.Status, jobs.StatusSucceeded)
	}
	if payload.JobType != "terminal-ok" {
		t.Errorf("payload.JobType = %q, want terminal-ok", payload.JobType)
	}
	if payload.Error != "" {
		t.Errorf("payload.Error = %q, want empty for a succeeded Job", payload.Error)
	}
	if payload.Attempts != 1 {
		t.Errorf("payload.Attempts = %d, want 1", payload.Attempts)
	}
	if payload.CompletedAt.IsZero() {
		t.Error("payload.CompletedAt is zero, want the success moment")
	}
}

// TestRedisQueue_TerminalEvent_DeadLetterPublishesBeforeArchive pins both
// halves of the dead-letter point's ordering concession on the real
// pipeline: the event fires with StatusDeadLetter, and a read-back taken
// from inside the publish hook — before asynq's own archive write, which
// strictly follows the ErrorHandler — still observes StatusRunning. That is
// exactly the "must not depend on the row having reached its terminal state"
// clause the consumer contract carries; after the pipeline settles, the row
// has moved to StatusDeadLetter as usual.
func TestRedisQueue_TerminalEvent_DeadLetterPublishesBeforeArchive(t *testing.T) {
	ctx := context.Background()
	bus := testutil.NewRecordingBus()
	q := startTestAsynqQueue(t, ctx, asynq.WithEventBus(bus))
	if err := q.RegisterHandler(&terminalFailingHandler{}); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const tenant = pkgcore.TenantID("tenant-a")
	ctxT := testkit.TenantCtx(tenant)
	var observedAtPublish atomic.Value // jobs.Status, read back from inside the hook.
	bus.SetPublishHook(func(_ context.Context, evt pkgcore.Event) error {
		payload, ok := evt.Payload.(jobs.JobTerminalEvent)
		if !ok || payload.JobType != "terminal-fail" {
			return nil
		}
		job, gerr := q.Get(ctxT, payload.JobID)
		if gerr != nil {
			observedAtPublish.Store(gerr.Error())
			return nil
		}
		observedAtPublish.Store(job.Status)
		return nil
	})
	id, err := q.Enqueue(ctx, jobs.Task{Type: "terminal-fail", TenantID: tenant}, jobs.WithMaxRetries(0))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	if job := waitForTerminal(t, ctxT, q, id, 10*time.Second); job.Status != jobs.StatusDeadLetter {
		t.Fatalf("Job status = %v, want %v", job.Status, jobs.StatusDeadLetter)
	}
	evt := bus.WaitForPublish(t, 10*time.Second, "the dead-letter terminal event", func(evt pkgcore.Event) bool {
		payload, ok := evt.Payload.(jobs.JobTerminalEvent)
		return ok && payload.JobID == id
	})
	payload := evt.Payload.(jobs.JobTerminalEvent)
	if payload.Status != jobs.StatusDeadLetter {
		t.Errorf("payload.Status = %q, want %q", payload.Status, jobs.StatusDeadLetter)
	}
	if payload.Error != "genuine business failure" {
		t.Errorf("payload.Error = %q, want the terminal failure's message", payload.Error)
	}
	observed, ok := observedAtPublish.Load().(jobs.Status)
	if !ok {
		t.Fatalf("read-back inside the publish hook did not observe a Status: %v", observedAtPublish.Load())
	}
	if observed != jobs.StatusRunning {
		t.Errorf("row status observed from inside the publish hook = %q, want %q (the event is published before asynq's own archive write)", observed, jobs.StatusRunning)
	}
}

// TestRedisQueue_TerminalEvent_CancelledScheduledJob pins Cancel's publish
// point: cancelling a not-yet-due Job writes the authoritative marker and
// publishes exactly one cancelled event whose CompletedAt is the marker's
// own timestamp — the same moment Get() reports as the Job's CompletedAt on
// this implementation, which is the cancelled-status pin the payload
// documents.
func TestRedisQueue_TerminalEvent_CancelledScheduledJob(t *testing.T) {
	ctx := context.Background()
	bus := testutil.NewRecordingBus()
	q := startTestAsynqQueue(t, ctx, asynq.WithEventBus(bus))

	const tenant = pkgcore.TenantID("tenant-a")
	id, err := q.Enqueue(ctx, jobs.Task{Type: "terminal-ok", TenantID: tenant},
		jobs.WithScheduledAt(time.Now().Add(time.Hour)))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	ctxT := testkit.TenantCtx(tenant)
	if err := q.Cancel(ctxT, id); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}

	evt := bus.WaitForPublish(t, 10*time.Second, "the cancelled terminal event", func(evt pkgcore.Event) bool {
		payload, ok := evt.Payload.(jobs.JobTerminalEvent)
		return ok && payload.JobID == id
	})
	payload := evt.Payload.(jobs.JobTerminalEvent)
	if payload.Status != jobs.StatusCancelled {
		t.Errorf("payload.Status = %q, want %q", payload.Status, jobs.StatusCancelled)
	}
	if payload.Attempts != 0 {
		t.Errorf("payload.Attempts = %d, want 0 for a Job that never ran", payload.Attempts)
	}
	if payload.CompletedAt.IsZero() {
		t.Error("payload.CompletedAt is zero, want the cancellation moment")
	}

	job, err := q.Get(ctxT, id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if job.Status != jobs.StatusCancelled {
		t.Fatalf("Job status = %v, want %v", job.Status, jobs.StatusCancelled)
	}
	if job.CompletedAt == nil || !payload.CompletedAt.Equal(*job.CompletedAt) {
		t.Errorf("payload.CompletedAt = %v, want the marker timestamp Get reports (%v)", payload.CompletedAt, job.CompletedAt)
	}
	if got := len(bus.Events()); got != 1 {
		t.Errorf("recorded events = %d, want exactly 1 for this cancellation", got)
	}
}
