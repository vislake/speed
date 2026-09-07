// Disconnect- and delivery-robustness regressions for Service's credit
// settlement and completion-notification bookkeeping (reviewer findings
// P2-6, P2-7, P2-8 and P2-10): the file's tests pin, respectively, that a
// refused completion-event publish leaves the notification retryable by a
// later poll instead of latching it as delivered (P2-6); that Simulate's
// compensating refund of a failed enqueue survives a canceled request
// context, and that a refund which still cannot run is durably recorded
// as an orphaned reservation for ReconcileOutstandingCredits to refund
// (P2-7); that Simulate's two post-enqueue persistence writes -- the
// credit-reservation row and the per-photo index row -- likewise survive a
// request context canceled after the enqueue, so the enqueued job stays
// settleable and enumerable (P2-8); and that a Service assembled with a
// CreditService but no ReservationStore reserves nothing at all, since a
// reservation no settlement path could ever act on must never be opened
// (P2-10).
//
// The smilesim rescan round added two further regressions here: that
// NotifyOnCompletion's settlement of a terminal job commits even when the
// poll request's context is already canceled -- the poll-tab-disconnect
// shape of the poll-driven settlement path, the twin of Simulate's own
// post-enqueue writes (P1-refapp-5); and that a Service assembled with a
// CreditService and a ReservationStore but no jobs.Queue reserves nothing
// at all, since the reconciliation sweep -- the net beneath the poll
// path -- is a permanent no-op without the queue, leaving such a
// reservation settleable only by a client that keeps polling forever
// (P2-refapp-9).
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
package smilesim

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

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

// TestService_Simulate_EnqueueFailureOnCanceledRequest_StillRefunds is the
// P2-7 regression: before the fix, Simulate's compensating refund of a
// failed enqueue ran on the request context itself, so a client that
// disconnected mid-request (the context canceled -- driven here at the
// enqueue point, after the reservation has already committed) made the
// refund fail with context canceled and left the reservation Reserved
// with no durable record anywhere and no path back. The refund must run
// on a cancel-free derivation of the request context and release the
// credits regardless of what happened to the caller.
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

// TestService_Simulate_CanceledAfterEnqueue_PersistenceWritesStillLand is
// the P2-8 regression: before the fix, Simulate's two post-enqueue
// persistence writes -- the credit-reservation row and the per-photo
// index row -- ran on the request context itself, so a request context
// canceled after the enqueue succeeded (the client closed its tab the
// moment the job was accepted) failed both writes. The enqueued job then
// settled against no durable reservation (its credits stayed Reserved
// forever, since neither NotifyOnCompletion nor the sweep had a row to
// act on) and the per-photo index lost the generation. Both writes must
// run on a cancel-free derivation of the request context and land
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
// is the P2-6 regression: before the fix, NotifyOnCompletion set its
// "already notified" latch before attempting the publish, so a refused
// publish (a subscriber failure) left the latch set -- and since this
// method's only caller (cmd/server's job-status route) logs and swallows
// the error, the notification was dropped silently and permanently: every
// later poll read the latch and skipped. The latch must record an
// ACCEPTED delivery only: a refused publish is rolled back, so the next
// poll retries the delivery, while an accepted delivery stays latched
// against later polls exactly once.
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
// is the P2-7 healing-half regression: when Simulate's compensating refund
// of a failed enqueue cannot run even on its cancel-free context (here the
// billing database itself is closed at the enqueue point -- a genuine
// store failure, not a client disconnect), the reservation must not be
// left with its only record in a log line. Simulate must durably record
// it as an orphaned reservation (orphanRefundJobIDPrefix in
// reservation_store.go) so ReconcileOutstandingCredits' sweep can refund
// it on a later pass; before the fix no such record was written and no
// mechanism could ever release the Reserved credits.
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
// is the P2-10 regression: before the fix, Simulate reserved credits
// whenever a CreditService was wired, regardless of whether the durable
// store was -- but settlement (settleCredit, reached from both
// NotifyOnCompletion's poll and the reconciliation sweep) requires the
// store's row before it acts on anything, so such a reservation could
// never be settled and stayed Reserved forever. A Service assembled with
// a CreditService but no store must perform no reservation at all (the
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
// is the P1-refapp-5 regression: before the fix, NotifyOnCompletion ran
// its credit settlement on the poll request's own context, so a client
// that closed its poll tab at the moment its poll observed the terminal
// status -- the context canceled, driven here before the call, the
// deterministic shape of that disconnect -- failed the settlement's own
// store read (the transaction cannot begin on a done context) and rolled
// the Confirm back: cmd/server's job-status route logs and swallows
// NotifyOnCompletion's error, so the reservation stayed silently Reserved,
// healed only if some later poll or the reconciliation sweep happened to
// come by. The settlement must run on a cancel-free derivation of the
// request context, the identical context.WithoutCancel boundary Simulate
// draws around its own post-enqueue writes, and commit regardless of what
// happened to the caller.
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

// TestService_Simulate_CreditServiceAndStoreWithoutQueue_DoesNotReserve is
// the P2-refapp-9 regression: before the fix, Simulate's reservation guard
// required only the CreditService and the durable store, so a Service
// assembled with both but no jobs.Queue still debited the tenant -- while
// the reconciliation sweep that heals a reservation no client ever polls
// to completion is a permanent no-op without the queue (it can ask
// nothing about any job's status, and StartReconciler refuses to even
// start). A debit whose only settlement net cannot run is exactly the
// never-settleable reservation the P2-10 store-less guard refuses, and
// the package doc already promises "a nil jobs.Queue performs no credit
// accounting at all": the queue must be required by the same guard, so a
// Service that debits always keeps its safety net reachable. This is the
// P2-10 test's own shape applied to the third missing piece.
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
