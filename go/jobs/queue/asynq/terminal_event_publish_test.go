package asynq

import (
	"context"
	"errors"
	"testing"
	"time"

	asynqlib "github.com/hibiken/asynq"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/jobs/internal/testutil"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// This file holds the unit-tier proof of this Queue's terminal-signal
// publish points that live on the ctx-free/Redis-free side of the split:
// handleErrorAttempt's archive-bound dead-letter site, its cancel-silenced
// and retryable branches (which must publish nothing), and publishTerminal's
// no-bus no-op. The other two publish points cannot be reached without a
// real backend and are proven by the module's integration tier instead, the
// same division the outcome metrics established: the success site needs a
// real ResultWriter (processTaskUncancelled's early exits all precede the
// publish), and Cancel's site needs a real Redis for the marker write and
// the task lookup (integration_test/terminal_event_test.go). Named for the
// behaviour it verifies, per the backend coding standard's test-naming rule.

// terminalEventsFor returns the recorded terminal-event payloads for jobID,
// in publish order.
func terminalEventsFor(bus *testutil.RecordingBus, id string) []jobs.JobTerminalEvent {
	var out []jobs.JobTerminalEvent
	for _, evt := range bus.Events() {
		if payload, ok := evt.Payload.(jobs.JobTerminalEvent); ok && string(payload.JobID) == id {
			out = append(out, payload)
		}
	}
	return out
}

// TestQueue_HandleErrorAttempt_DeadLetterPublishesTerminalEvent pins the
// dead-letter publish point: a genuine terminal failure's archive-bound
// error hook publishes exactly one jobs.job.terminal event, carrying the
// row's tenant header, the terminal status, the failure message, the
// attempt count the retry arithmetic implies and a non-zero completion
// moment.
func TestQueue_HandleErrorAttempt_DeadLetterPublishesTerminalEvent(t *testing.T) {
	bus := testutil.NewRecordingBus()
	q := newTestQueue(t)
	q.bus = bus
	if err := q.RegisterHandler(jobs.NewHandlerFunc("flaky.dead", func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
		return jobs.Result{}, errors.New("unused")
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	task := asynqlib.NewTaskWithHeaders("flaky.dead", nil, map[string]string{headerTenantID: "tenant-a"})

	q.handleErrorAttempt(context.Background(), task, wrapFailedAttempt(errors.New("permanent failure"), time.Second),
		3 /* retried */, 3 /* maxRetry */, "job-dead", nil, obs.FromContext(context.Background()))

	events := terminalEventsFor(bus, "job-dead")
	if len(events) != 1 {
		t.Fatalf("recorded terminal events = %d, want exactly 1: %+v", len(events), bus.Events())
	}
	evt := bus.Events()[0]
	if evt.Type != jobs.EventJobTerminal {
		t.Errorf("event type = %q, want %q", evt.Type, jobs.EventJobTerminal)
	}
	if evt.TenantID != pkgcore.TenantID("tenant-a") {
		t.Errorf("event TenantID = %q, want tenant-a (the task's own header)", evt.TenantID)
	}
	payload := events[0]
	if payload.Status != jobs.StatusDeadLetter {
		t.Errorf("payload.Status = %q, want %q", payload.Status, jobs.StatusDeadLetter)
	}
	if payload.JobType != "flaky.dead" {
		t.Errorf("payload.JobType = %q, want flaky.dead", payload.JobType)
	}
	if payload.Error != "permanent failure" {
		t.Errorf("payload.Error = %q, want the terminal failure's message", payload.Error)
	}
	if payload.Attempts != 4 {
		t.Errorf("payload.Attempts = %d, want 4 (retried 3 + the terminal attempt)", payload.Attempts)
	}
	if payload.CompletedAt.IsZero() {
		t.Error("payload.CompletedAt is zero, want the pre-archive publish moment")
	}
}

// TestQueue_HandleErrorAttempt_TerminalBouncePublishesDeadLetterEvent pins
// that an archive-bound tenant-concurrency bounce — whose archive fires the
// FailureHook and the dead-letter metrics, per this package's standing
// "a dead letter must never skip its compensation" rule — publishes the
// dead-letter event too: asynq archives it, DeadLetterJobs lists it, and the
// signal must agree with the record.
func TestQueue_HandleErrorAttempt_TerminalBouncePublishesDeadLetterEvent(t *testing.T) {
	bus := testutil.NewRecordingBus()
	q := newTestQueue(t)
	q.bus = bus
	task := asynqlib.NewTaskWithHeaders("bounced", nil, map[string]string{headerTenantID: "tenant-a"})

	q.handleErrorAttempt(context.Background(), task, errTenantAtCapacity, 3 /* retried */, 3 /* maxRetry */, "job-bounce", nil, obs.FromContext(context.Background()))

	events := terminalEventsFor(bus, "job-bounce")
	if len(events) != 1 {
		t.Fatalf("recorded terminal events = %d, want exactly 1 for an archive-bound bounce: %+v", len(events), bus.Events())
	}
	if events[0].Status != jobs.StatusDeadLetter {
		t.Errorf("payload.Status = %q, want %q", events[0].Status, jobs.StatusDeadLetter)
	}
}

// TestQueue_HandleErrorAttempt_NonTerminalBranchesPublishNothing pins the
// two branches that must stay silent: a retryable failure (asynq will
// retry; no terminal transition happened) and a terminal attempt a
// concurrent Cancel already settled (the cancellation won, and its own
// event went out at Cancel).
func TestQueue_HandleErrorAttempt_NonTerminalBranchesPublishNothing(t *testing.T) {
	bus := testutil.NewRecordingBus()
	q := newTestQueue(t)
	q.bus = bus
	task := asynqlib.NewTaskWithHeaders("flaky", nil, map[string]string{headerTenantID: "tenant-a"})
	log := obs.FromContext(context.Background())
	cancelledAt := time.Now()

	q.handleErrorAttempt(context.Background(), task, wrapFailedAttempt(errors.New("retry me"), time.Second),
		1 /* retried */, 3 /* maxRetry */, "job-retryable", nil, log)
	q.handleErrorAttempt(context.Background(), task, wrapFailedAttempt(errors.New("final failure"), time.Second),
		3 /* retried */, 3 /* maxRetry */, "job-cancelled", &cancelledAt, log)

	if got := len(bus.Events()); got != 0 {
		t.Errorf("recorded terminal events = %d, want 0 (no terminal transition): %+v", got, bus.Events())
	}
}

// TestQueue_PublishTerminal_NilBus_IsANoOp pins the no-bus composition on
// this implementation's direct-publish helper: with no bus configured the
// call returns without touching anything, at no cost — the shape every
// publish point degrades to for a host that never wired a bus.
func TestQueue_PublishTerminal_NilBus_IsANoOp(t *testing.T) {
	q := newTestQueue(t)
	if q.bus != nil {
		t.Fatal("newTestQueue must leave the bus nil for this test")
	}
	q.publishTerminal(context.Background(), pkgcore.TenantID("tenant-a"), jobs.JobTerminalEvent{
		JobID:       jobs.JobID("job-nil-bus"),
		JobType:     "noop",
		Status:      jobs.StatusSucceeded,
		CompletedAt: time.Now(),
	}) // must not panic; nothing to observe beyond the absence of one.
}

// TestQueue_PublishTerminal_FailedPublish_IsLoggedAndDropped pins the
// failure direction at this implementation's publish points: a bus that
// refuses the event is logged and dropped — never returned to the caller
// (the terminal transition it announces has already happened, or is
// happening) and never retried here, since this leg has no outbox and its
// at-least-once direction comes from asynq's own redelivery.
func TestQueue_PublishTerminal_FailedPublish_IsLoggedAndDropped(t *testing.T) {
	bus := testutil.NewRecordingBus()
	bus.SetPublishHook(func(context.Context, pkgcore.Event) error {
		return errors.New("bus unavailable")
	})
	q := newTestQueue(t)
	q.bus = bus

	q.publishTerminal(context.Background(), pkgcore.TenantID("tenant-a"), jobs.JobTerminalEvent{
		JobID:       jobs.JobID("job-failed-publish"),
		JobType:     "noop",
		Status:      jobs.StatusCancelled,
		CompletedAt: time.Now(),
	}) // must not panic and must not surface the bus error anywhere.

	if got := len(bus.Events()); got != 1 {
		t.Errorf("recorded publish attempts = %d, want 1 (the refused delivery)", got)
	}
}
