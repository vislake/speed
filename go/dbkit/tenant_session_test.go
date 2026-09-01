package dbkit

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
)

// errSessionFnFailed is a sentinel a test's fn returns to force
// WithTenantSession's transaction to roll back. It is distinct from any
// error dbkit or gorm could plausibly return on its own, so a test can tell
// "fn's own deliberate failure came back unchanged" apart from "something
// else went wrong and happened to also be non-nil".
var errSessionFnFailed = errors.New("tenant_session_test: deliberate fn failure")

// TestWithTenantSession_SQLite_CommitsOnSuccess_AndNeverAttemptsAGUCStep
// covers two things at once against SQLite: that WithTenantSession runs fn
// inside a real transaction whose write is durably committed once
// WithTenantSession returns, and — inseparably from that, on this dialect —
// that no PostgreSQL-only GUC step was attempted. SQLite has neither a
// SET LOCAL statement nor a set_config function, so if WithTenantSession's
// dialect check were ever wrong and it tried that step here, this whole
// transaction would fail with a SQL error instead of committing cleanly:
// the success asserted below IS the proof the GUC step was skipped, exactly
// as the isPostgres-gated branch in WithTenantSession's implementation
// intends for the sqlite dialect.
func TestWithTenantSession_SQLite_CommitsOnSuccess_AndNeverAttemptsAGUCStep(t *testing.T) {
	db := testutil.NewTestSQLite(t)
	ctx := ctxTenant("tenant-a")

	err := WithTenantSession(ctx, db, func(tx *gorm.DB) error {
		return tx.Create(&testutil.Widget{ID: "w-1", TenantID: "tenant-a", Name: "gadget"}).Error
	})
	if err != nil {
		t.Fatalf("WithTenantSession() error = %v", err)
	}

	// Read through the outer, non-transactional db handle: the row is only
	// visible here if the inner transaction actually committed.
	var got testutil.Widget
	if err := db.First(&got, "id = ?", "w-1").Error; err != nil {
		t.Fatalf("First() after successful WithTenantSession error = %v, want the committed row", err)
	}
	if got.Name != "gadget" || got.TenantID != "tenant-a" {
		t.Errorf("got = %+v, want {ID:w-1 TenantID:tenant-a Name:gadget}", got)
	}
}

// TestWithTenantSession_SQLite_FailureInFn_RollsBackWrites proves
// WithTenantSession's transaction is real, not merely a call-through to fn:
// a write fn issues before returning an error must not survive.
func TestWithTenantSession_SQLite_FailureInFn_RollsBackWrites(t *testing.T) {
	db := testutil.NewTestSQLite(t)
	ctx := ctxTenant("tenant-a")

	err := WithTenantSession(ctx, db, func(tx *gorm.DB) error {
		if err := tx.Create(&testutil.Widget{ID: "w-1", TenantID: "tenant-a", Name: "gadget"}).Error; err != nil {
			return err
		}
		return errSessionFnFailed
	})
	if !errors.Is(err, errSessionFnFailed) {
		t.Fatalf("WithTenantSession() error = %v, want errors.Is(err, errSessionFnFailed)", err)
	}

	var count int64
	if err := db.Model(&testutil.Widget{}).Where("id = ?", "w-1").Count(&count).Error; err != nil {
		t.Fatalf("Count() error = %v", err)
	}
	if count != 0 {
		t.Errorf("row count for w-1 after a failed WithTenantSession = %d, want 0 (the create must have been rolled back, not left committed)", count)
	}
}

// TestWithTenantSession_NoTenantInContext_FailsClosedBeforeFnRuns covers
// dbkit's standard fail-closed contract: a context with no tenant must
// never reach fn at all, not merely "return an error after running it".
func TestWithTenantSession_NoTenantInContext_FailsClosedBeforeFnRuns(t *testing.T) {
	db := testutil.NewTestSQLite(t)

	var calls atomic.Int64
	err := WithTenantSession(context.Background(), db, func(tx *gorm.DB) error {
		calls.Add(1)
		return nil
	})
	if !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Errorf("WithTenantSession() error = %v, want errors.Is(err, pkgcore.ErrNoTenant)", err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("fn call count = %d, want 0 (WithTenantSession must fail closed before ever calling fn)", got)
	}
}
