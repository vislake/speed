package jobs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// This file pins the terminal signal mechanism (events.go, worker.go's
// runTerminalPublisher, store.go's terminal_published_at outbox) end to
// end on StandaloneQueue: which transitions publish, what the payload
// carries for each terminal status, the publish-then-stamp crash window,
// the Start-time catch-up, the writer gate the pass runs under, the nil-bus
// no-op, and the in-place column upgrade with its one-time backfill. The
// asynq leg's publish points are pinned in queue/asynq's own tier and in
// integration_test/, against that implementation's own hooks.

// terminalPayload asserts evt is a jobs.job.terminal event and returns its
// payload.
func terminalPayload(t *testing.T, evt pkgcore.Event) JobTerminalEvent {
	t.Helper()
	if evt.Type != EventJobTerminal {
		t.Fatalf("event type = %q, want %q", evt.Type, EventJobTerminal)
	}
	payload, ok := evt.Payload.(JobTerminalEvent)
	if !ok {
		t.Fatalf("event payload type = %T, want jobs.JobTerminalEvent", evt.Payload)
	}
	return payload
}

// terminalEventsFor returns the recorded terminal-event attempts for jobID,
// in publish order.
func terminalEventsFor(bus *testutil.RecordingBus, id JobID) []JobTerminalEvent {
	var out []JobTerminalEvent
	for _, evt := range bus.Events() {
		if payload, ok := evt.Payload.(JobTerminalEvent); ok && payload.JobID == id {
			out = append(out, payload)
		}
	}
	return out
}

// waitForTerminalEvent waits until a jobs.job.terminal event for id has been
// published, returning the raw recorded event.
func waitForTerminalEvent(t *testing.T, bus *testutil.RecordingBus, id JobID) pkgcore.Event {
	t.Helper()
	return bus.WaitForPublish(t, signalWaitTimeout, "the terminal event for job "+string(id), func(evt pkgcore.Event) bool {
		payload, ok := evt.Payload.(JobTerminalEvent)
		return ok && payload.JobID == id
	})
}

// waitForTerminalStamp waits until id's row carries terminal_published_at —
// the publish pass's publish-then-stamp mark. Because the pass is a single
// goroutine that stamps strictly after one successful Publish, observing
// the stamp also freezes the event count for that row: a stamped row can
// never be selected by a later pass, so counting terminalEventsFor after
// this returns is deterministic rather than racy.
func waitForTerminalStamp(t *testing.T, q *StandaloneQueue, id JobID) {
	t.Helper()
	deadline := time.Now().Add(signalWaitTimeout)
	for {
		rec, err := findByID(context.Background(), q.db, id)
		if err != nil {
			t.Fatalf("findByID(%q) error = %v", id, err)
		}
		if rec.TerminalPublishedAt != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for job %q's terminal_published_at stamp", signalWaitTimeout, id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForAttemptSettled waits until tenant has no running slot left on q,
// i.e. the worker handed a claimed Job back: the point at which that
// attempt's outcome write (and any discarded-outcome handling) has run.
func waitForAttemptSettled(t *testing.T, q *StandaloneQueue, tenant pkgcore.TenantID) {
	t.Helper()
	deadline := time.Now().Add(signalWaitTimeout)
	for {
		q.tenantMu.Lock()
		running := q.runningPerTenant[tenant]
		q.tenantMu.Unlock()
		if running == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for tenant %q's running slot to be released", signalWaitTimeout, tenant)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestTerminalSignal_SucceededJob_PublishesExactlyOneEvent pins the success
// transition's publish: one event, the shared payload shape, the row's own
// tenant and completion moment.
func TestTerminalSignal_SucceededJob_PublishesExactlyOneEvent(t *testing.T) {
	const tenant = pkgcore.TenantID("tenant-a")
	bus := testutil.NewRecordingBus()
	q := newTestQueue(t, WithEventBus(bus))
	if err := q.RegisterHandler(NewHandlerFunc("greet.ok", func(context.Context, *Job, ProgressFn) (Result, error) {
		return Result{Data: []byte("done")}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	startQueue(t, q)

	ctx := pkgcore.WithTenant(context.Background(), tenant)
	id, err := q.Enqueue(ctx, Task{Type: "greet.ok", TenantID: tenant})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	job := waitTerminal(t, q, ctx, id)

	payload := terminalPayload(t, waitForTerminalEvent(t, bus, id))
	if payload.Status != StatusSucceeded {
		t.Errorf("payload.Status = %q, want %q", payload.Status, StatusSucceeded)
	}
	if payload.JobType != "greet.ok" {
		t.Errorf("payload.JobType = %q, want greet.ok", payload.JobType)
	}
	if payload.Error != "" {
		t.Errorf("payload.Error = %q, want empty for a succeeded Job", payload.Error)
	}
	if payload.Attempts != 1 {
		t.Errorf("payload.Attempts = %d, want 1", payload.Attempts)
	}
	if job.CompletedAt == nil || !payload.CompletedAt.Equal(*job.CompletedAt) {
		t.Errorf("payload.CompletedAt = %v, want the row's own completed_at %v", payload.CompletedAt, job.CompletedAt)
	}

	waitForTerminalStamp(t, q, id)
	if got := len(terminalEventsFor(bus, id)); got != 1 {
		t.Errorf("recorded publish attempts for the Job = %d, want exactly 1", got)
	}
}

// TestTerminalSignal_DeadLetterJob_PublishesExactlyOneEvent pins the
// dead-letter transition's publish, payload Error included.
func TestTerminalSignal_DeadLetterJob_PublishesExactlyOneEvent(t *testing.T) {
	const tenant = pkgcore.TenantID("tenant-a")
	bus := testutil.NewRecordingBus()
	q := newTestQueue(t, WithEventBus(bus))
	if err := q.RegisterHandler(NewHandlerFunc("always.fails", func(context.Context, *Job, ProgressFn) (Result, error) {
		return Result{}, errors.New("simulated failure")
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	startQueue(t, q)

	ctx := pkgcore.WithTenant(context.Background(), tenant)
	id, err := q.Enqueue(ctx, Task{Type: "always.fails", TenantID: tenant}, WithMaxRetries(0))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	job := waitTerminal(t, q, ctx, id)

	evt := waitForTerminalEvent(t, bus, id)
	if evt.TenantID != tenant {
		t.Errorf("event TenantID = %q, want %q", evt.TenantID, tenant)
	}
	payload := terminalPayload(t, evt)
	if payload.Status != StatusDeadLetter {
		t.Errorf("payload.Status = %q, want %q", payload.Status, StatusDeadLetter)
	}
	if payload.Error != "simulated failure" {
		t.Errorf("payload.Error = %q, want the handler's failure message", payload.Error)
	}
	if payload.Attempts != 1 {
		t.Errorf("payload.Attempts = %d, want 1", payload.Attempts)
	}
	if job.CompletedAt == nil || !payload.CompletedAt.Equal(*job.CompletedAt) {
		t.Errorf("payload.CompletedAt = %v, want the row's own completed_at %v", payload.CompletedAt, job.CompletedAt)
	}

	waitForTerminalStamp(t, q, id)
	if got := len(terminalEventsFor(bus, id)); got != 1 {
		t.Errorf("recorded publish attempts for the Job = %d, want exactly 1", got)
	}
}

// TestTerminalSignal_CancelledJob_PublishesTheCancellationMoment pins the
// cancelled status's CompletedAt source, the pin the contract text carries:
// the row's completed_at stays NULL for a cancelled Job (markCancelled
// writes none), and the payload reports the cancellation's own instant —
// the row's updated_at, which no later write moves. The stamp must not move
// it either (stampTerminalPublished's UpdateColumn), which the read-back
// after the stamp landing pins.
func TestTerminalSignal_CancelledJob_PublishesTheCancellationMoment(t *testing.T) {
	const tenant = pkgcore.TenantID("tenant-a")
	bus := testutil.NewRecordingBus()
	q := newTestQueue(t, WithEventBus(bus))

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseNow := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseNow()
	started := make(chan struct{}, 1)
	if err := q.RegisterHandler(NewHandlerFunc("slow.job", func(context.Context, *Job, ProgressFn) (Result, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	startQueue(t, q)

	ctx := pkgcore.WithTenant(context.Background(), tenant)
	id, err := q.Enqueue(ctx, Task{Type: "slow.job", TenantID: tenant})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(signalWaitTimeout):
		t.Fatal("timed out waiting for the handler to start; the Job never reached StatusRunning")
	}

	if cancelErr := q.Cancel(ctx, id); cancelErr != nil {
		t.Fatalf("Cancel() error = %v", cancelErr)
	}
	payload := terminalPayload(t, waitForTerminalEvent(t, bus, id))
	releaseNow() // let the (now discarded) attempt finish so the worker frees.

	if payload.Status != StatusCancelled {
		t.Errorf("payload.Status = %q, want %q", payload.Status, StatusCancelled)
	}
	if payload.CompletedAt.IsZero() {
		t.Error("payload.CompletedAt is zero, want the cancellation's own instant")
	}
	waitForTerminalStamp(t, q, id)

	job, err := q.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if job.Status != StatusCancelled {
		t.Fatalf("job.Status = %q, want %q", job.Status, StatusCancelled)
	}
	if job.CompletedAt != nil {
		t.Errorf("row completed_at = %v, want nil for a cancelled Job (the pin's other half: the moment lives in updated_at)", job.CompletedAt)
	}
	if !payload.CompletedAt.Equal(job.UpdatedAt) {
		t.Errorf("payload.CompletedAt = %v, want the row's updated_at %v (the cancellation moment the stamp must not move)", payload.CompletedAt, job.UpdatedAt)
	}
	if got := len(terminalEventsFor(bus, id)); got != 1 {
		t.Errorf("recorded publish attempts for the Job = %d, want exactly 1", got)
	}
}

// TestTerminalSignal_CancelWins_DiscardedOutcomePublishesNothing pins the
// cancel-wins interaction with the signal: an attempt whose outcome a
// concurrent Cancel discards produces no event of its own — the cancelled
// transition's own event (published from the row, not the attempt) is the
// only one, even though the handler returned success after the cancel.
func TestTerminalSignal_CancelWins_DiscardedOutcomePublishesNothing(t *testing.T) {
	const tenant = pkgcore.TenantID("tenant-a")
	bus := testutil.NewRecordingBus()
	q := newTestQueue(t, WithEventBus(bus))

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseNow := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseNow()
	started := make(chan struct{}, 1)
	if err := q.RegisterHandler(NewHandlerFunc("late.success", func(context.Context, *Job, ProgressFn) (Result, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return Result{Data: []byte("succeeded after the cancel")}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	startQueue(t, q)

	ctx := pkgcore.WithTenant(context.Background(), tenant)
	id, err := q.Enqueue(ctx, Task{Type: "late.success", TenantID: tenant})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(signalWaitTimeout):
		t.Fatal("timed out waiting for the handler to start")
	}
	if err := q.Cancel(ctx, id); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	payload := terminalPayload(t, waitForTerminalEvent(t, bus, id))
	if payload.Status != StatusCancelled {
		t.Fatalf("payload.Status = %q, want %q", payload.Status, StatusCancelled)
	}
	waitForTerminalStamp(t, q, id)

	// Release the attempt and wait until its outcome write has actually run
	// (the worker released the tenant slot), so a success event from the
	// discarded outcome would demonstrably have had its chance to appear.
	releaseNow()
	waitForAttemptSettled(t, q, tenant)

	if got := len(terminalEventsFor(bus, id)); got != 1 {
		t.Errorf("recorded publish attempts for the Job = %d, want exactly 1 (the cancel's); a discarded outcome must publish nothing", got)
	}
}

// TestTerminalSignal_FailedPublish_LeavesRowOwedAndNextPassRepublishes pins
// the publish-then-stamp order and the retry direction: a Publish that
// returns an error must leave the row unstamped (never a "sent" mark for a
// never-delivered event), and a later pass must republish it.
func TestTerminalSignal_FailedPublish_LeavesRowOwedAndNextPassRepublishes(t *testing.T) {
	const tenant = pkgcore.TenantID("tenant-a")
	bus := testutil.NewRecordingBus()
	var fail atomic.Bool
	fail.Store(true)
	bus.SetPublishHook(func(context.Context, pkgcore.Event) error {
		if fail.Load() {
			return errors.New("bus unavailable")
		}
		return nil
	})

	q := newTestQueue(t, WithEventBus(bus))
	if err := q.RegisterHandler(NewHandlerFunc("greet.ok2", func(context.Context, *Job, ProgressFn) (Result, error) {
		return Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	startQueue(t, q)

	ctx := pkgcore.WithTenant(context.Background(), tenant)
	id, err := q.Enqueue(ctx, Task{Type: "greet.ok2", TenantID: tenant})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitTerminal(t, q, ctx, id)

	// The first publish attempt is recorded and then fails inside the hook,
	// so the pass cannot have stamped the row: the failed attempt observed
	// below is in flight or finished, and in neither state can a stamp exist
	// yet (the hook has not returned nil).
	waitForTerminalEvent(t, bus, id)
	rec, err := findByID(context.Background(), q.db, id)
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if rec.TerminalPublishedAt != nil {
		t.Fatal("terminal_published_at was stamped for an event whose Publish failed; the stamp must follow a successful Publish only")
	}

	fail.Store(false)
	waitForTerminalStamp(t, q, id)
	if got := len(terminalEventsFor(bus, id)); got < 2 {
		t.Errorf("recorded publish attempts = %d, want at least 2 (one failed, then the republish)", got)
	}
}

// TestTerminalSignal_PublishingNeverBlocksTheTerminalPath pins the
// separation the mechanism rests on: the publish pass shares nothing with
// the worker's execution path, so a Publish call stuck forever (a hung
// broker, here a blocking hook) cannot delay a Job's terminal transition —
// a Job still reaches StatusSucceeded and a Cancel still lands while the
// pass sits blocked inside its first publish.
func TestTerminalSignal_PublishingNeverBlocksTheTerminalPath(t *testing.T) {
	const tenant = pkgcore.TenantID("tenant-a")
	bus := testutil.NewRecordingBus()
	// The channels are closed through releaseNow/started-style helpers with
	// a deferred invocation, so that even a failed assertion (a t.Fatalf
	// that skips the rest of the body) cannot leave the publish pass or a
	// handler blocked: an undrained goroutine would outlive this test's
	// Close and keep logging through the process-global logger while later
	// tests run.
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseNow := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseNow()
	bus.SetPublishHook(func(context.Context, pkgcore.Event) error {
		<-release
		return nil
	})

	q := newTestQueue(t, WithEventBus(bus))
	if err := q.RegisterHandler(NewHandlerFunc("greet.ok3", func(context.Context, *Job, ProgressFn) (Result, error) {
		return Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	// The second Job's handler blocks until released, so that Job is
	// deterministically still running when Cancel lands: with an
	// instantly-succeeding handler the dispatcher could race the Cancel
	// and settle the Job as succeeded first, which would test nothing.
	secondStarted := make(chan struct{}, 1)
	if err := q.RegisterHandler(NewHandlerFunc("slow.ok3", func(context.Context, *Job, ProgressFn) (Result, error) {
		select {
		case secondStarted <- struct{}{}:
		default:
		}
		<-release
		return Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	startQueue(t, q)

	ctx := pkgcore.WithTenant(context.Background(), tenant)
	firstID, err := q.Enqueue(ctx, Task{Type: "greet.ok3", TenantID: tenant})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	// The pass is blocked inside the first Job's publish from here on.
	waitTerminal(t, q, ctx, firstID)

	secondID, err := q.Enqueue(ctx, Task{Type: "slow.ok3", TenantID: tenant})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	select {
	case <-secondStarted:
	case <-time.After(signalWaitTimeout):
		t.Fatal("timed out waiting for the second Job's handler to start")
	}
	if cancelErr := q.Cancel(ctx, secondID); cancelErr != nil {
		t.Fatalf("Cancel() error = %v", cancelErr)
	}
	job, err := q.Get(ctx, secondID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if job.Status != StatusCancelled {
		t.Fatalf("job.Status = %q, want %q while the publish pass is blocked", job.Status, StatusCancelled)
	}

	releaseNow()
	waitForTerminalStamp(t, q, firstID)
	waitForTerminalStamp(t, q, secondID)
}

// TestTerminalSignal_PassReadFailure_LogsAndNeverStamps pins the publish
// pass's failure direction for its own read: a pass whose query fails —
// the table renamed out from under it, the store-table sabotage technique
// the fail-closed tier uses — logs and returns without publishing or
// stamping anything, and the queue recovers on its own once the read works
// again: the outage can delay owed events, never mark them sent.
func TestTerminalSignal_PassReadFailure_LogsAndNeverStamps(t *testing.T) {
	const tenant = pkgcore.TenantID("tenant-a")
	bus := testutil.NewRecordingBus()
	q := newTestQueue(t, WithEventBus(bus))
	if err := q.RegisterHandler(NewHandlerFunc("greet.outage", func(context.Context, *Job, ProgressFn) (Result, error) {
		return Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	startQueue(t, q)

	const sabotageAlias = "jobs_terminal_signal_sabotage"
	if err := q.db.Exec("ALTER TABLE " + jobsTable + " RENAME TO " + sabotageAlias).Error; err != nil {
		t.Fatalf("rename jobs table: %v", err)
	}
	q.terminalPublishOnce() // must log the read failure and return, not panic.
	if got := len(bus.Events()); got != 0 {
		t.Errorf("a pass with an unreadable jobs table published %d events, want 0", got)
	}
	if err := q.db.Exec("ALTER TABLE " + sabotageAlias + " RENAME TO " + jobsTable).Error; err != nil {
		t.Fatalf("rename jobs table back: %v", err)
	}

	ctx := pkgcore.WithTenant(context.Background(), tenant)
	id, err := q.Enqueue(ctx, Task{Type: "greet.outage", TenantID: tenant})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitTerminal(t, q, ctx, id)
	waitForTerminalEvent(t, bus, id)
	waitForTerminalStamp(t, q, id)
}

// TestTerminalSignal_StampFailure_RepublishesRatherThanLosing pins the
// pass's other failure direction: the event goes out, the stamp's write
// fails (the table is renamed away in the instant between Publish's return
// and the stamp — the publish hook runs exactly there), so the row stays
// owed and a later pass republishes it. A duplicate, never a silent loss —
// the consumer contract's idempotency clause is written for exactly this.
func TestTerminalSignal_StampFailure_RepublishesRatherThanLosing(t *testing.T) {
	const tenant = pkgcore.TenantID("tenant-a")
	const sabotageAlias = "jobs_terminal_stamp_sabotage"
	bus := testutil.NewRecordingBus()
	q := newTestQueue(t, WithEventBus(bus))
	if err := q.RegisterHandler(NewHandlerFunc("greet.stampfail", func(context.Context, *Job, ProgressFn) (Result, error) {
		return Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	var sabotaged atomic.Bool
	bus.SetPublishHook(func(context.Context, pkgcore.Event) error {
		if sabotaged.CompareAndSwap(false, true) {
			// Rename for the duration of the stamp attempt only: the stamp
			// runs on the pass goroutine immediately after this hook
			// returns, so a short deferred repair renames the table back
			// strictly after that attempt — deterministic, not a race with
			// the clock.
			if err := q.db.Exec("ALTER TABLE " + jobsTable + " RENAME TO " + sabotageAlias).Error; err != nil {
				t.Errorf("rename jobs table: %v", err)
				return nil
			}
			time.AfterFunc(50*time.Millisecond, func() {
				if err := q.db.Exec("ALTER TABLE " + sabotageAlias + " RENAME TO " + jobsTable).Error; err != nil {
					t.Errorf("rename jobs table back: %v", err)
				}
			})
		}
		return nil
	})
	startQueue(t, q)

	ctx := pkgcore.WithTenant(context.Background(), tenant)
	id, err := q.Enqueue(ctx, Task{Type: "greet.stampfail", TenantID: tenant})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	// Wait for the republish through the bus alone, not through the
	// database: the jobs table is renamed away during the stamp-failure
	// window, and the second recorded attempt can only exist after the
	// repair (a pass must read the table before it can publish), so
	// observing it is both the assertion and the all-clear for DB reads.
	deadline := time.Now().Add(signalWaitTimeout)
	for len(terminalEventsFor(bus, id)) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for the republish; recorded publish attempts = %d, want at least 2 (the unstamped-first attempt, then the republish)",
				signalWaitTimeout, len(terminalEventsFor(bus, id)))
		}
		time.Sleep(5 * time.Millisecond)
	}

	job, err := q.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if job.Status != StatusSucceeded {
		t.Errorf("job.Status = %q, want %q", job.Status, StatusSucceeded)
	}
	waitForTerminalStamp(t, q, id)
}

// TestTerminalSignal_NilBus_PublishesNothingAndStampsNothing pins the
// no-bus composition: without WithEventBus the queue behaves exactly as it
// did before the signal existed — no publish pass, no publishes, and the
// outbox column never written. The row left owed here is exactly what the
// catch-up test below republishes.
func TestTerminalSignal_NilBus_PublishesNothingAndStampsNothing(t *testing.T) {
	const tenant = pkgcore.TenantID("tenant-a")
	q := newTestQueue(t) // no WithEventBus
	if err := q.RegisterHandler(NewHandlerFunc("greet.nobus", func(context.Context, *Job, ProgressFn) (Result, error) {
		return Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	startQueue(t, q)

	ctx := pkgcore.WithTenant(context.Background(), tenant)
	id, err := q.Enqueue(ctx, Task{Type: "greet.nobus", TenantID: tenant})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitTerminal(t, q, ctx, id)

	rec, err := findByID(context.Background(), q.db, id)
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if rec.TerminalPublishedAt != nil {
		t.Error("terminal_published_at was written by a queue with no bus configured")
	}
}

// TestTerminalSignal_NextStartRepublishesRowsLeftOwed pins the Start-time
// catch-up: a terminal row left owed by an earlier process — here one that
// ran with no bus, the same end state a crash between the terminal write
// and the stamp leaves — is republished by the next process's first publish
// pass, the recovery spirit resetInterruptedRecords applies to execution.
func TestTerminalSignal_NextStartRepublishesRowsLeftOwed(t *testing.T) {
	const tenant = pkgcore.TenantID("tenant-a")
	q1 := newTestQueue(t) // the earlier process: no bus, so nothing was ever stamped.
	if err := q1.RegisterHandler(NewHandlerFunc("greet.owed", func(context.Context, *Job, ProgressFn) (Result, error) {
		return Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	startQueue(t, q1)

	ctx := pkgcore.WithTenant(context.Background(), tenant)
	id, err := q1.Enqueue(ctx, Task{Type: "greet.owed", TenantID: tenant})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitTerminal(t, q1, ctx, id)

	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := q1.Close(closeCtx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	bus := testutil.NewRecordingBus()
	q2 := NewStandaloneQueue(q1.db, WithPollInterval(15*time.Millisecond), WithEventBus(bus))
	startQueue(t, q2)

	payload := terminalPayload(t, waitForTerminalEvent(t, bus, id))
	if payload.Status != StatusSucceeded {
		t.Errorf("republished payload.Status = %q, want %q", payload.Status, StatusSucceeded)
	}
	waitForTerminalStamp(t, q2, id)
}

// TestTerminalSignal_RefusedStartPublishesNothing pins the writer-gate
// discipline the design rests on: the publish pass runs only under the
// queue_writers registration, so a Start refused with ErrQueueWriterActive
// leaves the incumbent's owed rows alone — nothing is published by a queue
// that is not the jobs table's live writer.
func TestTerminalSignal_RefusedStartPublishesNothing(t *testing.T) {
	const tenant = pkgcore.TenantID("tenant-a")
	q1 := newTestQueue(t) // holds the writer gate; no bus.
	if err := q1.RegisterHandler(NewHandlerFunc("greet.gate", func(context.Context, *Job, ProgressFn) (Result, error) {
		return Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	startQueue(t, q1)

	ctx := pkgcore.WithTenant(context.Background(), tenant)
	id, err := q1.Enqueue(ctx, Task{Type: "greet.gate", TenantID: tenant})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitTerminal(t, q1, ctx, id)

	bus := testutil.NewRecordingBus()
	q2 := NewStandaloneQueue(q1.db, WithPollInterval(15*time.Millisecond), WithEventBus(bus))
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = q2.Close(closeCtx)
	})

	if startErr := q2.Start(context.Background()); !apperr.HasCode(startErr, ErrQueueWriterActive.Code) {
		t.Fatalf("Start() error = %v, want ErrQueueWriterActive while q1 holds the gate", startErr)
	}
	if got := len(bus.Events()); got != 0 {
		t.Errorf("a refused Start published %d events, want 0 (the pass runs only under the writer gate)", got)
	}
	rec, err := findByID(context.Background(), q1.db, id)
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if rec.TerminalPublishedAt != nil {
		t.Error("terminal_published_at was stamped by a queue whose Start was refused")
	}
}

// TestEnsureJobsSchema_UpgradesLegacyTableWithoutTerminalPublishedAt proves
// the in-place column add and its one-time backfill: a jobs table created
// by a release predating terminal_published_at must come out of
// ensureJobsSchema with the column present, every pre-existing terminal row
// stamped at the change moment (historical terminal states are never
// republished), every non-terminal row still NULL, and the publisher's
// pending index created. A later re-run must NOT re-stamp — the backfill is
// conditional on the column add, so it can never swallow an owed event —
// and a bus-equipped queue over the upgraded table must publish the next
// new terminal transition normally.
func TestEnsureJobsSchema_UpgradesLegacyTableWithoutTerminalPublishedAt(t *testing.T) {
	const tenant = pkgcore.TenantID("tenant-a")
	db := dbtest.NewSQLite(t)
	legacy := `CREATE TABLE ` + jobsTable + ` (
		id              VARCHAR(36) NOT NULL PRIMARY KEY,
		type            VARCHAR(255) NOT NULL,
		tenant_id       VARCHAR(64) NOT NULL,
		payload         BLOB,
		idempotency_key VARCHAR(255) NOT NULL DEFAULT '',
		status          VARCHAR(32) NOT NULL,
		claimed_by      VARCHAR(64) NOT NULL DEFAULT '',
		priority        INTEGER NOT NULL,
		progress_pct    INTEGER NOT NULL DEFAULT 0,
		progress_msg    VARCHAR(1000) NOT NULL DEFAULT '',
		result          BLOB,
		error_message   VARCHAR(4000) NOT NULL DEFAULT '',
		attempts        INTEGER NOT NULL DEFAULT 0,
		max_retries     INTEGER NOT NULL,
		timeout_nanos   BIGINT NOT NULL,
		scheduled_at    TIMESTAMP NOT NULL,
		created_at      TIMESTAMP NOT NULL,
		updated_at      TIMESTAMP NOT NULL,
		started_at      TIMESTAMP,
		completed_at    TIMESTAMP
	)`
	if err := db.Exec(legacy).Error; err != nil {
		t.Fatalf("create legacy jobs table: %v", err)
	}
	now := time.Now()
	insert := func(id string, status Status, completedAt *time.Time) {
		t.Helper()
		if err := db.Exec(
			`INSERT INTO `+jobsTable+` (id, type, tenant_id, status, priority, max_retries, timeout_nanos, scheduled_at, created_at, updated_at, completed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, "legacy.type", string(tenant), string(status), int(PriorityNormal), DefaultMaxRetries, int64(DefaultTimeout),
			now, now, now, completedAt,
		).Error; err != nil {
			t.Fatalf("insert legacy row %q: %v", id, err)
		}
	}
	insert("legacy-pending", StatusPending, nil)
	insert("legacy-succeeded", StatusSucceeded, &now)
	insert("legacy-cancelled", StatusCancelled, nil)

	before := time.Now()
	if err := ensureJobsSchema(context.Background(), db); err != nil {
		t.Fatalf("ensureJobsSchema() over a legacy table error = %v", err)
	}
	// Idempotent over the upgraded table too.
	if err := ensureJobsSchema(context.Background(), db); err != nil {
		t.Fatalf("second ensureJobsSchema() over the upgraded table error = %v", err)
	}

	stampOf := func(id JobID) *time.Time {
		t.Helper()
		rec, err := findByID(context.Background(), db, id)
		if err != nil {
			t.Fatalf("findByID(%q) error = %v", id, err)
		}
		return rec.TerminalPublishedAt
	}
	for _, id := range []JobID{"legacy-succeeded", "legacy-cancelled"} {
		stamp := stampOf(id)
		if stamp == nil {
			t.Fatalf("historical terminal row %q came out unstamped; the column add must backfill it", id)
		}
		if stamp.Before(before) {
			t.Errorf("row %q stamped at %v, want the column-add moment (>= %v)", id, stamp, before)
		}
	}
	if stamp := stampOf("legacy-pending"); stamp != nil {
		t.Errorf("non-terminal row stamped at %v, want NULL (its terminal transition has not happened)", stamp)
	}

	var indexCount int64
	if err := db.Raw(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_jobs_terminal_pending'`).Scan(&indexCount).Error; err != nil {
		t.Fatalf("probe for the pending index: %v", err)
	}
	if indexCount != 1 {
		t.Errorf("idx_jobs_terminal_pending present = %d, want 1", indexCount)
	}

	// The re-run guard, run before any publisher exists over this table:
	// un-stamping a row and running ensureJobsSchema again must leave it
	// owed — the backfill runs only when this call is the one adding the
	// column, never as a sweep that would silently mark an owed event as
	// sent.
	originalStamp := stampOf("legacy-succeeded")
	if err := db.Exec(`UPDATE `+jobsTable+` SET terminal_published_at = NULL WHERE id = ?`, "legacy-succeeded").Error; err != nil {
		t.Fatalf("un-stamp legacy row: %v", err)
	}
	if err := ensureJobsSchema(context.Background(), db); err != nil {
		t.Fatalf("ensureJobsSchema() over the already-upgraded table error = %v", err)
	}
	if stamp := stampOf("legacy-succeeded"); stamp != nil {
		t.Errorf("re-running ensureJobsSchema re-stamped an owed row at %v; the backfill must be conditional on the column add", stamp)
	}
	if err := db.Exec(`UPDATE `+jobsTable+` SET terminal_published_at = ? WHERE id = ?`, originalStamp, "legacy-succeeded").Error; err != nil {
		t.Fatalf("restore legacy row's stamp: %v", err)
	}

	// The upgraded table serves the signal normally: nothing historical is
	// republished (the backfilled rows are stamped), and a NEW terminal
	// transition publishes.
	bus := testutil.NewRecordingBus()
	q := NewStandaloneQueue(db, WithPollInterval(15*time.Millisecond), WithEventBus(bus))
	if err := q.RegisterHandler(NewHandlerFunc("fresh.job", func(context.Context, *Job, ProgressFn) (Result, error) {
		return Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	startQueue(t, q)
	// Several publish passes run in this window; none may touch the
	// historical rows.
	time.Sleep(3 * q.pollInterval)
	if got := len(bus.Events()); got != 0 {
		t.Fatalf("historical terminal rows were republished: %d events recorded: %+v", got, bus.Events())
	}

	ctx := pkgcore.WithTenant(context.Background(), tenant)
	id, err := q.Enqueue(ctx, Task{Type: "fresh.job", TenantID: tenant})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitTerminal(t, q, ctx, id)
	payload := terminalPayload(t, waitForTerminalEvent(t, bus, id))
	if payload.Status != StatusSucceeded {
		t.Errorf("new Job's payload.Status = %q, want %q", payload.Status, StatusSucceeded)
	}

	// The re-run guard: un-stamping a row and running ensureJobsSchema again
	// must leave it owed — the backfill runs only when this call is the one
	// adding the column, never as a sweep that would silently mark an owed
	// event as sent.
	if err := db.Exec(`UPDATE `+jobsTable+` SET terminal_published_at = NULL WHERE id = ?`, "legacy-succeeded").Error; err != nil {
		t.Fatalf("un-stamp legacy row: %v", err)
	}
	if err := ensureJobsSchema(context.Background(), db); err != nil {
		t.Fatalf("ensureJobsSchema() over the already-upgraded table error = %v", err)
	}
	if stamp := stampOf("legacy-succeeded"); stamp != nil {
		t.Errorf("re-running ensureJobsSchema re-stamped an owed row at %v; the backfill must be conditional on the column add", stamp)
	}
}
