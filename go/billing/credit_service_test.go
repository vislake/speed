package billing

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/componenttest"
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
	reg := componenttest.NewRegistry()
	reg.Put(bus)
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
// waiting on received -- proves an idempotent no-op retry, or
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

	if _, err := svc.Grant(ctx, GrantInput{Amount: 0}); !apperr.HasCode(err, ErrInvalidAmount.Code) {
		t.Errorf("Grant(0): err = %v, want %s", err, ErrInvalidAmount.Code)
	}
	if _, err := svc.Grant(ctx, GrantInput{Amount: -1}); !apperr.HasCode(err, ErrInvalidAmount.Code) {
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

// TestCreditService_PreDeduct_InsufficientBalance_WritesNothing pins the
// no-trace refusal contract: a reservation the balance guard refuses
// leaves the balance unchanged and no CreditTransaction row exists for
// the attempted IdempotencyKey.
func TestCreditService_PreDeduct_InsufficientBalance_WritesNothing(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 10}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	_, err := svc.PreDeduct(ctx, PreDeductInput{Amount: 50, IdempotencyKey: "job-1"})
	if !apperr.HasCode(err, ErrInsufficientCredits.Code) {
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

	if _, err := svc.PreDeduct(ctx, PreDeductInput{Amount: 0, IdempotencyKey: "k"}); !apperr.HasCode(err, ErrInvalidAmount.Code) {
		t.Errorf("Amount=0: err = %v, want %s", err, ErrInvalidAmount.Code)
	}
	if _, err := svc.PreDeduct(ctx, PreDeductInput{Amount: 1, IdempotencyKey: ""}); !apperr.HasCode(err, ErrIdempotencyKeyRequired.Code) {
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
	if !apperr.HasCode(err, ErrCreditTransactionAlreadyResolved.Code) {
		t.Errorf("Refund after Confirm: err = %v, want %s", err, ErrCreditTransactionAlreadyResolved.Code)
	}
}

func TestCreditService_Confirm_UnknownIdempotencyKey_NotFound(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	_, err := svc.Confirm(ctx, "never-reserved")
	if !apperr.HasCode(err, ErrCreditTransactionNotFound.Code) {
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
	if !apperr.HasCode(err, ErrInsufficientCredits.Code) {
		t.Errorf("Expire(50) over Available=10: err = %v, want %s", err, ErrInsufficientCredits.Code)
	}
}

// TestCreditService_Expire_UnkeyedRetry_DoubleApplies pins the unkeyed
// Expire mode's behavior: two unkeyed Expire calls with the same amount
// and reason -- a scheduler that crashed after its first attempt
// committed, retrying without a key -- deduct twice and append two ledger
// rows. This is the hazard the keyed form (next tests) exists to close,
// deliberately pinned here so the contrast between the two modes stays
// explicit: unkeyed is the one-off operator shape, never the shape a
// retrying caller may use.
func TestCreditService_Expire_UnkeyedRetry_DoubleApplies(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 100}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if _, err := svc.Expire(ctx, ExpireInput{Amount: 40, Reason: "expiry:2026-09-policy"}); err != nil {
		t.Fatalf("first unkeyed Expire: %v", err)
	}
	if _, err := svc.Expire(ctx, ExpireInput{Amount: 40, Reason: "expiry:2026-09-policy"}); err != nil {
		t.Fatalf("retried unkeyed Expire: %v", err)
	}

	bal, err := svc.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 20 {
		t.Errorf("Available = %d, want 20 -- an unkeyed retry deducted twice (the hazard the keyed form exists to close)", bal.Available)
	}
}

// TestCreditService_Expire_KeyedRetry_DoesNotDoubleApply pins the keyed
// scheduler contract: ExpireInput.IdempotencyKey makes a keyed Expire's
// row ID the caller's own deterministic per-window key (the go/storage
// EnqueueExpirySweep shape), so a retried call -- a jobs-driven sweep
// rerunning its own window after a crash or a timeout -- is answered with
// the first call's own row and applies NO second deduction. Without the
// key, a retrying sweep run would deduct twice (see the unkeyed pin
// above); the unkeyed API cannot express the contract, so this test
// cannot compile against it -- the compile failure is the point.
func TestCreditService_Expire_KeyedRetry_DoesNotDoubleApply(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 100}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	const key = "expiry:2026-09-policy:window-1"
	first, err := svc.Expire(ctx, ExpireInput{Amount: 40, IdempotencyKey: key, Reason: "expiry:2026-09-policy"})
	if err != nil {
		t.Fatalf("first keyed Expire: %v", err)
	}
	if first.ID != key {
		t.Errorf("keyed Expire row ID = %q, want the supplied key %q", first.ID, key)
	}
	if first.Type != string(CreditTransactionExpire) || first.Status != string(CreditTransactionStatusConfirmed) {
		t.Errorf("keyed Expire row = %+v, want Type=expire Status=confirmed", first)
	}

	// The retried sweep run: same window, same deterministic key.
	second, err := svc.Expire(ctx, ExpireInput{Amount: 40, IdempotencyKey: key, Reason: "expiry:2026-09-policy"})
	if err != nil {
		t.Fatalf("retried keyed Expire: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("retried keyed Expire returned row %q, want the first call's own row %q", second.ID, first.ID)
	}

	bal, err := svc.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 60 {
		t.Errorf("Available = %d, want 60 -- a retried keyed sweep run must not deduct twice", bal.Available)
	}

	rows, err := svc.Transactions(ctx)
	if err != nil {
		t.Fatalf("Transactions: %v", err)
	}
	expireRows := 0
	for _, row := range rows {
		if row.Type == string(CreditTransactionExpire) {
			expireRows++
		}
	}
	if expireRows != 1 {
		t.Errorf("expire ledger rows = %d, want exactly 1 -- the retry must not append a second row", expireRows)
	}
}

// TestCreditService_Expire_KeyedRetry_DoesNotEmitASecondAuditEvent is the
// audit half of the retry contract: the idempotent no-op retry changed
// nothing THIS call, so it must not produce a second audit record -- the
// identical "never write an audit record for something that did not
// actually happen this call" rule PreDeduct's own reserved-flag guard
// exists for: the keyed retry branch applies no deduction and must stay
// equally silent on the audit side.
func TestCreditService_Expire_KeyedRetry_DoesNotEmitASecondAuditEvent(t *testing.T) {
	svc, received := newAuditedCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 100}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	recvAuditEvent(t, received) // drain the grant's own event.

	const key = "expiry:2026-09-policy:window-1"
	first, err := svc.Expire(ctx, ExpireInput{Amount: 40, IdempotencyKey: key, Reason: "expiry:2026-09-policy"})
	if err != nil {
		t.Fatalf("first keyed Expire: %v", err)
	}

	evt := recvAuditEvent(t, received)
	if evt.Action != AuditActionCreditExpire {
		t.Errorf("Action = %q, want %q", evt.Action, AuditActionCreditExpire)
	}
	if evt.Resource.ID != key || evt.Resource.ID != first.ID {
		t.Errorf("Resource.ID = %q, want the keyed row %q", evt.Resource.ID, first.ID)
	}
	if evt.Changes.After["resulting_available"] != int64(60) {
		t.Errorf("Changes.After[resulting_available] = %v, want 60", evt.Changes.After["resulting_available"])
	}
	assertNoAuditEvent(t, received)

	// The retried sweep run with the same key must record nothing: the
	// deduction it would describe already happened in the first call.
	if _, err := svc.Expire(ctx, ExpireInput{Amount: 40, IdempotencyKey: key, Reason: "expiry:2026-09-policy"}); err != nil {
		t.Fatalf("retried keyed Expire: %v", err)
	}
	assertNoAuditEvent(t, received)
}

// TestCreditService_Expire_KeyCollidingWithAnotherKind_Refused proves the
// keyed retry branch distinguishes its own earlier run from a row of
// another kind sitting under the same key: the ledger's row-ID namespace
// is shared across every row type, and a key that names a Grant (or a
// Deduct, or an unkeyed row's UUID) must never be answered as a successful
// expiry -- reporting success on a row that is not an expiry record would
// be a wrong answer the audit trail would then preserve. Such a call is
// refused with ErrIdempotencyKeyCollision, and nothing is written.
func TestCreditService_Expire_KeyCollidingWithAnotherKind_Refused(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	grantTx, err := svc.Grant(ctx, GrantInput{Amount: 100})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}

	_, err = svc.Expire(ctx, ExpireInput{Amount: 40, IdempotencyKey: grantTx.ID})
	if !apperr.HasCode(err, ErrIdempotencyKeyCollision.Code) {
		t.Errorf("keyed Expire reusing a grant row's id: err = %v, want %s", err, ErrIdempotencyKeyCollision.Code)
	}

	bal, err := svc.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100 {
		t.Errorf("Available = %d, want 100 -- the refused call wrote nothing", bal.Available)
	}

	rows, err := svc.Transactions(ctx)
	if err != nil {
		t.Fatalf("Transactions: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("ledger rows = %d, want exactly 1 (the grant, untouched)", len(rows))
	}
}

// TestCreditService_Expire_KeyedRefusedAttempt_BurnsNoKey proves a keyed
// Expire whose balance guard refuses leaves no trace at all: the whole
// transaction rolls back, the just-inserted row included, so the key is
// never burned and a later, better-funded run with the same key -- the
// sweep's next window after the tenant tops up -- is a fresh attempt
// again, not a retry that would no-op against nothing.
func TestCreditService_Expire_KeyedRefusedAttempt_BurnsNoKey(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 10}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	const key = "expiry:2026-09-policy:window-1"
	if _, err := svc.Expire(ctx, ExpireInput{Amount: 50, IdempotencyKey: key}); !apperr.HasCode(err, ErrInsufficientCredits.Code) {
		t.Fatalf("Expire(50) over Available=10: err = %v, want %s", err, ErrInsufficientCredits.Code)
	}

	bal, err := svc.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 10 {
		t.Errorf("Available after the refused attempt = %d, want 10", bal.Available)
	}
	rows, err := svc.Transactions(ctx)
	if err != nil {
		t.Fatalf("Transactions: %v", err)
	}
	for _, row := range rows {
		if row.Type == string(CreditTransactionExpire) {
			t.Errorf("a refused keyed Expire left an expire row behind: %+v", row)
		}
	}

	// The tenant tops up; the same window's keyed run is a fresh attempt
	// and succeeds -- the refused attempt never burned the key.
	if _, grantErr := svc.Grant(ctx, GrantInput{Amount: 100}); grantErr != nil {
		t.Fatalf("second Grant: %v", grantErr)
	}
	expired, err := svc.Expire(ctx, ExpireInput{Amount: 50, IdempotencyKey: key})
	if err != nil {
		t.Fatalf("keyed Expire after the top-up: %v", err)
	}
	if expired.ID != key {
		t.Errorf("expire row ID = %q, want the supplied key %q", expired.ID, key)
	}
	bal, err = svc.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 60 {
		t.Errorf("Available = %d, want 60", bal.Available)
	}
}

// TestCreditService_Expire_ConcurrentSameKey_ExactlyOneDeducts is the
// concurrency proof of the keyed contract, run under -race: two goroutines
// expiring the same window with the same deterministic key -- a scheduler
// with two replicas, or a manual re-run overlapping a scheduled one --
// both succeed (the loser's retry branch returns the winner's row) while
// exactly ONE deduction lands and exactly ONE ledger row exists. The
// insert-first ON CONFLICT DO NOTHING shape makes the database itself the
// arbiter: the second writer blocks on the first's row, no-ops, and reads
// the winner's committed row back.
func TestCreditService_Expire_ConcurrentSameKey_ExactlyOneDeducts(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := svc.Grant(ctx, GrantInput{Amount: 100}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	const key = "expiry:2026-09-policy:window-1"
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.Expire(ctx, ExpireInput{Amount: 40, IdempotencyKey: key})
			results[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range results {
		if err != nil {
			t.Errorf("concurrent keyed Expire %d failed: %v", i, err)
		}
	}

	bal, err := svc.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 60 {
		t.Errorf("Available = %d, want 60 -- exactly one of the two same-key runs may deduct", bal.Available)
	}

	rows, err := svc.Transactions(ctx)
	if err != nil {
		t.Fatalf("Transactions: %v", err)
	}
	expireRows := 0
	for _, row := range rows {
		if row.Type == string(CreditTransactionExpire) {
			expireRows++
		}
	}
	if expireRows != 1 {
		t.Errorf("expire ledger rows = %d, want exactly 1", expireRows)
	}
}

// TestCreditService_PreDeduct_ConcurrentOverBalance_OnlyOneSucceeds pins
// the balance guard under concurrency: two concurrent PreDeduct calls
// whose combined Amount exceeds the tenant's balance cannot both succeed
// -- raced under -race so the database-arbitrated guard, not Go-level
// scheduling luck, is what lets exactly one through.
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
		} else if !apperr.HasCode(err, ErrInsufficientCredits.Code) {
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

// TestCreditService_Grant_ConcurrentGrantForSameTenant_BlocksUntilPriorTransactionCommits
// pins the resulting-balance read's correctness: the
// resulting_available/resulting_reserved audit fields are read from
// INSIDE the mutating transaction (credit_service.go's readBalanceForAudit
// reads the balance in the same transaction that applies the delta,
// before it commits), never by a separate post-commit query
// (s.balances.FindByID after the mutating dbkit.WithTenantSession
// transaction has committed) -- a post-commit read has no synchronization
// at all against a second, concurrently-committing operation for the SAME
// tenant. Two concurrent Grants (100 and 1 credits) racing for one tenant
// can interleave as: the 100-credit grant commits (Available 0->100),
// then the 1-credit grant commits (Available 100->101), then the
// 100-credit grant's own post-commit read finally runs and observes 101 --
// not the 100 its own operation actually produced -- so its audit event
// would wrongly record resulting_available=101 for an operation whose own
// effect was to move a starting balance of 0 to 100.
//
// This test proves the in-transaction read's load-bearing property
// directly rather than hoping real goroutine scheduling happens to hit
// the race's narrow window (which it is not guaranteed to on every run,
// making a pure racing test unreliable in either direction): using
// testHookAfterBalanceDelta, it pauses Grant A's transaction right after
// its own delta is applied -- the point where a separate post-commit read
// would still have no synchronization against B -- and starts a second,
// concurrent Grant B for the SAME tenant while A is paused there. If
// Grant B could commit during that window, the in-transaction read would
// offer no protection: this test's own next step -- reading back Grant
// A's audit event once both finish -- would observe
// resulting_available=101 for Grant A, the wrong-answer failure shape.
// B's write is provably still blocked by A's own not-yet-committed
// transaction throughout that window (asserted directly below via a
// timeout), so it can only land after A commits, and Grant A's own audit
// event deterministically shows resulting_available=100 -- its own delta
// alone -- every time.
func TestCreditService_Grant_ConcurrentGrantForSameTenant_BlocksUntilPriorTransactionCommits(t *testing.T) {
	svc, received := newAuditedCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-lock")

	const amountA int64 = 100
	const amountB int64 = 1

	reachedHook := make(chan struct{})
	proceed := make(chan struct{})
	var pauseOnce sync.Once
	svc.testHookAfterBalanceDelta = func() {
		// The hook fires for every Grant call against svc, including
		// Grant B's own once it finally acquires the row -- pause exactly
		// once, on the first (Grant A's) invocation, and let every later
		// one through immediately.
		pauseOnce.Do(func() {
			close(reachedHook)
			<-proceed
		})
	}

	grantAErr := make(chan error, 1)
	go func() {
		_, err := svc.Grant(ctx, GrantInput{Amount: amountA})
		grantAErr <- err
	}()

	select {
	case <-reachedHook:
	case <-time.After(2 * time.Second):
		t.Fatal("Grant A never reached testHookAfterBalanceDelta")
	}

	// Grant A's transaction has applied its own delta and is now paused,
	// still open (not committed). Start Grant B for the SAME tenant here:
	// its own applyBalanceDelta UPDATE cannot proceed until A's
	// transaction ends, which is exactly the property that makes reading
	// the resulting balance INSIDE the transaction (rather than via a
	// separate post-commit query) safe.
	grantBErr := make(chan error, 1)
	go func() {
		_, err := svc.Grant(ctx, GrantInput{Amount: amountB})
		grantBErr <- err
	}()

	select {
	case err := <-grantBErr:
		t.Fatalf("Grant B committed (err=%v) while Grant A's transaction was still open -- B should have been blocked by A's own not-yet-committed write, the exact property that makes reading the resulting balance inside the transaction safe", err)
	case <-time.After(200 * time.Millisecond):
		// Expected: B is still blocked waiting for A's transaction to end.
	}

	close(proceed) // let A finish its own read and commit.

	if err := <-grantAErr; err != nil {
		t.Fatalf("Grant A: %v", err)
	}
	if err := <-grantBErr; err != nil {
		t.Fatalf("Grant B: %v", err)
	}

	var availA, availB int64
	var sawA, sawB bool
	for i := 0; i < 2; i++ {
		evt := recvAuditEvent(t, received)
		amount, _ := evt.Changes.After["amount"].(int64)
		avail, _ := evt.Changes.After["resulting_available"].(int64)
		switch amount {
		case amountA:
			availA, sawA = avail, true
		case amountB:
			availB, sawB = avail, true
		default:
			t.Fatalf("unexpected audit event amount %v", evt.Changes.After["amount"])
		}
	}
	if !sawA || !sawB {
		t.Fatal("did not receive one audit event per concurrent Grant call")
	}

	if availA != amountA {
		t.Errorf("Grant A's Changes.After[resulting_available] = %d, want %d (its own delta alone -- it committed while B was still blocked)", availA, amountA)
	}
	if availB != amountA+amountB {
		t.Errorf("Grant B's Changes.After[resulting_available] = %d, want %d (both deltas -- it committed after A)", availB, amountA+amountB)
	}
}

// --- audit.Emit wiring ---
//
// The five tests below are the audit-wiring proof: each of CreditService's
// five state-changing methods calls audit.Emit with the exact action name
// module.go's Register declares, a real Resource naming the
// CreditTransaction the call itself produced, and a useful payload --
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
// test in this file constructs one that way -- never attempts to call
// audit.Emit at all: s.events stays nil until module.go's Register wires
// it, exactly mirroring notes' own Handler.bus == nil short-circuit.
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

// TestCreditService_Reason_NonPhraseRefused pins the declared bounded-phrase
// constraint on credit-operation reasons (validateReason): a reason that is
// not a bounded phrase of ASCII letters, digits and ':' '_' '-' separators,
// or that exceeds the ledger column's 255-character bound, is refused with
// ErrInvalidReason before anything is written. The constraint exists
// because the reason is copied verbatim into the audit trail's changes
// column (emitCreditAudit), and dbkit/audit's Diff content contract
// (go/dbkit/audit/emit.go) forbids free text there -- a reason carrying
// prose (an email address, a complaint) would be carved into the one table
// no code can ever delete from. Without the phrase gate, any string would
// be written into both the ledger row and the audit diff.
func TestCreditService_Reason_NonPhraseRefused(t *testing.T) {
	svc := newCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	calls := []struct {
		name string
		call func(reason string) error
	}{
		{"Grant", func(reason string) error {
			_, err := svc.Grant(ctx, GrantInput{Amount: 10, Reason: reason})
			return err
		}},
		{"PreDeduct", func(reason string) error {
			_, err := svc.PreDeduct(ctx, PreDeductInput{Amount: 10, IdempotencyKey: "reserve-reason-test", Reason: reason})
			return err
		}},
		{"Expire", func(reason string) error {
			_, err := svc.Expire(ctx, ExpireInput{Amount: 10, Reason: reason})
			return err
		}},
	}
	refused := []string{
		"refund for alice@example.com", // prose, whitespace
		"Refund For Alice",             // prose, capitals + whitespace
		"ai_generation:job_123 extra",  // trailing prose
		":leading-separator",           // separator outside a phrase
		"trailing:",                    // separator outside a phrase
		"a::b",                         // doubled separator
		strings.Repeat("a", 256),       // over the 255-character column bound
	}
	for _, tc := range calls {
		for _, reason := range refused {
			if err := tc.call(reason); !apperr.HasCode(err, ErrInvalidReason.Code) {
				t.Errorf("%s(Reason=%q): err = %v, want %s", tc.name, reason, err, ErrInvalidReason.Code)
			}
		}
	}

	// Nothing was written by any refused call: the ledger is empty.
	rows, err := svc.Transactions(ctx)
	if err != nil {
		t.Fatalf("Transactions() error = %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("Transactions() returned %d rows, want 0 -- a refused reason must fail before anything is written", len(rows))
	}

	// A bounded phrase -- the module's own documented vocabulary -- still
	// succeeds on every entry point, each on its own tenant so one call's
	// balance movement never starves the next.
	phraseCalls := []struct {
		name string
		call func() error
	}{
		{"Grant", func() error {
			_, err := svc.Grant(pkgcore.WithTenant(context.Background(), "phrase-grant"), GrantInput{Amount: 10, Reason: "promo:welcome_2026"})
			return err
		}},
		{"PreDeduct", func() error {
			// A grant first, so the reservation has an Available balance to
			// reserve from.
			reserveCtx := pkgcore.WithTenant(context.Background(), "phrase-reserve")
			if _, err := svc.Grant(reserveCtx, GrantInput{Amount: 10, Reason: "seed"}); err != nil {
				return err
			}
			_, err := svc.PreDeduct(reserveCtx, PreDeductInput{Amount: 10, IdempotencyKey: "reserve-phrase", Reason: "ai_generation:job-1"})
			return err
		}},
		{"Expire", func() error {
			// A grant first, so the expire has an Available balance to take.
			grantCtx := pkgcore.WithTenant(context.Background(), "phrase-expire")
			if _, err := svc.Grant(grantCtx, GrantInput{Amount: 10, Reason: "seed"}); err != nil {
				return err
			}
			_, err := svc.Expire(grantCtx, ExpireInput{Amount: 10, Reason: "expiry:2026-09-policy"})
			return err
		}},
	}
	for _, tc := range phraseCalls {
		if err := tc.call(); err != nil {
			t.Errorf("%s with a bounded-phrase reason: err = %v, want nil", tc.name, err)
		}
	}

	// An absent reason stays legal: reason is optional.
	if _, err := svc.Grant(ctx, GrantInput{Amount: 10}); err != nil {
		t.Errorf("Grant with no reason: err = %v, want nil", err)
	}
}
