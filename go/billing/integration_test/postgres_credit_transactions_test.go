//go:build integration

// PostgreSQL regressions for CreditService's credit ledger on the real
// server -- see the package doc comment in postgres_isolation_test.go for
// the defect class this tier exists for and the contract each test
// guards.
package billing_test

import (
	"sync"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/billing/internal/testutil"
	"github.com/vislake/speed/go/billing/migrations"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/testkit"
)

// newPostgresDB returns a fresh migrated PostgreSQL *gorm.DB -- the
// zero-to-head migration proof (billing's postgres/*.sql applied from zero
// through dbkit.MigrationRegistry) plus the connection every test drives
// its services over.
func newPostgresDB(t *testing.T) *gorm.DB {
	t.Helper()
	return testutil.NewPostgres(t, "billing", migrations.FS)
}

// TestPostgres_PreDeduct_IdempotentRetry_ReturnsTheSameReservation is the
// PostgreSQL leg of the unit tier's own
// TestCreditService_PreDeduct_IdempotentRetry_ReturnsTheSameReservation,
// in the shape the hazard actually lives in: the retried PreDeduct runs
// against the same database as a first attempt that already committed, on
// PostgreSQL itself.
//
// The retry's insert runs as ON CONFLICT DO NOTHING (which never aborts
// the transaction), the pre-existing row is read back on the
// still-healthy transaction, and the retry returns the identical
// reservation. The dialect difference underneath is why the shape
// matters: SQLite tolerates a failed statement inside an open
// transaction, PostgreSQL does not -- after a statement error the
// transaction is aborted and the recovery's read would fail with SQLSTATE
// 25P02, so PreDeduct would return an error and the idempotency contract
// would collapse: a caller retrying a PreDeduct whose first attempt had
// already committed would get an error instead of its own earlier
// reservation, on the money path itself (no balance double-reserved --
// the transaction aborts before anything else runs -- but the retry could
// never succeed, so every retry-driven caller breaks).
func TestPostgres_PreDeduct_IdempotentRetry_ReturnsTheSameReservation(t *testing.T) {
	db := newPostgresDB(t)
	svc := billing.NewCreditService(db)
	ctx := testkit.TenantCtx("tenant-a")

	if _, err := svc.Grant(ctx, billing.GrantInput{Amount: 100}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	first, err := svc.PreDeduct(ctx, billing.PreDeductInput{Amount: 30, IdempotencyKey: "job-1"})
	if err != nil {
		t.Fatalf("first PreDeduct: %v", err)
	}
	second, err := svc.PreDeduct(ctx, billing.PreDeductInput{Amount: 30, IdempotencyKey: "job-1"})
	if err != nil {
		t.Fatalf("retried PreDeduct = %v: on PostgreSQL the pre-fix recovery read the duplicate back on the aborted transaction, failed with 25P02, and the idempotent retry could never succeed", err)
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

	// Exactly one ledger row for the reservation, read back under the
	// tenant session -- the append-only ledger must not have been
	// double-written by the retry.
	var rows []billing.CreditTransaction
	err = dbkit.WithTenantSession(ctx, db, func(session *gorm.DB) error {
		return session.Where("id = ?", "job-1").Find(&rows).Error
	})
	if err != nil {
		t.Fatalf("read ledger rows: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("ledger rows for job-1 = %d, want exactly 1", len(rows))
	}
}

// TestPostgres_CreditLifecycle_ReserveConfirmRefundAndRetries drives the
// whole reserve -> confirm -> refund family against real PostgreSQL,
// including both idempotent-retry legs (a repeated Confirm and a repeated
// Refund must each be clean no-ops returning the existing row, never
// errors) and the confirm-then-refund refusal -- the full matrix the unit
// tier covers on SQLite, re-proven on the second dialect where the
// statement-level semantics genuinely differ. This is the family-level
// counterpart of the PreDeduct-specific regression above: it pins that the
// reserve half and the confirm/refund half compose into a working ledger
// lifecycle on the real server.
func TestPostgres_CreditLifecycle_ReserveConfirmRefundAndRetries(t *testing.T) {
	svc := billing.NewCreditService(newPostgresDB(t))
	ctx := testkit.TenantCtx("tenant-a")

	if _, err := svc.Grant(ctx, billing.GrantInput{Amount: 100}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	res, err := svc.PreDeduct(ctx, billing.PreDeductInput{Amount: 40, IdempotencyKey: "lifecycle-1"})
	if err != nil {
		t.Fatalf("PreDeduct: %v", err)
	}
	if res.Status != string(billing.CreditTransactionStatusPending) {
		t.Fatalf("reservation status = %q, want pending", res.Status)
	}

	confirmed, err := svc.Confirm(ctx, "lifecycle-1")
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if confirmed.Status != string(billing.CreditTransactionStatusConfirmed) {
		t.Errorf("confirmed status = %q, want confirmed", confirmed.Status)
	}
	// The idempotent-retry leg of Confirm: a second Confirm for the same
	// key is a no-op success returning the existing row.
	again, err := svc.Confirm(ctx, "lifecycle-1")
	if err != nil {
		t.Fatalf("retried Confirm: %v, want the same no-op success", err)
	}
	if again.ID != confirmed.ID || again.Status != confirmed.Status {
		t.Errorf("retried Confirm = %+v, want the identical row %+v", again, confirmed)
	}

	bal, err := svc.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance after confirm: %v", err)
	}
	if bal.Available != 60 || bal.Reserved != 0 {
		t.Errorf("balance after confirm = %+v, want Available=60 Reserved=0 (40 permanently spent)", bal)
	}

	// A second reservation, refunded this time -- the refund leg with its
	// own idempotent-retry no-op.
	second, err := svc.PreDeduct(ctx, billing.PreDeductInput{Amount: 25, IdempotencyKey: "lifecycle-2"})
	if err != nil {
		t.Fatalf("second PreDeduct: %v", err)
	}
	refunded, err := svc.Refund(ctx, "lifecycle-2")
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if refunded.Status != string(billing.CreditTransactionStatusRefunded) {
		t.Errorf("refunded status = %q, want refunded", refunded.Status)
	}
	retried, err := svc.Refund(ctx, "lifecycle-2")
	if err != nil {
		t.Fatalf("retried Refund: %v, want the same no-op success", err)
	}
	if retried.ID != second.ID {
		t.Errorf("retried Refund = %+v, want the identical row %+v", retried, second)
	}

	bal, err = svc.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance after refund: %v", err)
	}
	// 100 granted, 40 confirmed-spent (Available 60), 25 reserved then
	// refunded back onto the 60: Available 60, Reserved 0.
	if bal.Available != 60 || bal.Reserved != 0 {
		t.Errorf("balance after refund = %+v, want Available=60 Reserved=0 (the 25 reservation released back onto the 60)", bal)
	}

	// Confirm-then-refund of the same reservation is refused -- reversing
	// an already-confirmed spend would double-release credits.
	if _, err := svc.Refund(ctx, "lifecycle-1"); !apperr.HasCode(err, billing.ErrCreditTransactionAlreadyResolved.Code) {
		t.Errorf("Refund of the confirmed reservation: err = %v, want %s", err, billing.ErrCreditTransactionAlreadyResolved.Code)
	}
	if _, err := svc.Confirm(ctx, "lifecycle-2"); !apperr.HasCode(err, billing.ErrCreditTransactionAlreadyResolved.Code) {
		t.Errorf("Confirm of the refunded reservation: err = %v, want %s", err, billing.ErrCreditTransactionAlreadyResolved.Code)
	}
	// And an unknown key is NotFound on both legs.
	if _, err := svc.Confirm(ctx, "no-such-reservation"); !apperr.HasCode(err, billing.ErrCreditTransactionNotFound.Code) {
		t.Errorf("Confirm of an unknown key: err = %v, want %s", err, billing.ErrCreditTransactionNotFound.Code)
	}
}

// TestPostgres_PreDeduct_ConcurrentOverBalance_OnlyOneSucceeds re-runs the
// unit tier's concurrent single-winner proof against real PostgreSQL: the
// family's concurrency core is one database-arbitrated arithmetic UPDATE
// per mutation (applyBalanceDelta), whose "the database itself serializes
// the two UPDATEs" argument holds identically on both dialects -- this is
// that argument's real-server leg, run under the same goroutine shape the
// unit test uses.
func TestPostgres_PreDeduct_ConcurrentOverBalance_OnlyOneSucceeds(t *testing.T) {
	svc := billing.NewCreditService(newPostgresDB(t))
	ctx := testkit.TenantCtx("tenant-a")

	const balance = 100
	const deductEach = 60 // two of these (120) exceed the 100 balance -- at most one may succeed.
	if _, err := svc.Grant(ctx, billing.GrantInput{Amount: balance}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	var wg sync.WaitGroup
	results := make([]error, 2)
	keys := []string{"concurrent-a", "concurrent-b"}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.PreDeduct(ctx, billing.PreDeductInput{Amount: deductEach, IdempotencyKey: keys[i]})
			results[i] = err
		}(i)
	}
	wg.Wait()

	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		} else if !apperr.HasCode(err, billing.ErrInsufficientCredits.Code) {
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

// TestPostgres_Expire_KeyedRetry_DoesNotDoubleApply is the PostgreSQL leg
// of the unit tier's keyed-Expire regression
// (TestCreditService_Expire_KeyedRetry_DoesNotDoubleApply), in the shape
// the hazard actually lives in: the retried
// keyed Expire runs against real PostgreSQL, where a unique-violation
// statement error on a still-open transaction would leave it aborted
// (SQLSTATE 25P02) and the retry's read-back could never run. The keyed
// Expire inserts as ON CONFLICT DO NOTHING on the shared insertIdempotent
// core -- never a statement error -- so the retry's read-back runs on a
// healthy transaction, returns the first run's own row, and applies no
// second deduction: the same guarantee PreDeduct's reserve half already
// proves on this tier, now for the expiry write a jobs-driven sweep would
// call under its deterministic per-window key.
func TestPostgres_Expire_KeyedRetry_DoesNotDoubleApply(t *testing.T) {
	db := newPostgresDB(t)
	svc := billing.NewCreditService(db)
	ctx := testkit.TenantCtx("tenant-a")

	if _, err := svc.Grant(ctx, billing.GrantInput{Amount: 100}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	const key = "expiry:2026-09-policy:window-1"
	first, err := svc.Expire(ctx, billing.PreDeductInput{Amount: 40, IdempotencyKey: key})
	if err != nil {
		t.Fatalf("first keyed Expire: %v", err)
	}
	if first.ID != key {
		t.Fatalf("keyed Expire row ID = %q, want the supplied key %q", first.ID, key)
	}

	second, err := svc.Expire(ctx, billing.PreDeductInput{Amount: 40, IdempotencyKey: key})
	if err != nil {
		t.Fatalf("retried keyed Expire = %v: on PostgreSQL a statement error on the open transaction would abort it (25P02) and the read-back could never run", err)
	}
	if second.ID != first.ID {
		t.Errorf("retried keyed Expire = %+v, want the first run's own row %+v", second, first)
	}

	bal, err := svc.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 60 {
		t.Errorf("balance after a retried keyed Expire = %+v, want Available=60 (deducted exactly once)", bal)
	}

	// Exactly one expire row for the key -- the retry must not have
	// double-written the ledger.
	var rows []billing.CreditTransaction
	err = dbkit.WithTenantSession(ctx, db, func(session *gorm.DB) error {
		return session.Where("id = ?", key).Find(&rows).Error
	})
	if err != nil {
		t.Fatalf("read ledger rows: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("ledger rows for %q = %d, want exactly 1", key, len(rows))
	}
}
