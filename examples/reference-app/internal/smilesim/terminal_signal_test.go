package smilesim

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// jobTerminalEvent builds the pkgcore.Event the queue publishes for one
// terminal job -- jobs.EventJobTerminal carrying a jobs.JobTerminalEvent
// payload, exactly the shape go/jobs' two implementations publish (the
// in-process struct; TestService_OnJobTerminal_JSONMapPayload_Settles below
// covers the JSON round-trip a broker-backed bus delivers instead).
func jobTerminalEvent(jobID jobs.JobID, tenant pkgcore.TenantID, status jobs.Status) pkgcore.Event {
	return pkgcore.Event{
		Type:     jobs.EventJobTerminal,
		TenantID: tenant,
		Payload: jobs.JobTerminalEvent{
			JobID:   jobID,
			JobType: "ai-gateway.image.generate",
			Status:  status,
		},
	}
}

// TestService_OnJobTerminal_Succeeded_ConfirmsReservation proves the main
// path the subscription exists for: the terminal signal alone -- no
// NotifyOnCompletion call, no job-status poll, and a context carrying no
// tenant of its own, the shape the queue's background publish pass hands a
// subscriber -- settles a succeeded job's reservation into a permanent
// spend. The event's own TenantID is what Confirm is attributed to.
// A repeated signal for the same job (the delivery contract's ordinary
// duplicate) settles again without error or double-applying.
func TestService_OnJobTerminal_Succeeded_ConfirmsReservation(t *testing.T) {
	credits := newTestCreditService(t)
	grantTestCredits(t, credits)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	queue := &recordingQueue{jobID: "job-signal-succeed"}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)
	jobID, err := svc.Simulate(ctx, "photo-1", "")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}
	queue.setJob(newSimulateResultJob(t, jobID, "tenant-acme", jobs.StatusSucceeded, "object-out-1"))

	if err = svc.OnJobTerminal(context.Background(), jobTerminalEvent(jobID, "tenant-acme", jobs.StatusSucceeded)); err != nil {
		t.Fatalf("OnJobTerminal: %v", err)
	}

	bal, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100-CreditsPerSimulation {
		t.Errorf("Available after the terminal signal = %d, want %d", bal.Available, 100-CreditsPerSimulation)
	}
	if bal.Reserved != 0 {
		t.Errorf("Reserved after the terminal signal = %d, want 0", bal.Reserved)
	}
	rows, err := svc.store.listAll(ctx)
	if err != nil {
		t.Fatalf("ReservationStore.listAll: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("reservation rows after the terminal signal = %d, want 0 -- settleCredit deletes the row it settled", len(rows))
	}

	// A duplicate signal leaves the balance exactly where the first left it.
	if err = svc.OnJobTerminal(context.Background(), jobTerminalEvent(jobID, "tenant-acme", jobs.StatusSucceeded)); err != nil {
		t.Fatalf("OnJobTerminal (duplicate signal): %v", err)
	}
	balAgain, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance (after duplicate signal): %v", err)
	}
	if *balAgain != *bal {
		t.Errorf("balance changed on a duplicate terminal signal: first %+v, second %+v", *bal, *balAgain)
	}
}

// TestService_OnJobTerminal_DeadLetterAndCancelled_RefundReservation proves
// the Refund half for both non-success terminal statuses: the signal alone
// releases the reservation back to Available, and a duplicate signal
// refunds nothing twice.
func TestService_OnJobTerminal_DeadLetterAndCancelled_RefundReservation(t *testing.T) {
	for _, status := range []jobs.Status{jobs.StatusDeadLetter, jobs.StatusCancelled} {
		t.Run(string(status), func(t *testing.T) {
			credits := newTestCreditService(t)
			grantTestCredits(t, credits)
			ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")

			queue := &recordingQueue{jobID: "job-signal-" + jobs.JobID(status)}
			svc := newTestService(t, &fakeImageProvider{}, queue, credits)
			jobID, err := svc.Simulate(ctx, "photo-1", "")
			if err != nil {
				t.Fatalf("Simulate: %v", err)
			}

			if err = svc.OnJobTerminal(context.Background(), jobTerminalEvent(jobID, "tenant-acme", status)); err != nil {
				t.Fatalf("OnJobTerminal(%s): %v", status, err)
			}

			bal, err := credits.Balance(ctx)
			if err != nil {
				t.Fatalf("Balance: %v", err)
			}
			if bal.Available != 100 {
				t.Errorf("Available after a %s signal = %d, want 100 (back to the pre-reservation balance)", status, bal.Available)
			}
			if bal.Reserved != 0 {
				t.Errorf("Reserved after a %s signal = %d, want 0", status, bal.Reserved)
			}

			if err = svc.OnJobTerminal(context.Background(), jobTerminalEvent(jobID, "tenant-acme", status)); err != nil {
				t.Fatalf("OnJobTerminal(%s, duplicate signal): %v", status, err)
			}
			balAgain, err := credits.Balance(ctx)
			if err != nil {
				t.Fatalf("Balance (after duplicate signal): %v", err)
			}
			if *balAgain != *bal {
				t.Errorf("balance changed on a duplicate %s signal: first %+v, second %+v", status, *bal, *balAgain)
			}
		})
	}
}

// TestService_OnJobTerminal_NoReservationRow_IsANoOp pins the ledger gate:
// the signal for a job this Service never reserved for -- every job of
// every other task type this app's queue runs, and a simulation created
// while this Service carried no credit wiring -- settles nothing and
// reports no error.
func TestService_OnJobTerminal_NoReservationRow_IsANoOp(t *testing.T) {
	credits := newTestCreditService(t)
	grantTestCredits(t, credits)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	queue := &recordingQueue{}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)
	const jobID jobs.JobID = "job-never-reserved"
	queue.setJob(newSimulateResultJob(t, jobID, "tenant-acme", jobs.StatusSucceeded, "object-out-1"))

	if err := svc.OnJobTerminal(context.Background(), jobTerminalEvent(jobID, "tenant-acme", jobs.StatusSucceeded)); err != nil {
		t.Fatalf("OnJobTerminal: %v", err)
	}

	bal, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100 || bal.Reserved != 0 {
		t.Errorf("balance after a signal for a job with no reservation row = %+v, want Available 100 Reserved 0 -- nothing was reserved, so nothing may settle", *bal)
	}
}

// TestService_OnJobTerminal_JSONMapPayload_Settles pins that the subscriber
// accepts the payload shape a broker-backed bus delivers: pkgcore's
// distributed implementations round-trip a payload through JSON, so the
// same terminal signal arrives as a map keyed by jobs.JobTerminalEvent's
// own json tags. The settlement must not depend on which bus delivered the
// event.
func TestService_OnJobTerminal_JSONMapPayload_Settles(t *testing.T) {
	credits := newTestCreditService(t)
	grantTestCredits(t, credits)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	queue := &recordingQueue{jobID: "job-signal-json"}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)
	jobID, err := svc.Simulate(ctx, "photo-1", "")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}
	queue.setJob(newSimulateResultJob(t, jobID, "tenant-acme", jobs.StatusSucceeded, "object-out-1"))

	event := pkgcore.Event{
		Type:     jobs.EventJobTerminal,
		TenantID: "tenant-acme",
		Payload: map[string]any{
			"job_id":       string(jobID),
			"job_type":     "ai-gateway.image.generate",
			"status":       string(jobs.StatusSucceeded),
			"error":        "",
			"attempts":     1,
			"completed_at": time.Now().UTC().Format(time.RFC3339Nano),
		},
	}
	if err = svc.OnJobTerminal(context.Background(), event); err != nil {
		t.Fatalf("OnJobTerminal (JSON map payload): %v", err)
	}

	bal, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100-CreditsPerSimulation || bal.Reserved != 0 {
		t.Errorf("balance after a JSON-round-tripped terminal signal = %+v, want Available %d Reserved 0", *bal, 100-CreditsPerSimulation)
	}
}

// TestService_OnJobTerminal_PublishesCompletionOnceWithTheJobsResult proves
// the notification half now rides the same signal: it publishes
// EventSimulationCompleted for a job Simulate was given a recipient for,
// with the output object id read back from the succeeded job's own Result
// (the payload deliberately carries no Result body), and a duplicate
// signal never delivers it twice.
func TestService_OnJobTerminal_PublishesCompletionOnceWithTheJobsResult(t *testing.T) {
	bus := pkgcore.NewMemoryEventBus()
	events := subscribeSimulationCompleted(bus)

	queue := &recordingQueue{jobID: "job-signal-notify"}
	svc := NewService(nil, nil, bus, queue, nil, nil, nil)
	const jobID jobs.JobID = "job-signal-notify"
	svc.recipients[jobID] = "user-7"
	queue.setJob(newSimulateResultJob(t, jobID, "tenant-acme", jobs.StatusSucceeded, "object-out-1"))

	if err := svc.OnJobTerminal(context.Background(), jobTerminalEvent(jobID, "tenant-acme", jobs.StatusSucceeded)); err != nil {
		t.Fatalf("OnJobTerminal: %v", err)
	}

	got := events()
	if len(got) != 1 {
		t.Fatalf("published %d completion events, want exactly 1: %+v", len(got), got)
	}
	if got[0].RecipientUserID != "user-7" || got[0].TenantID != "tenant-acme" || got[0].ImageJobID != string(jobID) {
		t.Errorf("payload = %+v, want recipient=user-7 tenant=tenant-acme job=%s", got[0], jobID)
	}
	if !got[0].Succeeded {
		t.Error("Succeeded = false, want true for a StatusSucceeded signal")
	}
	if got[0].OutputObjectID != "object-out-1" {
		t.Errorf("OutputObjectID = %q, want %q -- the read-back of the succeeded job must fill it", got[0].OutputObjectID, "object-out-1")
	}

	// The delivery contract's ordinary duplicate: the second signal is
	// absorbed, not republished.
	if err := svc.OnJobTerminal(context.Background(), jobTerminalEvent(jobID, "tenant-acme", jobs.StatusSucceeded)); err != nil {
		t.Fatalf("OnJobTerminal (duplicate signal): %v", err)
	}
	if got := events(); len(got) != 1 {
		t.Errorf("published %d completion events after a duplicate signal, want still 1", len(got))
	}

	// The poll-driven leg drives the very same latch: a status read that
	// arrives after the signal must not deliver the notification a second
	// time.
	if err := svc.NotifyOnCompletion(context.Background(), queue.jobs[jobID]); err != nil {
		t.Fatalf("NotifyOnCompletion (after the signal): %v", err)
	}
	if got := events(); len(got) != 1 {
		t.Errorf("published %d completion events after the poll leg followed the signal, want still 1 -- both drivers share one latch", len(got))
	}
}

// TestService_OnJobTerminal_ReadBackFailure_PublishesWithoutTheOutputObjectID
// pins the enrichment's best-effort boundary: a job whose row the read-back
// cannot resolve (the queue's retention passed first, say) still gets its
// completion event -- only the output object id is missing -- rather than
// the notification being held back by a detail.
func TestService_OnJobTerminal_ReadBackFailure_PublishesWithoutTheOutputObjectID(t *testing.T) {
	bus := pkgcore.NewMemoryEventBus()
	events := subscribeSimulationCompleted(bus)

	queue := &recordingQueue{}
	svc := NewService(nil, nil, bus, queue, nil, nil, nil)
	const jobID jobs.JobID = "job-signal-aged-out"
	svc.recipients[jobID] = "user-7"
	queue.ageOut(jobID)

	if err := svc.OnJobTerminal(context.Background(), jobTerminalEvent(jobID, "tenant-acme", jobs.StatusSucceeded)); err != nil {
		t.Fatalf("OnJobTerminal: %v", err)
	}

	got := events()
	if len(got) != 1 {
		t.Fatalf("published %d completion events with the read-back failing, want 1: %+v", len(got), got)
	}
	if !got[0].Succeeded {
		t.Error("Succeeded = false, want true -- the signal's own status decides success, never the read-back")
	}
	if got[0].OutputObjectID != "" {
		t.Errorf("OutputObjectID = %q, want empty when the read-back failed", got[0].OutputObjectID)
	}
}

// TestService_OnJobTerminal_RefusedPublish_LeavesNotificationRetryable
// pins the latch's retryability on the signal-driven path: a refused
// publish surfaces the error -- which the queue's publish pass turns into
// a republished signal on its next pass -- and must not leave the delivery
// latched as done, so the republished signal delivers it. An accepted
// delivery, by contrast, stays latched against later signals.
func TestService_OnJobTerminal_RefusedPublish_LeavesNotificationRetryable(t *testing.T) {
	bus := pkgcore.NewMemoryEventBus()
	queue := &recordingQueue{}
	svc := NewService(nil, nil, bus, queue, nil, nil, nil)
	const jobID jobs.JobID = "job-signal-refused"
	svc.recipients[jobID] = "user-7"
	queue.setJob(newSimulateResultJob(t, jobID, "tenant-acme", jobs.StatusSucceeded, "object-out-1"))

	// A subscriber that refuses the first delivery and accepts every later
	// one -- a transient downstream failure.
	attempts := 0
	delivered := 0
	bus.Subscribe(EventSimulationCompleted, func(_ context.Context, _ pkgcore.Event) error {
		attempts++
		if attempts == 1 {
			return errors.New("delivery refused")
		}
		delivered++
		return nil
	})

	event := jobTerminalEvent(jobID, "tenant-acme", jobs.StatusSucceeded)
	if err := svc.OnJobTerminal(context.Background(), event); err == nil {
		t.Fatal("OnJobTerminal with a refused publish succeeded, want its error propagated for the republishing pass")
	}
	if delivered != 0 {
		t.Fatalf("delivered %d events on the refused publish, want 0", delivered)
	}

	// The queue's next pass republishes the terminal signal: the delivery
	// must be re-attempted rather than skipped on a stale latch.
	if err := svc.OnJobTerminal(context.Background(), event); err != nil {
		t.Fatalf("OnJobTerminal (republished signal): %v", err)
	}
	if delivered != 1 {
		t.Errorf("delivered = %d after the republished signal, want 1 -- a refused publish must leave the notification retryable", delivered)
	}

	// A third signal must not deliver again: the accepted delivery stayed
	// latched.
	if err := svc.OnJobTerminal(context.Background(), event); err != nil {
		t.Fatalf("OnJobTerminal (third signal): %v", err)
	}
	if delivered != 1 {
		t.Errorf("delivered = %d after the third signal, want still 1 -- an accepted delivery must remain latched", delivered)
	}
}

// TestService_OnJobTerminal_UnreadablePayload_IsDropped pins the drop rule
// for a signal this Service cannot read: the subscriber logs and drops it
// (returning nil, never failing the publisher) and settles nothing.
func TestService_OnJobTerminal_UnreadablePayload_IsDropped(t *testing.T) {
	credits := newTestCreditService(t)
	grantTestCredits(t, credits)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	svc := newTestService(t, &fakeImageProvider{}, &recordingQueue{jobID: "job-unused"}, credits)
	event := pkgcore.Event{Type: jobs.EventJobTerminal, TenantID: "tenant-acme", Payload: "not a job terminal payload"}

	if err := svc.OnJobTerminal(context.Background(), event); err != nil {
		t.Fatalf("OnJobTerminal with an unreadable payload = %v, want nil -- a dropped event must never fail the publisher", err)
	}

	bal, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100 || bal.Reserved != 0 {
		t.Errorf("balance after an unreadable signal = %+v, want untouched Available 100 Reserved 0", *bal)
	}
}

// TestService_OnJobTerminal_NonTerminalStatus_IsDropped pins the guard that
// keeps Refund's "anything but StatusSucceeded" branch away from a
// non-outcome: a status outside the three terminal states is not the
// signal's shape, so it neither settles nor consumes the job's recipient.
func TestService_OnJobTerminal_NonTerminalStatus_IsDropped(t *testing.T) {
	credits := newTestCreditService(t)
	grantTestCredits(t, credits)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	bus := pkgcore.NewMemoryEventBus()
	events := subscribeSimulationCompleted(bus)
	queue := &recordingQueue{jobID: "job-signal-running"}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)
	svc.bus = bus
	jobID, err := svc.Simulate(ctx, "photo-1", "user-7")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}

	for _, status := range []jobs.Status{jobs.StatusPending, jobs.StatusRunning, jobs.StatusRetrying} {
		if err = svc.OnJobTerminal(context.Background(), jobTerminalEvent(jobID, "tenant-acme", status)); err != nil {
			t.Fatalf("OnJobTerminal(%s): %v", status, err)
		}
	}

	bal, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100-CreditsPerSimulation || bal.Reserved != CreditsPerSimulation {
		t.Errorf("balance after non-terminal signals = %+v, want Available %d Reserved %d -- a non-outcome must not settle", *bal, 100-CreditsPerSimulation, CreditsPerSimulation)
	}
	if got := events(); len(got) != 0 {
		t.Errorf("published %d completion events for non-terminal signals, want 0: %+v", len(got), got)
	}
	// The recipient entry stays on file: the signal was dropped, not
	// processed, so a later true terminal observation must still deliver.
	svc.mu.Lock()
	_, kept := svc.recipients[jobID]
	svc.mu.Unlock()
	if !kept {
		t.Error("the job's recipient entry was consumed by a dropped non-terminal signal")
	}
}

// TestService_OnJobTerminal_NilWiring_IsANoOp pins the optional-seam
// contract every other entry point of this package follows: a Service with
// no credits, store, queue or bus wired settles nothing, publishes nothing
// and does not panic.
func TestService_OnJobTerminal_NilWiring_IsANoOp(t *testing.T) {
	svc := NewService(nil, nil, nil, nil, nil, nil, nil)
	if err := svc.OnJobTerminal(context.Background(), jobTerminalEvent("job-x", "tenant-acme", jobs.StatusSucceeded)); err != nil {
		t.Fatalf("OnJobTerminal on a nil-wired Service = %v, want nil", err)
	}
}
