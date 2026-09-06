package billing

import (
	"context"
	"sync"
	"testing"

	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
)

func newCreditService(t *testing.T) *CreditService {
	t.Helper()
	return NewCreditService(newTestDB(t))
}

// newAuditedCreditService returns a CreditService wired with a real
// pkgcore.MemoryEventBus and a pkgcore.AuditActionRegistrar carrying this
// module's five declared credit audit actions -- exactly what module.go's
// Register wires onto m.credits at Bootstrap time (module_test.go's own
// TestModule_Register_PerformsNoIO builds the identical Registry shape for
// Register itself), reached here directly by setting the two unexported
// fields since this test file lives in package billing. received collects
// every audit.EventRecorded event the bus delivers, in publish order.
func newAuditedCreditService(t *testing.T) (svc *CreditService, received chan pkgcore.Event) {
	t.Helper()
	bus := pkgcore.NewMemoryEventBus()
	reg := pkgcore.NewRegistry(bus, pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := reg.AuditActions.Add(
		AuditActionCreditGrant,
		AuditActionCreditDeductReserve,
		AuditActionCreditDeductConfirm,
		AuditActionCreditRefund,
		AuditActionCreditExpire,
	); err != nil {
		t.Fatalf("declare this module's audit actions: %v", err)
	}

	events := make(chan pkgcore.Event, 8)
	bus.Subscribe(audit.EventRecorded, func(_ context.Context, evt pkgcore.Event) error {
		events <- evt
		return nil
	})

	svc = NewCreditService(newTestDB(t))
	svc.events = reg.EventBus()
	svc.auditActions = reg.AuditActions
	return svc, events
}

// recvAuditEvent drains one audit.RecordedEvent from received, failing the
// test if none arrives -- MemoryEventBus.Publish is synchronous (proved
// elsewhere in this package by TestSubscriptionService_Transition_
// PublishesEvent's identical non-blocking channel-read pattern), so by the
// time a CreditService method above has already returned, its own
// audit.Emit call has already run to completion one way or the other.
func recvAuditEvent(t *testing.T, received chan pkgcore.Event) audit.RecordedEvent {
	t.Helper()
	select {
	case evt := <-received:
		payload, ok := evt.Payload.(audit.RecordedEvent)
		if !ok {
			t.Fatalf("Payload type = %T, want audit.RecordedEvent", evt.Payload)
		}
		return payload
	default:
		t.Fatal("no audit.EventRecorded event was published")
		return audit.RecordedEvent{}
	}
}

// assertNoAuditEvent fails the test if any audit.EventRecorded event is
// waiting on received -- used to prove an idempotent no-op retry, or
// Balance's own read-only path, records nothing.
func assertNoAuditEvent(t *testing.T, received chan pkgcore.Event) {
	t.Helper()
	select {
	case evt := <-received:
		t.Fatalf("unexpected audit.EventRecorded event published: %+v", evt.Payload)
	default:
	}
}

func TestCreditService_Balance_MaterializesZeroBalance(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	bal, err := svc.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 0 || bal.Reserved != 0 {
		t.Errorf("Balance = %+v, want a fresh zero balance", bal)
	}

	// A second call must not fail on the already-materialized row.
	if _, err := svc.Balance(ctx); err != nil {
		t.Fatalf("second Balance call: %v", err)
	}
}

func TestCreditService_Grant_IncreasesAvailable(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	tx, err := svc.Grant(ctx, GrantInput{Amount: 100, Reason: "promo"})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if tx.Type != string(CreditTransactionGrant) || tx.Status != string(CreditTransactionStatusConfirmed) {
		t.Errorf("transaction = %+v, want Type=grant Status=confirmed", tx)
	}

	bal, err := svc.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100 {
		t.Errorf("Available = %d, want 100", bal.Available)
	}
}

func TestCreditService_Grant_NonPositiveAmount_Refused(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 0}); !hasCode(err, ErrInvalidAmount.Code) {
		t.Errorf("Grant(0): err = %v, want %s", err, ErrInvalidAmount.Code)
	}
	if _, err := svc.Grant(ctx, GrantInput{Amount: -1}); !hasCode(err, ErrInvalidAmount.Code) {
		t.Errorf("Grant(-1): err = %v, want %s", err, ErrInvalidAmount.Code)
	}
}

// TestCreditService_PreDeduct_ReservesAgainstAvailable proves the pure
// reserve half: Available drops by Amount, Reserved rises by Amount, and a
// pending CreditTransaction lands keyed by IdempotencyKey.
func TestCreditService_PreDeduct_ReservesAgainstAvailable(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 100}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	tx, err := svc.PreDeduct(ctx, PreDeductInput{Amount: 30, IdempotencyKey: "job-1", Reason: "ai_generation"})
	if err != nil {
		t.Fatalf("PreDeduct: %v", err)
	}
	if tx.Status != string(CreditTransactionStatusPending) {
		t.Errorf("Status = %q, want %q", tx.Status, CreditTransactionStatusPending)
	}

	bal, err := svc.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 70 || bal.Reserved != 30 {
		t.Errorf("balance = %+v, want Available=70 Reserved=30", bal)
	}
}

// TestCreditService_PreDeduct_InsufficientBalance_WritesNothing is the
// round's mandated proof that a refused reservation leaves no trace: the
// balance is unchanged and no CreditTransaction row exists for the
// attempted IdempotencyKey.
func TestCreditService_PreDeduct_InsufficientBalance_WritesNothing(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 10}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	_, err := svc.PreDeduct(ctx, PreDeductInput{Amount: 50, IdempotencyKey: "job-1"})
	if !hasCode(err, ErrInsufficientCredits.Code) {
		t.Fatalf("PreDeduct: err = %v, want %s", err, ErrInsufficientCredits.Code)
	}

	bal, err := svc.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 10 || bal.Reserved != 0 {
		t.Errorf("balance after a refused PreDeduct = %+v, want unchanged Available=10 Reserved=0", bal)
	}

	got, err := svc.transactions.Get(ctx, "job-1")
	if err != nil {
		t.Fatalf("transactions.Get: %v", err)
	}
	if got != nil {
		t.Errorf("a credit_transaction row exists for the refused attempt: %+v, want none", got)
	}
}

func TestCreditService_PreDeduct_IdempotentRetry_ReturnsTheSameReservation(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 100}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	first, err := svc.PreDeduct(ctx, PreDeductInput{Amount: 30, IdempotencyKey: "job-1"})
	if err != nil {
		t.Fatalf("first PreDeduct: %v", err)
	}
	second, err := svc.PreDeduct(ctx, PreDeductInput{Amount: 30, IdempotencyKey: "job-1"})
	if err != nil {
		t.Fatalf("retried PreDeduct: %v, want the same success", err)
	}
	if first.ID != second.ID || first.Status != second.Status {
		t.Errorf("retried PreDeduct = %+v, want the identical reservation %+v", second, first)
	}

	bal, err := svc.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 70 || bal.Reserved != 30 {
		t.Errorf("balance after a retried PreDeduct = %+v, want it reserved exactly once (Available=70 Reserved=30)", bal)
	}
}

func TestCreditService_PreDeduct_Validation(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.PreDeduct(ctx, PreDeductInput{Amount: 0, IdempotencyKey: "k"}); !hasCode(err, ErrInvalidAmount.Code) {
		t.Errorf("Amount=0: err = %v, want %s", err, ErrInvalidAmount.Code)
	}
	if _, err := svc.PreDeduct(ctx, PreDeductInput{Amount: 1, IdempotencyKey: ""}); !hasCode(err, ErrIdempotencyKeyRequired.Code) {
		t.Errorf("empty IdempotencyKey: err = %v, want %s", err, ErrIdempotencyKeyRequired.Code)
	}
}

func TestCreditService_Confirm_SettlesTheReservation(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 100}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if _, err := svc.PreDeduct(ctx, PreDeductInput{Amount: 30, IdempotencyKey: "job-1"}); err != nil {
		t.Fatalf("PreDeduct: %v", err)
	}

	tx, err := svc.Confirm(ctx, "job-1")
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if tx.Status != string(CreditTransactionStatusConfirmed) {
		t.Errorf("Status = %q, want %q", tx.Status, CreditTransactionStatusConfirmed)
	}

	bal, err := svc.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 70 || bal.Reserved != 0 {
		t.Errorf("balance after Confirm = %+v, want Available=70 Reserved=0 (the spend is now permanent)", bal)
	}
}

func TestCreditService_Confirm_IdempotentRetry_IsANoOpSuccess(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 100}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if _, err := svc.PreDeduct(ctx, PreDeductInput{Amount: 30, IdempotencyKey: "job-1"}); err != nil {
		t.Fatalf("PreDeduct: %v", err)
	}
	if _, err := svc.Confirm(ctx, "job-1"); err != nil {
		t.Fatalf("first Confirm: %v", err)
	}
	if _, err := svc.Confirm(ctx, "job-1"); err != nil {
		t.Fatalf("retried Confirm: %v, want a no-op success", err)
	}

	bal, err := svc.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 70 || bal.Reserved != 0 {
		t.Errorf("balance after a retried Confirm = %+v, want it applied exactly once (Available=70 Reserved=0)", bal)
	}
}

func TestCreditService_Refund_ReleasesTheReservation(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 100}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if _, err := svc.PreDeduct(ctx, PreDeductInput{Amount: 30, IdempotencyKey: "job-1"}); err != nil {
		t.Fatalf("PreDeduct: %v", err)
	}

	tx, err := svc.Refund(ctx, "job-1")
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if tx.Status != string(CreditTransactionStatusRefunded) {
		t.Errorf("Status = %q, want %q", tx.Status, CreditTransactionStatusRefunded)
	}

	bal, err := svc.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100 || bal.Reserved != 0 {
		t.Errorf("balance after Refund = %+v, want Available=100 Reserved=0 (fully restored)", bal)
	}
}

func TestCreditService_ConfirmThenRefund_IsRefused(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 100}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if _, err := svc.PreDeduct(ctx, PreDeductInput{Amount: 30, IdempotencyKey: "job-1"}); err != nil {
		t.Fatalf("PreDeduct: %v", err)
	}
	if _, err := svc.Confirm(ctx, "job-1"); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	_, err := svc.Refund(ctx, "job-1")
	if !hasCode(err, ErrCreditTransactionAlreadyResolved.Code) {
		t.Errorf("Refund after Confirm: err = %v, want %s", err, ErrCreditTransactionAlreadyResolved.Code)
	}
}

func TestCreditService_Confirm_UnknownIdempotencyKey_NotFound(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	_, err := svc.Confirm(ctx, "never-reserved")
	if !hasCode(err, ErrCreditTransactionNotFound.Code) {
		t.Errorf("Confirm(unknown key): err = %v, want %s", err, ErrCreditTransactionNotFound.Code)
	}
}

func TestCreditService_Expire_DecreasesAvailable(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 100}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	tx, err := svc.Expire(ctx, ExpireInput{Amount: 40, Reason: "expiry:2026-09"})
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if tx.Type != string(CreditTransactionExpire) {
		t.Errorf("Type = %q, want %q", tx.Type, CreditTransactionExpire)
	}

	bal, err := svc.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 60 {
		t.Errorf("Available = %d, want 60", bal.Available)
	}
}

func TestCreditService_Expire_MoreThanAvailable_Refused(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 10}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	_, err := svc.Expire(ctx, ExpireInput{Amount: 50})
	if !hasCode(err, ErrInsufficientCredits.Code) {
		t.Errorf("Expire(50) over Available=10: err = %v, want %s", err, ErrInsufficientCredits.Code)
	}
}

// TestCreditService_PreDeduct_ConcurrentOverBalance_OnlyOneSucceeds is the
// round's mandated proof: two concurrent PreDeduct calls whose combined
// Amount exceeds the tenant's balance cannot both succeed. Run under
// -race per this codebase's own concurrency-hot-spot testing requirement.
func TestCreditService_PreDeduct_ConcurrentOverBalance_OnlyOneSucceeds(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	const balance = 100
	const deductEach = 60 // two of these (120) exceed the 100 balance -- at most one may succeed.
	if _, err := svc.Grant(ctx, GrantInput{Amount: balance}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	var wg sync.WaitGroup
	results := make([]error, 2)
	keys := []string{"concurrent-a", "concurrent-b"}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.PreDeduct(ctx, PreDeductInput{Amount: deductEach, IdempotencyKey: keys[i]})
			results[i] = err
		}(i)
	}
	wg.Wait()

	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		} else if !hasCode(err, ErrInsufficientCredits.Code) {
			t.Errorf("unexpected error from a concurrent PreDeduct: %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("succeeded = %d of 2 concurrent over-balance PreDeduct calls, want exactly 1", succeeded)
	}

	bal, err := svc.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != balance-deductEach || bal.Reserved != deductEach {
		t.Errorf("balance after the race = %+v, want Available=%d Reserved=%d (exactly one reservation applied)", bal, balance-deductEach, deductEach)
	}
}

// TestCreditService_Grant_ConcurrentCallsForANewTenant_BothSucceed proves
// ensureBalance's own concurrency safety: two callers racing to touch a
// brand-new tenant's balance for the first time (via Grant, which calls
// ensureBalance internally) must not fail or lose either grant, however
// their two INSERT ... ON CONFLICT DO NOTHING attempts interleave.
func TestCreditService_Grant_ConcurrentCallsForANewTenant_BothSucceed(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-new")

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.Grant(ctx, GrantInput{Amount: 25, Reason: "concurrent-grant"})
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Grant %d: %v", i, err)
		}
	}

	bal, err := svc.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 50 {
		t.Errorf("Available = %d, want 50 (both grants applied)", bal.Available)
	}
}

// --- audit.Emit wiring ---
//
// The five tests below are this round's own mandated proof: each of
// CreditService's five state-changing methods calls audit.Emit with the
// exact action name module.go's Register declares, a real Resource naming
// the CreditTransaction the call itself produced, and a useful payload --
// never merely "Emit was called". Two further tests prove the idempotent
// no-op retry paths (PreDeduct, Confirm) do NOT produce a second audit
// record for a call that changed nothing, and Balance -- deliberately
// undeclared in Register -- never audits at all.

func TestCreditService_Grant_EmitsAuditEvent(t *testing.T) {
	svc, received := newAuditedCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	tx, err := svc.Grant(ctx, GrantInput{Amount: 100, Reason: "promo:welcome"})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}

	evt := recvAuditEvent(t, received)
	if evt.Action != AuditActionCreditGrant {
		t.Errorf("Action = %q, want %q", evt.Action, AuditActionCreditGrant)
	}
	if evt.TenantID != "tenant-a" {
		t.Errorf("TenantID = %q, want %q", evt.TenantID, "tenant-a")
	}
	if evt.Resource.Type != "credit_transaction" || evt.Resource.ID != tx.ID {
		t.Errorf("Resource = %+v, want Type=credit_transaction ID=%q", evt.Resource, tx.ID)
	}
	if !evt.Result.Success {
		t.Errorf("Result.Success = false, want true")
	}
	if evt.Changes == nil {
		t.Fatal("Changes is nil, want a populated After payload")
	}
	if evt.Changes.After["amount"] != int64(100) {
		t.Errorf("Changes.After[amount] = %v, want 100", evt.Changes.After["amount"])
	}
	if evt.Changes.After["reason"] != "promo:welcome" {
		t.Errorf("Changes.After[reason] = %v, want %q", evt.Changes.After["reason"], "promo:welcome")
	}
	if evt.Changes.After["resulting_available"] != int64(100) {
		t.Errorf("Changes.After[resulting_available] = %v, want 100", evt.Changes.After["resulting_available"])
	}
	assertNoAuditEvent(t, received)
}

func TestCreditService_PreDeduct_EmitsAuditEventUnderTheReserveAction(t *testing.T) {
	svc, received := newAuditedCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 100}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	recvAuditEvent(t, received) // drain the Grant's own audit event

	tx, err := svc.PreDeduct(ctx, PreDeductInput{Amount: 30, IdempotencyKey: "job-1", Reason: "ai_generation:job-1"})
	if err != nil {
		t.Fatalf("PreDeduct: %v", err)
	}

	evt := recvAuditEvent(t, received)
	if evt.Action != AuditActionCreditDeductReserve {
		t.Errorf("Action = %q, want %q", evt.Action, AuditActionCreditDeductReserve)
	}
	if evt.Resource.Type != "credit_transaction" || evt.Resource.ID != tx.ID {
		t.Errorf("Resource = %+v, want Type=credit_transaction ID=%q", evt.Resource, tx.ID)
	}
	if evt.Changes == nil || evt.Changes.After["amount"] != int64(30) {
		t.Errorf("Changes.After[amount] = %v, want 30", changesAfterOrNil(evt.Changes, "amount"))
	}
	if evt.Changes.After["resulting_available"] != int64(70) || evt.Changes.After["resulting_reserved"] != int64(30) {
		t.Errorf("Changes.After = %+v, want resulting_available=70 resulting_reserved=30", evt.Changes.After)
	}
	assertNoAuditEvent(t, received)
}

// TestCreditService_PreDeduct_IdempotentRetry_DoesNotEmitASecondAuditEvent
// is the negative half of the reserve-audit proof above: a retried
// PreDeduct with the same IdempotencyKey reserves nothing new (see
// TestCreditService_PreDeduct_IdempotentRetry_ReturnsTheSameReservation),
// so it must not produce a second audit.deduct_reserve record for an
// operation that did not happen this call.
func TestCreditService_PreDeduct_IdempotentRetry_DoesNotEmitASecondAuditEvent(t *testing.T) {
	svc, received := newAuditedCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 100}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	recvAuditEvent(t, received) // drain the Grant's own audit event

	if _, err := svc.PreDeduct(ctx, PreDeductInput{Amount: 30, IdempotencyKey: "job-1"}); err != nil {
		t.Fatalf("first PreDeduct: %v", err)
	}
	recvAuditEvent(t, received) // drain the first, genuine reservation's audit event

	if _, err := svc.PreDeduct(ctx, PreDeductInput{Amount: 30, IdempotencyKey: "job-1"}); err != nil {
		t.Fatalf("retried PreDeduct: %v", err)
	}
	assertNoAuditEvent(t, received)
}

func TestCreditService_Confirm_EmitsAuditEventUnderTheConfirmAction(t *testing.T) {
	svc, received := newAuditedCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 100}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	recvAuditEvent(t, received)
	if _, err := svc.PreDeduct(ctx, PreDeductInput{Amount: 30, IdempotencyKey: "job-1"}); err != nil {
		t.Fatalf("PreDeduct: %v", err)
	}
	recvAuditEvent(t, received)

	tx, err := svc.Confirm(ctx, "job-1")
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	evt := recvAuditEvent(t, received)
	if evt.Action != AuditActionCreditDeductConfirm {
		t.Errorf("Action = %q, want %q", evt.Action, AuditActionCreditDeductConfirm)
	}
	if evt.Resource.ID != tx.ID {
		t.Errorf("Resource.ID = %q, want %q", evt.Resource.ID, tx.ID)
	}
	if evt.Changes == nil || evt.Changes.After["amount"] != int64(30) {
		t.Errorf("Changes.After[amount] = %v, want 30", changesAfterOrNil(evt.Changes, "amount"))
	}
	// Confirm removes the 30 from Reserved permanently; Available stays 70
	// (it already left Available at PreDeduct time).
	if evt.Changes.After["resulting_available"] != int64(70) || evt.Changes.After["resulting_reserved"] != int64(0) {
		t.Errorf("Changes.After = %+v, want resulting_available=70 resulting_reserved=0", evt.Changes.After)
	}
	assertNoAuditEvent(t, received)
}

// TestCreditService_Confirm_IdempotentRetry_DoesNotEmitASecondAuditEvent
// mirrors PreDeduct's own idempotent-retry proof above: a second Confirm
// call for an already-confirmed idempotencyKey is a no-op success (see
// TestCreditService_Confirm_IdempotentRetry_IsANoOpSuccess) and must not
// audit a second time.
func TestCreditService_Confirm_IdempotentRetry_DoesNotEmitASecondAuditEvent(t *testing.T) {
	svc, received := newAuditedCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 100}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	recvAuditEvent(t, received)
	if _, err := svc.PreDeduct(ctx, PreDeductInput{Amount: 30, IdempotencyKey: "job-1"}); err != nil {
		t.Fatalf("PreDeduct: %v", err)
	}
	recvAuditEvent(t, received)
	if _, err := svc.Confirm(ctx, "job-1"); err != nil {
		t.Fatalf("first Confirm: %v", err)
	}
	recvAuditEvent(t, received)

	if _, err := svc.Confirm(ctx, "job-1"); err != nil {
		t.Fatalf("retried Confirm: %v", err)
	}
	assertNoAuditEvent(t, received)
}

func TestCreditService_Refund_EmitsAuditEventUnderTheRefundAction(t *testing.T) {
	svc, received := newAuditedCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 100}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	recvAuditEvent(t, received)
	if _, err := svc.PreDeduct(ctx, PreDeductInput{Amount: 30, IdempotencyKey: "job-1"}); err != nil {
		t.Fatalf("PreDeduct: %v", err)
	}
	recvAuditEvent(t, received)

	tx, err := svc.Refund(ctx, "job-1")
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}

	evt := recvAuditEvent(t, received)
	if evt.Action != AuditActionCreditRefund {
		t.Errorf("Action = %q, want %q", evt.Action, AuditActionCreditRefund)
	}
	if evt.Resource.ID != tx.ID {
		t.Errorf("Resource.ID = %q, want %q", evt.Resource.ID, tx.ID)
	}
	// Refund gives the 30 back to Available and clears Reserved.
	if evt.Changes.After["resulting_available"] != int64(100) || evt.Changes.After["resulting_reserved"] != int64(0) {
		t.Errorf("Changes.After = %+v, want resulting_available=100 resulting_reserved=0", evt.Changes.After)
	}
	assertNoAuditEvent(t, received)
}

func TestCreditService_Expire_EmitsAuditEvent(t *testing.T) {
	svc, received := newAuditedCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 100}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	recvAuditEvent(t, received)

	tx, err := svc.Expire(ctx, ExpireInput{Amount: 40, Reason: "expiry:2026-09-policy"})
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}

	evt := recvAuditEvent(t, received)
	if evt.Action != AuditActionCreditExpire {
		t.Errorf("Action = %q, want %q", evt.Action, AuditActionCreditExpire)
	}
	if evt.Resource.ID != tx.ID {
		t.Errorf("Resource.ID = %q, want %q", evt.Resource.ID, tx.ID)
	}
	if evt.Changes.After["amount"] != int64(40) || evt.Changes.After["reason"] != "expiry:2026-09-policy" {
		t.Errorf("Changes.After = %+v, want amount=40 reason=expiry:2026-09-policy", evt.Changes.After)
	}
	if evt.Changes.After["resulting_available"] != int64(60) {
		t.Errorf("Changes.After[resulting_available] = %v, want 60", evt.Changes.After["resulting_available"])
	}
	assertNoAuditEvent(t, received)
}

// TestCreditService_Balance_NeverEmitsAnAuditEvent confirms the read-only
// path stays entirely unaudited, matching Register's own declaration of
// exactly five state-changing actions -- never six.
func TestCreditService_Balance_NeverEmitsAnAuditEvent(t *testing.T) {
	svc, received := newAuditedCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Balance(ctx); err != nil {
		t.Fatalf("Balance: %v", err)
	}
	assertNoAuditEvent(t, received)
}

// TestCreditService_EmitCreditAudit_NoEventBusWired_IsANoOp proves a bare
// CreditService built directly through NewCreditService -- every other
// test in this file, and every pre-existing call site before this round --
// never attempts to call audit.Emit at all: s.events stays nil until
// module.go's Register wires it, exactly mirroring notes' own
// Handler.bus == nil short-circuit.
func TestCreditService_EmitCreditAudit_NoEventBusWired_IsANoOp(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 10}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	// No assertion beyond "this did not panic or block": svc.events is nil,
	// so emitCreditAudit's own guard returns immediately.
}

// changesAfterOrNil is a small nil-safe accessor used only where a test
// wants a readable failure message even when Changes itself turned out
// nil (which would otherwise panic on Changes.After[key]).
func changesAfterOrNil(c *audit.Diff, key string) any {
	if c == nil {
		return nil
	}
	return c.After[key]
}
