package smilesim

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	aigateway "github.com/vislake/speed/go/ai-gateway"
	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// TestService_Simulate_InsufficientCredits_RefusesBeforeAnyEnqueue is the
// mandated proof: a tenant whose balance cannot cover
// CreditsPerSimulation is refused with billing.ErrInsufficientCredits
// BEFORE Gateway.GenerateImage ever reaches the queue -- queue.enqueueCalls
// stays at zero, proving go/ai-gateway (and, transitively, any real
// vendor) was never invoked.
func TestService_Simulate_InsufficientCredits_RefusesBeforeAnyEnqueue(t *testing.T) {
	credits := newTestCreditService(t)
	queue := &recordingQueue{jobID: "job-should-never-run"}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)

	// tenant-acme's balance was never granted anything -- CreditService
	// materializes a fresh, all-zero balance on first read, so
	// CreditsPerSimulation (10) already exceeds it.
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	jobID, err := svc.Simulate(ctx, "photo-1", "")
	if err == nil {
		t.Fatalf("Simulate with an insufficient balance succeeded (job %q), want billing.ErrInsufficientCredits", jobID)
	}
	if jobID != "" {
		t.Errorf("Simulate returned job id %q on refusal, want empty", jobID)
	}
	if !apperr.HasCode(err, "billing.insufficient_credits") {
		t.Fatalf("Simulate error = %v, want a coded billing.insufficient_credits error", err)
	}
	if queue.enqueueCalls != 0 {
		t.Errorf("queue.enqueueCalls = %d, want 0 -- Gateway.GenerateImage (and go/ai-gateway) must never be reached on an insufficient balance", queue.enqueueCalls)
	}

	bal, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 0 || bal.Reserved != 0 {
		t.Errorf("balance after a refused reservation = %+v, want zero on both -- PreDeduct must leave no trace", bal)
	}
}

// TestService_Simulate_SufficientCredits_ReservesBeforeEnqueue proves the
// success half of the same ordering: a tenant with enough balance is
// debited into Reserved (never Available -> nothing, and never a second,
// unrelated bucket) BEFORE the enqueue happens, and the enqueue then
// genuinely runs.
func TestService_Simulate_SufficientCredits_ReservesBeforeEnqueue(t *testing.T) {
	credits := newTestCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if _, err := credits.Grant(ctx, billing.GrantInput{Amount: 100, Reason: "test:seed"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	queue := &recordingQueue{jobID: "job-99"}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)

	jobID, err := svc.Simulate(ctx, "photo-1", "")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}
	if jobID != queue.jobID {
		t.Fatalf("Simulate returned job id %q, want the queue's %q", jobID, queue.jobID)
	}
	if queue.enqueueCalls != 1 {
		t.Fatalf("queue.enqueueCalls = %d, want exactly 1", queue.enqueueCalls)
	}

	bal, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100-CreditsPerSimulation {
		t.Errorf("Available = %d, want %d", bal.Available, 100-CreditsPerSimulation)
	}
	if bal.Reserved != CreditsPerSimulation {
		t.Errorf("Reserved = %d, want %d", bal.Reserved, CreditsPerSimulation)
	}
}

// newSimulateResultJob builds the *jobs.Job NotifyOnCompletion expects for
// a StatusSucceeded job carrying outputObjectID as its ImageJobResult.
func newSimulateResultJob(t *testing.T, jobID jobs.JobID, tenantID pkgcore.TenantID, status jobs.Status, outputObjectID string) *jobs.Job {
	t.Helper()
	job := &jobs.Job{ID: jobID, TenantID: tenantID, Status: status}
	if status == jobs.StatusSucceeded {
		data, err := json.Marshal(aigateway.ImageJobResult{OutputObjectID: outputObjectID})
		if err != nil {
			t.Fatalf("marshal ImageJobResult: %v", err)
		}
		job.Result = &jobs.Result{Data: data}
	}
	return job
}

// TestService_NotifyOnCompletion_Succeeded_ConfirmsReservation proves the
// Confirm half of settleCredit end to end: a succeeded job's reservation
// becomes a permanent spend (Reserved returns to zero, Available stays
// debited), and a second, repeated poll of the same already-terminal job
// settles again without error or double-applying -- the "provably safe
// under a retried job settlement" property.
func TestService_NotifyOnCompletion_Succeeded_ConfirmsReservation(t *testing.T) {
	credits := newTestCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if _, err := credits.Grant(ctx, billing.GrantInput{Amount: 100, Reason: "test:seed"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	queue := &recordingQueue{jobID: "job-succeed-1"}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)
	jobID, err := svc.Simulate(ctx, "photo-1", "")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}

	job := newSimulateResultJob(t, jobID, "tenant-acme", jobs.StatusSucceeded, "object-out-1")
	if err = svc.NotifyOnCompletion(ctx, job); err != nil {
		t.Fatalf("NotifyOnCompletion: %v", err)
	}

	bal, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100-CreditsPerSimulation {
		t.Errorf("Available after confirm = %d, want %d", bal.Available, 100-CreditsPerSimulation)
	}
	if bal.Reserved != 0 {
		t.Errorf("Reserved after confirm = %d, want 0", bal.Reserved)
	}

	// A repeated poll of the same, already-settled job must settle again
	// without error and without moving the balance a second time.
	if err = svc.NotifyOnCompletion(ctx, job); err != nil {
		t.Fatalf("NotifyOnCompletion (repeated poll): %v", err)
	}
	balAgain, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance (after repeated poll): %v", err)
	}
	if *balAgain != *bal {
		t.Errorf("balance changed on a repeated settle of an already-confirmed job: first %+v, second %+v", *bal, *balAgain)
	}
}

// TestService_NotifyOnCompletion_DeadLetter_RefundsReservation proves the
// Refund half: a dead-lettered job's reservation is released back to
// Available in full, and a repeated poll settles again without error or
// double-refunding.
func TestService_NotifyOnCompletion_DeadLetter_RefundsReservation(t *testing.T) {
	credits := newTestCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if _, err := credits.Grant(ctx, billing.GrantInput{Amount: 100, Reason: "test:seed"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	queue := &recordingQueue{jobID: "job-fail-1"}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)
	jobID, err := svc.Simulate(ctx, "photo-1", "")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}

	job := newSimulateResultJob(t, jobID, "tenant-acme", jobs.StatusDeadLetter, "")
	if err = svc.NotifyOnCompletion(ctx, job); err != nil {
		t.Fatalf("NotifyOnCompletion: %v", err)
	}

	bal, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100 {
		t.Errorf("Available after refund = %d, want 100 (back to the pre-reservation balance)", bal.Available)
	}
	if bal.Reserved != 0 {
		t.Errorf("Reserved after refund = %d, want 0", bal.Reserved)
	}

	if err = svc.NotifyOnCompletion(ctx, job); err != nil {
		t.Fatalf("NotifyOnCompletion (repeated poll): %v", err)
	}
	balAgain, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance (after repeated poll): %v", err)
	}
	if *balAgain != *bal {
		t.Errorf("balance changed on a repeated settle of an already-refunded job: first %+v, second %+v", *bal, *balAgain)
	}
}

func TestService_NotifyOnCompletion_NilBus_IsANoOp(t *testing.T) {
	svc := NewService(nil, nil, nil, nil, nil, nil, nil)
	job := &jobs.Job{ID: "job-x", TenantID: "tenant-acme", Status: jobs.StatusSucceeded}
	// Never given a recipient, so this would be a no-op regardless, but the
	// point is that a nil bus must not panic even when it IS reached.
	svc.recipients[job.ID] = "user-7"
	if err := svc.NotifyOnCompletion(context.Background(), job); err != nil {
		t.Fatalf("NotifyOnCompletion with a nil bus error = %v, want nil", err)
	}
}

// TestService_ReconcileOutstandingCredits_SettlesAJobNeverPolled pins
// the sweep's reach: settleCredit must be reachable beyond
// NotifyOnCompletion, which only a client polling the job-status route
// drives -- a reservation whose job finished while nobody was watching (a
// closed tab, a dropped connection, a caller that simply never checked
// back) must not stay Reserved forever with no other path to settle it.
// This test drives Simulate to
// open a real reservation, records the resulting job as StatusSucceeded
// directly on queue (standing in for "the job finished"), and calls ONLY
// ReconcileOutstandingCredits -- NotifyOnCompletion is never called at
// all -- proving the reservation still settles.
func TestService_ReconcileOutstandingCredits_SettlesAJobNeverPolled(t *testing.T) {
	credits := newTestCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if _, err := credits.Grant(ctx, billing.GrantInput{Amount: 100, Reason: "test:seed"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	queue := &recordingQueue{jobID: "job-recon-1"}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)

	jobID, err := svc.Simulate(ctx, "photo-1", "")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}
	bal, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance (after Simulate): %v", err)
	}
	if bal.Reserved != CreditsPerSimulation {
		t.Fatalf("Reserved after Simulate = %d, want %d", bal.Reserved, CreditsPerSimulation)
	}

	// The job finished -- recorded directly on the queue double, never
	// observed through NotifyOnCompletion (no poll ever happens in this
	// test).
	queue.setJob(&jobs.Job{ID: jobID, TenantID: "tenant-acme", Status: jobs.StatusSucceeded})

	settled, err := svc.ReconcileOutstandingCredits(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOutstandingCredits: %v", err)
	}
	if settled != 1 {
		t.Fatalf("settled = %d, want 1", settled)
	}

	bal, err = credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance (after reconcile): %v", err)
	}
	if bal.Available != 100-CreditsPerSimulation {
		t.Errorf("Available after reconcile = %d, want %d", bal.Available, 100-CreditsPerSimulation)
	}
	if bal.Reserved != 0 {
		t.Errorf("Reserved after reconcile = %d, want 0 -- the never-polled reservation must still be confirmed, not left stuck Reserved forever", bal.Reserved)
	}

	// The reservation row is gone now, so sweeping again finds nothing
	// left to settle.
	settledAgain, err := svc.ReconcileOutstandingCredits(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOutstandingCredits (second sweep): %v", err)
	}
	if settledAgain != 0 {
		t.Errorf("settled on second sweep = %d, want 0", settledAgain)
	}
}

// TestService_ReconcileOutstandingCredits_NonTerminalJob_LeavesReservationInPlace
// proves the sweep does not touch a job still in flight: settling a
// reservation before its job actually finishes would be a correctness bug
// of its own (an early Confirm on a job that later dead-letters, or an
// early Refund on one that later succeeds).
func TestService_ReconcileOutstandingCredits_NonTerminalJob_LeavesReservationInPlace(t *testing.T) {
	credits := newTestCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if _, err := credits.Grant(ctx, billing.GrantInput{Amount: 100, Reason: "test:seed"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	queue := &recordingQueue{jobID: "job-recon-2"}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)

	jobID, err := svc.Simulate(ctx, "photo-1", "")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}
	queue.setJob(&jobs.Job{ID: jobID, TenantID: "tenant-acme", Status: jobs.StatusRunning})

	settled, err := svc.ReconcileOutstandingCredits(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOutstandingCredits: %v", err)
	}
	if settled != 0 {
		t.Fatalf("settled = %d, want 0 for a still-running job", settled)
	}

	bal, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Reserved != CreditsPerSimulation {
		t.Errorf("Reserved after sweeping a non-terminal job = %d, want unchanged %d", bal.Reserved, CreditsPerSimulation)
	}
}

// TestService_ReconcileOutstandingCredits_NilWiring_IsANoOp mirrors every
// other optional-seam nil-safety test in this file: a Service missing
// credits, store or queue must not panic, and must settle nothing.
func TestService_ReconcileOutstandingCredits_NilWiring_IsANoOp(t *testing.T) {
	svc := NewService(nil, nil, nil, nil, nil, nil, nil)
	settled, err := svc.ReconcileOutstandingCredits(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOutstandingCredits: %v", err)
	}
	if settled != 0 {
		t.Errorf("settled = %d, want 0", settled)
	}
}

// TestService_CreditReservation_SurvivesRestart pins the durable mapping:
// the job-id-to-CreditTransaction-key mapping must not live in a plain Go
// map (creditKeys), which a process restart wipes --
// even a client that kept polling faithfully after a restart would find
// settleCredit's lookup come up empty and silently do nothing, leaking the
// reservation forever with no error and no distinguishing log line. This
// test builds a Service (serviceA), reserves credits through it, then
// builds a brand new, second Service instance (serviceB) sharing nothing
// in memory with serviceA -- no recipients, no notified, no in-process
// state of any kind -- but the SAME underlying database, exactly modeling
// a process restart in which only durable state (the database, and the
// jobs queue's own persisted rows, modeled here by reusing the same queue
// double) survives. serviceB alone is used to reconcile the reservation
// serviceA opened, proving the mapping itself -- not just the process that
// happened to create it -- is what makes settlement possible.
func TestService_CreditReservation_SurvivesRestart(t *testing.T) {
	credits := newTestCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if _, err := credits.Grant(ctx, billing.GrantInput{Amount: 100, Reason: "test:seed"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	db := dbtest.NewSQLite(t)
	queue := &recordingQueue{jobID: "job-restart-1"}
	serviceA := newTestServiceWithDB(t, db, &fakeImageProvider{}, queue, credits)

	jobID, err := serviceA.Simulate(ctx, "photo-1", "")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}

	// The "restart": a second Service, its own fresh in-memory state, over
	// the same database and the same (queue-double-modeled) persisted
	// queue -- gateway is nil because nothing here calls Simulate on
	// serviceB, only settlement.
	store := NewReservationStore(db)
	if schemaErr := store.EnsureSchema(context.Background()); schemaErr != nil {
		t.Fatalf("EnsureSchema: %v", schemaErr)
	}
	simulations := NewSimulationStore(db)
	if schemaErr := simulations.EnsureSchema(context.Background()); schemaErr != nil {
		t.Fatalf("EnsureSchema (simulation index): %v", schemaErr)
	}
	serviceB := NewService(nil, credits, pkgcore.NewMemoryEventBus(), queue, store, simulations, nil)

	queue.setJob(&jobs.Job{ID: jobID, TenantID: "tenant-acme", Status: jobs.StatusSucceeded})

	settled, err := serviceB.ReconcileOutstandingCredits(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOutstandingCredits: %v", err)
	}
	if settled != 1 {
		t.Fatalf("settled = %d, want 1 -- the reservation serviceA opened must still be visible to serviceB after the simulated restart", settled)
	}

	bal, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100-CreditsPerSimulation {
		t.Errorf("Available after restart-survived settlement = %d, want %d", bal.Available, 100-CreditsPerSimulation)
	}
	if bal.Reserved != 0 {
		t.Errorf("Reserved after restart-survived settlement = %d, want 0", bal.Reserved)
	}
}

// TestService_StartReconciler_AutomaticallySettlesWithoutAnyPoll proves
// StartReconciler's background ticker loop actually runs
// ReconcileOutstandingCredits on its own, with no test code ever calling
// NotifyOnCompletion or ReconcileOutstandingCredits directly -- the full,
// end-to-end shape of "settlement is reachable independent of any client
// polling".
func TestService_StartReconciler_AutomaticallySettlesWithoutAnyPoll(t *testing.T) {
	credits := newTestCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if _, err := credits.Grant(ctx, billing.GrantInput{Amount: 100, Reason: "test:seed"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	queue := &recordingQueue{jobID: "job-ticker-1"}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)

	jobID, err := svc.Simulate(ctx, "photo-1", "")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}
	queue.setJob(&jobs.Job{ID: jobID, TenantID: "tenant-acme", Status: jobs.StatusSucceeded})

	stop := svc.StartReconciler(context.Background(), 10*time.Millisecond)
	defer stop()

	deadline := time.Now().Add(2 * time.Second)
	for {
		bal, balErr := credits.Balance(ctx)
		if balErr != nil {
			t.Fatalf("Balance: %v", balErr)
		}
		if bal.Reserved == 0 {
			if bal.Available != 100-CreditsPerSimulation {
				t.Fatalf("Available once settled = %d, want %d", bal.Available, 100-CreditsPerSimulation)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("reservation still Reserved after waiting for the background reconciler to tick -- balance = %+v", bal)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestService_StartReconciler_NilWiring_ReturnsAHarmlessStop mirrors every
// other optional-seam nil-safety test in this file: calling
// StartReconciler on a Service with nothing wired must not panic, and its
// returned stop func must be safe to call.
func TestService_StartReconciler_NilWiring_ReturnsAHarmlessStop(t *testing.T) {
	svc := NewService(nil, nil, nil, nil, nil, nil, nil)
	stop := svc.StartReconciler(context.Background(), time.Millisecond)
	stop()
}

// The tests below pin Service's credit settlement and
// completion-notification bookkeeping under disconnects and delivery
// failures: respectively, that a
// refused completion-event publish leaves the notification retryable by a
// later poll instead of latching it as delivered; that Simulate's
// compensating refund of a failed enqueue survives a canceled request
// context, and that a refund which still cannot run is durably recorded
// as an orphaned reservation for ReconcileOutstandingCredits to refund;
// that Simulate's two post-enqueue persistence writes -- the
// credit-reservation row and the per-photo index row -- likewise survive a
// request context canceled after the enqueue, so the enqueued job stays
// settleable and enumerable; and that a Service assembled with a
// CreditService but no ReservationStore reserves nothing at all, since a
// reservation no settlement path could ever act on must never be opened.
//
// Two further regressions follow: that
// NotifyOnCompletion's settlement of a terminal job commits even when the
// poll request's context is already canceled -- the poll-tab-disconnect
// shape of the poll-driven settlement path, the twin of Simulate's own
// post-enqueue writes; and that a Service assembled with a
// CreditService and a ReservationStore but no jobs.Queue reserves nothing
// at all, since the reconciliation sweep -- the net beneath the poll
// path -- is a permanent no-op without the queue, leaving such a
// reservation settleable only by a client that keeps polling forever.
//
// The canceled-context shapes are driven deterministically through
// recordingQueue's onEnqueue hook (see that type's doc comment in
// service_test.go): the hook fires at the exact point of the real
// gateway's enqueue call -- after PreDeduct has committed, before
// Simulate's own post-enqueue writes -- which is the one interleaving a
// real client disconnect can produce that a synchronous Simulate call
// could not otherwise expose; NotifyOnCompletion's own canceled-context
// regression cancels before the call, the deterministic shape of a
// disconnect that beats the terminal poll's settlement to the store. The
// tests run against the real billing CreditService over a real SQLite
// file, so a "refund still executes on a canceled context" pass means a
// real balance moved.

// grantTestCredits grants 100 credits to tenant-acme on credits, failing
// the test when the grant cannot land -- the shared seed this file's
// balance-moving tests all start from.
func grantTestCredits(t *testing.T, credits *billing.CreditService) {
	t.Helper()
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if _, err := credits.Grant(ctx, billing.GrantInput{Amount: 100, Reason: "test:seed"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
}

// TestService_Simulate_EnqueueFailureOnCanceledRequest_StillRefunds pins
// that Simulate's compensating refund of a failed enqueue does not run on
// the request context itself: a client that disconnects mid-request (the
// context canceled -- driven here at the enqueue point, after the
// reservation has already committed) must not make the refund fail with
// context canceled, leaving the reservation Reserved with no durable
// record anywhere and no path back. The refund runs on a cancel-free
// derivation of the request context and releases the credits regardless
// of what happened to the caller.
func TestService_Simulate_EnqueueFailureOnCanceledRequest_StillRefunds(t *testing.T) {
	credits := newTestCreditService(t)
	grantTestCredits(t, credits)

	requestCtx, cancel := context.WithCancel(pkgcore.WithTenant(context.Background(), "tenant-acme"))
	queue := &recordingQueue{jobID: "job-should-never-exist"}
	queue.enqueueErr = errors.New("enqueue exploded")
	queue.onEnqueue = cancel // the client disconnects at the enqueue point
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)

	if _, err := svc.Simulate(requestCtx, "photo-1", ""); err == nil {
		t.Fatal("Simulate with an enqueue failure succeeded, want an error")
	}
	if queue.enqueueCalls != 1 {
		t.Fatalf("queue.enqueueCalls = %d, want 1", queue.enqueueCalls)
	}

	// Balance queries need a live context: the canceled one belonged to
	// the Simulate call alone -- exactly the production shape, where the
	// request context died but the process and its stores did not.
	bal, err := credits.Balance(pkgcore.WithTenant(context.Background(), "tenant-acme"))
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100 {
		t.Errorf("Available after the failed enqueue = %d, want 100 -- the compensating refund must have released the reservation", bal.Available)
	}
	if bal.Reserved != 0 {
		t.Errorf("Reserved after the failed enqueue = %d, want 0 -- a canceled request context must not kill the compensating refund", bal.Reserved)
	}
}

// TestService_Simulate_CanceledAfterEnqueue_PersistenceWritesStillLand
// pins that Simulate's two post-enqueue
// persistence writes -- the credit-reservation row and the per-photo
// index row -- do not run on the request context itself: a request
// context canceled after the enqueue succeeded (the client closed its
// tab the moment the job was accepted) must not fail both writes,
// leaving the enqueued job to settle against no durable reservation (its
// credits stuck Reserved forever, since neither NotifyOnCompletion nor
// the sweep would have a row to act on) and the per-photo index to lose
// the generation. Both writes run on a cancel-free derivation of the
// request context and land
// regardless of what happened to the caller.
func TestService_Simulate_CanceledAfterEnqueue_PersistenceWritesStillLand(t *testing.T) {
	credits := newTestCreditService(t)
	grantTestCredits(t, credits)
	liveCtx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	requestCtx, cancel := context.WithCancel(liveCtx)
	queue := &recordingQueue{jobID: "job-canceled-after-enqueue"}
	queue.onEnqueue = cancel // the client disconnects right after the enqueue commits
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)

	jobID, err := svc.Simulate(requestCtx, "photo-1", "")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}
	if jobID != queue.jobID {
		t.Fatalf("Simulate returned job id %q, want the queue's %q", jobID, queue.jobID)
	}

	// The credit-reservation row landed, so the reservation is settleable.
	if _, has, getErr := svc.store.get(liveCtx, jobID); getErr != nil {
		t.Fatalf("ReservationStore.get: %v", getErr)
	} else if !has {
		t.Fatal("no credit reservation row for the just-enqueued job -- the post-enqueue reservation write must not fail on the canceled request context")
	}

	// The per-photo index row landed, so the generation is enumerable.
	queue.setJob(newSimulateResultJob(t, jobID, "tenant-acme", jobs.StatusSucceeded, "object-out-1"))
	sims, err := svc.ListSimulationsByPhoto(liveCtx, "photo-1")
	if err != nil {
		t.Fatalf("ListSimulationsByPhoto: %v", err)
	}
	if len(sims) != 1 {
		t.Fatalf("per-photo listing after the enqueue returned %d entries, want 1 -- the post-enqueue index write must not fail on the canceled request context", len(sims))
	}

	// And the reservation genuinely settles: the durable row is what
	// settleCredit acts on, so a terminal job confirms it.
	if notifyErr := svc.NotifyOnCompletion(liveCtx, newSimulateResultJob(t, jobID, "tenant-acme", jobs.StatusSucceeded, "object-out-1")); notifyErr != nil {
		t.Fatalf("NotifyOnCompletion: %v", notifyErr)
	}
	bal, err := credits.Balance(liveCtx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100-CreditsPerSimulation {
		t.Errorf("Available after settling = %d, want %d", bal.Available, 100-CreditsPerSimulation)
	}
	if bal.Reserved != 0 {
		t.Errorf("Reserved after settling = %d, want 0 -- the just-enqueued job's reservation must be confirmable, not stuck Reserved", bal.Reserved)
	}
}

// TestService_NotifyOnCompletion_RefusedPublish_LeavesNotificationRetryable
// pins that NotifyOnCompletion sets its
// "already notified" latch only AFTER the publish is accepted: a refused
// publish (a subscriber failure) must not leave the latch set -- this
// method's only caller (internal/app's job-status route) logs and swallows
// the error, so a latched refusal would drop the notification silently
// and permanently: every later poll would read the latch and skip. The
// latch records an ACCEPTED delivery only: a refused publish is rolled
// back, so the next poll retries the delivery, while an accepted delivery
// stays latched against later polls exactly once.
func TestService_NotifyOnCompletion_RefusedPublish_LeavesNotificationRetryable(t *testing.T) {
	bus := pkgcore.NewMemoryEventBus()
	svc := NewService(nil, nil, bus, nil, nil, nil, nil)

	const jobID = jobs.JobID("job-publish-refused")
	svc.recipients[jobID] = "user-7"
	job := newSimulateResultJob(t, jobID, "tenant-acme", jobs.StatusSucceeded, "object-out-1")
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")

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

	if err := svc.NotifyOnCompletion(ctx, job); err == nil {
		t.Fatal("NotifyOnCompletion with a refused publish succeeded, want its error")
	}
	if delivered != 0 {
		t.Fatalf("delivered %d events on the refused publish, want 0", delivered)
	}

	// The very next poll -- the caller's retry -- must re-attempt the
	// delivery rather than skipping on a stale latch.
	if err := svc.NotifyOnCompletion(ctx, job); err != nil {
		t.Fatalf("NotifyOnCompletion (retrying poll): %v", err)
	}
	if delivered != 1 {
		t.Errorf("delivered = %d after the retrying poll, want 1 -- a refused publish must leave the notification retryable by the poll path", delivered)
	}

	// A third poll must not deliver again: the accepted delivery stayed
	// latched.
	if err := svc.NotifyOnCompletion(ctx, job); err != nil {
		t.Fatalf("NotifyOnCompletion (third poll): %v", err)
	}
	if delivered != 1 {
		t.Errorf("delivered = %d after the third poll, want still 1 -- an accepted delivery must remain latched", delivered)
	}
}

// TestService_Simulate_RefundFailure_RecordsOrphanedReservationForTheSweep
// pins the healing half: when Simulate's compensating refund
// of a failed enqueue cannot run even on its cancel-free context (here the
// billing database itself is closed at the enqueue point -- a genuine
// store failure, not a client disconnect), the reservation must not be
// left with its only record in a log line. Simulate durably records it
// as an orphaned reservation (orphanRefundJobIDPrefix in
// reservation_store.go) so ReconcileOutstandingCredits' sweep can refund
// it on a later pass; without the record no mechanism could ever release
// the Reserved credits.
func TestService_Simulate_RefundFailure_RecordsOrphanedReservationForTheSweep(t *testing.T) {
	billingDB := dbtest.NewSQLite(t)
	credits := newTestCreditServiceWithDB(t, billingDB)
	grantTestCredits(t, credits)
	liveCtx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	raw, err := billingDB.DB()
	if err != nil {
		t.Fatalf("underlying sql.DB: %v", err)
	}
	queue := &recordingQueue{jobID: "job-should-never-exist"}
	queue.enqueueErr = errors.New("enqueue exploded")
	// The billing store goes down at the enqueue point: PreDeduct above
	// has already committed against it, and every later billing call
	// fails.
	queue.onEnqueue = func() { _ = raw.Close() }
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)

	if _, simulateErr := svc.Simulate(liveCtx, "photo-1", ""); simulateErr == nil {
		t.Fatal("Simulate with an enqueue failure succeeded, want an error")
	}

	rows, err := svc.store.listAll(liveCtx)
	if err != nil {
		t.Fatalf("ReservationStore.listAll: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("reservation rows after the failed refund = %d, want exactly 1 orphaned reservation -- the refund failure must be durably recorded for the reconciliation sweep", len(rows))
	}
	row := rows[0]
	if !strings.HasPrefix(row.JobID, orphanRefundJobIDPrefix) {
		t.Errorf("recorded job id = %q, want one under the %q orphan prefix", row.JobID, orphanRefundJobIDPrefix)
	}
	if row.TenantID != "tenant-acme" {
		t.Errorf("recorded tenant = %q, want tenant-acme", row.TenantID)
	}
	if row.CreditKey == "" {
		t.Error("recorded credit key is empty -- the sweep needs it to refund the reservation")
	}
}

// TestService_ReconcileOutstandingCredits_RefundsOrphanedReservation is
// the sweep half of the same healing mechanism: an orphaned reservation
// on file (the durable state Simulate's refund-failure path records) is
// refunded by ReconcileOutstandingCredits without any job to poll -- the
// sweep recognizes the orphanRefundJobIDPrefix, refunds the row's stored
// credit key directly under CreditService's idempotent-refund contract and
// removes the row. Before the fix the sweep had no orphan concept at all:
// a row whose job id is synthetic was looked up on the queue like any
// other, so an orphaned reservation could never be settled (a real queue
// answers "not found", and this file's queue double answers a nil job the
// sweep then dereferences) and the Reserved credits were unreachable.
func TestService_ReconcileOutstandingCredits_RefundsOrphanedReservation(t *testing.T) {
	credits := newTestCreditService(t)
	grantTestCredits(t, credits)
	liveCtx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	svc := newTestService(t, &fakeImageProvider{}, &recordingQueue{jobID: "unused"}, credits)

	// A genuine reservation whose immediate refund could not run: opened
	// through the real CreditService (so the refund has a real pending
	// transaction to act on), then recorded as an orphan.
	const creditKey = "smilesim:orphan-seed-key"
	if _, err := credits.PreDeduct(liveCtx, billing.PreDeductInput{
		Amount:         CreditsPerSimulation,
		IdempotencyKey: creditKey,
		Reason:         "test:orphan-seed",
	}); err != nil {
		t.Fatalf("PreDeduct: %v", err)
	}
	orphanJobID := jobs.JobID(orphanRefundJobIDPrefix + creditKey)
	if err := svc.store.save(liveCtx, orphanJobID, "tenant-acme", creditKey); err != nil {
		t.Fatalf("seed orphaned reservation row: %v", err)
	}

	settled, err := svc.ReconcileOutstandingCredits(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOutstandingCredits: %v", err)
	}
	if settled != 1 {
		t.Fatalf("settled = %d, want 1 -- the sweep must refund the orphaned reservation", settled)
	}

	bal, err := credits.Balance(liveCtx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100 {
		t.Errorf("Available after the sweep = %d, want 100 -- the orphaned reservation's credits must return to Available", bal.Available)
	}
	if bal.Reserved != 0 {
		t.Errorf("Reserved after the sweep = %d, want 0", bal.Reserved)
	}

	rows, err := svc.store.listAll(liveCtx)
	if err != nil {
		t.Fatalf("ReservationStore.listAll: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("reservation rows after the sweep = %d, want 0 -- a refunded orphaned reservation must be removed like any settled one", len(rows))
	}
}

// TestService_Simulate_CreditServiceWithoutReservationStore_DoesNotReserve
// pins that Simulate does not reserve credits
// when only a CreditService is wired, without the durable
// store: settlement (settleCredit, reached from both
// NotifyOnCompletion's poll and the reconciliation sweep) requires the
// store's row before it acts on anything, so such a reservation could
// never be settled and would stay Reserved forever. A Service assembled
// with a CreditService but no store performs no reservation at all (the
// package doc's "a nil store ... performs no credit accounting at all"
// contract), leaving the request charge-free and nothing to settle later.
func TestService_Simulate_CreditServiceWithoutReservationStore_DoesNotReserve(t *testing.T) {
	credits := newTestCreditService(t)
	grantTestCredits(t, credits)
	liveCtx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	queue := &recordingQueue{jobID: "job-no-store"}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)
	svc.store = nil // a Service assembled with a CreditService but no ReservationStore

	jobID, err := svc.Simulate(liveCtx, "photo-1", "")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}
	if jobID != queue.jobID {
		t.Fatalf("Simulate returned job id %q, want the queue's %q", jobID, queue.jobID)
	}
	if queue.enqueueCalls != 1 {
		t.Fatalf("queue.enqueueCalls = %d, want 1", queue.enqueueCalls)
	}

	bal, err := credits.Balance(liveCtx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100 {
		t.Errorf("Available after Simulate = %d, want 100 -- a Service with a CreditService but no store must not reserve credits that no mechanism could ever settle", bal.Available)
	}
	if bal.Reserved != 0 {
		t.Errorf("Reserved after Simulate = %d, want 0", bal.Reserved)
	}

	// Nothing was reserved, so a terminal job settles nothing -- no error,
	// no balance move -- and the per-photo index still records the
	// generation (the index is independent of the credit pair).
	job := newSimulateResultJob(t, jobID, "tenant-acme", jobs.StatusSucceeded, "object-out-1")
	queue.setJob(job)
	if notifyErr := svc.NotifyOnCompletion(liveCtx, job); notifyErr != nil {
		t.Fatalf("NotifyOnCompletion: %v", notifyErr)
	}
	balAfter, err := credits.Balance(liveCtx)
	if err != nil {
		t.Fatalf("Balance (after terminal job): %v", err)
	}
	if *balAfter != *bal {
		t.Errorf("balance moved after a terminal job on a no-store Service: first %+v, after %+v", *bal, *balAfter)
	}
	sims, err := svc.ListSimulationsByPhoto(liveCtx, "photo-1")
	if err != nil {
		t.Fatalf("ListSimulationsByPhoto: %v", err)
	}
	if len(sims) != 1 {
		t.Errorf("per-photo listing returned %d entries, want 1 -- the generation must still be recorded", len(sims))
	}
}

// TestService_NotifyOnCompletion_CanceledRequestCtx_SettlementStillCommits
// pins that NotifyOnCompletion does not run
// its credit settlement on the poll request's own context: a client that
// closes its poll tab at the moment its poll observes the terminal
// status -- the context canceled, driven here before the call, the
// deterministic shape of that disconnect -- would fail the settlement's
// own store read (the transaction cannot begin on a done context) and
// roll the Confirm back: internal/app's job-status route logs and swallows
// NotifyOnCompletion's error, so the reservation would stay silently
// Reserved, healed only if some later poll or the reconciliation sweep
// happens to come by. The settlement runs on a cancel-free derivation of
// the request context, the identical context.WithoutCancel boundary
// Simulate draws around its own post-enqueue writes, and commits
// regardless of what happened to the caller.
func TestService_NotifyOnCompletion_CanceledRequestCtx_SettlementStillCommits(t *testing.T) {
	credits := newTestCreditService(t)
	grantTestCredits(t, credits)
	liveCtx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	queue := &recordingQueue{jobID: "job-terminal-poll-canceled"}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)

	jobID, err := svc.Simulate(liveCtx, "photo-1", "")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}

	// The client's poll connection is already gone when the terminal
	// status read settles -- the shape of a tab closed just before the
	// poll that would have observed the completed job.
	requestCtx, cancel := context.WithCancel(liveCtx)
	cancel()
	job := newSimulateResultJob(t, jobID, "tenant-acme", jobs.StatusSucceeded, "object-out-1")
	if notifyErr := svc.NotifyOnCompletion(requestCtx, job); notifyErr != nil {
		t.Fatalf("NotifyOnCompletion on a canceled request context: %v -- the settlement must not depend on the poller's connection staying alive", notifyErr)
	}

	bal, err := credits.Balance(liveCtx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100-CreditsPerSimulation {
		t.Errorf("Available after settling on a canceled context = %d, want %d -- the Confirm must have committed", bal.Available, 100-CreditsPerSimulation)
	}
	if bal.Reserved != 0 {
		t.Errorf("Reserved after settling on a canceled context = %d, want 0 -- a canceled poll request must not leave the reservation stuck Reserved", bal.Reserved)
	}
}

// TestService_Simulate_CreditServiceAndStoreWithoutQueue_DoesNotReserve
// pins that Simulate's reservation guard requires the jobs.Queue too, not
// only the CreditService and the durable store: a Service assembled with
// both but no jobs.Queue would debit the tenant while the reconciliation
// sweep that heals a reservation no client ever polls to completion is a
// permanent no-op without the queue (it can ask nothing about any job's
// status, and StartReconciler refuses to even start). A debit whose only
// settlement net cannot run is exactly the never-settleable reservation
// the store-less guard refuses, and the package doc promises "a nil
// jobs.Queue performs no credit accounting at all": the queue is
// required by the same guard, so a Service that debits always keeps its
// safety net reachable.
func TestService_Simulate_CreditServiceAndStoreWithoutQueue_DoesNotReserve(t *testing.T) {
	credits := newTestCreditService(t)
	grantTestCredits(t, credits)
	liveCtx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	queue := &recordingQueue{jobID: "job-no-queue"}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)
	svc.queue = nil // a Service assembled with a CreditService and a store but no jobs.Queue

	jobID, err := svc.Simulate(liveCtx, "photo-1", "")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}
	if jobID != queue.jobID {
		t.Fatalf("Simulate returned job id %q, want the queue's %q", jobID, queue.jobID)
	}
	if queue.enqueueCalls != 1 {
		t.Fatalf("queue.enqueueCalls = %d, want 1 -- the generation itself is unaffected by the credit pair", queue.enqueueCalls)
	}

	bal, err := credits.Balance(liveCtx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100 {
		t.Errorf("Available after Simulate = %d, want 100 -- a Service with no queue must not reserve credits that no reconciliation sweep could ever settle", bal.Available)
	}
	if bal.Reserved != 0 {
		t.Errorf("Reserved after Simulate = %d, want 0", bal.Reserved)
	}

	// Nothing was reserved, so a terminal job settles nothing -- no error,
	// no balance move -- and the per-photo index still records the
	// generation (the index is independent of the credit pair; the
	// enumeration itself is a queue-less no-op by design, so the durable
	// row is checked directly).
	job := newSimulateResultJob(t, jobID, "tenant-acme", jobs.StatusSucceeded, "object-out-1")
	queue.setJob(job)
	if notifyErr := svc.NotifyOnCompletion(liveCtx, job); notifyErr != nil {
		t.Fatalf("NotifyOnCompletion: %v", notifyErr)
	}
	balAfter, err := credits.Balance(liveCtx)
	if err != nil {
		t.Fatalf("Balance (after terminal job): %v", err)
	}
	if *balAfter != *bal {
		t.Errorf("balance moved after a terminal job on a queue-less Service: first %+v, after %+v", *bal, *balAfter)
	}
	rows, err := svc.simulations.listByPhoto(liveCtx, "photo-1")
	if err != nil {
		t.Fatalf("listByPhoto: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("index rows for the photo = %d, want 1 -- the generation must still be recorded", len(rows))
	}
}
