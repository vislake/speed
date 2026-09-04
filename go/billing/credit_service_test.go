package billing

import (
	"context"
	"sync"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

func newCreditService(t *testing.T) *CreditService {
	t.Helper()
	return NewCreditService(newTestDB(t))
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
