package dbkit

import (
	"context"
	"errors"
	"fmt"
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

// tenantSessionNestedTestDBSeq numbers this test's dbkit.Open databases so a
// re-run never collides with a still-open in-memory database from a prior
// run sharing the same process.
var tenantSessionNestedTestDBSeq atomic.Int64

// TestWithTenantSession_Nested_RefusesRatherThanPublishingBeforeOuterCommits
// is the regression for a real, reproduced bug: calling WithTenantSession a
// second time from inside an outer WithTenantSession's own fn — passing the
// tx *gorm.DB the outer fn received, rather than the base db everything else
// in this codebase passes — used to resurrect the exact phantom-audit-event
// bug the buffered-publish mechanism (audit_capture.go, this file's sibling)
// exists to close.
//
// Before ErrNestedTenantSession's check existed, the inner WithTenantSession
// call allocated its own fresh *auditBuffer, saw its own
// db.Transaction(...) call return nil (GORM issues a SAVEPOINT for a
// *gorm.DB already inside a transaction and releases it, per
// gorm.io/gorm@v1.31.2/finisher_api.go's Transaction — never a real BEGIN or
// COMMIT), and published that buffer immediately — before the real, still-
// open outer transaction was resolved at all. Forcing the outer fn to then
// return a real error rolled back the widget row the inner call had
// created, while the event the inner call had already published stayed on
// the bus: a real phantom audit event surviving a real rollback, against a
// real SQLite database, no mocking involved.
//
// Post-fix, the nested call is refused outright — ErrNestedTenantSession,
// returned before any transaction (even a savepoint) opens and before fn
// runs at all — so the inner Create never executes, nothing is ever
// buffered, and nothing is ever published, independent of how the outer
// transaction eventually resolves.
func TestWithTenantSession_Nested_RefusesRatherThanPublishingBeforeOuterCommits(t *testing.T) {
	dsn := fmt.Sprintf("file:tenant_session_nested_%d?mode=memory&cache=shared", tenantSessionNestedTestDBSeq.Add(1))

	bus := pkgcore.NewMemoryEventBus()
	var published atomic.Int64
	bus.Subscribe(EventWriteCaptured, func(_ context.Context, _ pkgcore.Event) error {
		published.Add(1)
		return nil
	})

	db, err := Open(context.Background(), Options{Dialect: DialectSQLite, DSN: dsn, AuditBus: bus})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Exec(`CREATE TABLE widgets (
		id        VARCHAR(26)  NOT NULL,
		tenant_id VARCHAR(26)  NOT NULL,
		name      VARCHAR(255) NOT NULL,
		value     INTEGER      NOT NULL DEFAULT 0,
		PRIMARY KEY (tenant_id, id)
	)`).Error; err != nil {
		t.Fatalf("create widgets table: %v", err)
	}

	ctx := ctxTenant("tenant-a")

	var nestedErr error
	var innerCreateRan bool
	outerErr := WithTenantSession(ctx, db, func(tx *gorm.DB) error {
		nestedErr = WithTenantSession(ctx, tx, func(innerTx *gorm.DB) error {
			innerCreateRan = true
			return innerTx.Create(&testutil.Widget{ID: "w-1", TenantID: "tenant-a", Name: "gadget"}).Error
		})
		return nestedErr
	})

	if !errors.Is(nestedErr, ErrNestedTenantSession) {
		t.Fatalf("nested WithTenantSession() error = %v, want errors.Is(err, ErrNestedTenantSession)", nestedErr)
	}
	if innerCreateRan {
		t.Error("the nested call's own fn ran; want it refused before fn is ever called, exactly like the missing-tenant-context check")
	}
	if !errors.Is(outerErr, ErrNestedTenantSession) {
		t.Fatalf("outer WithTenantSession() error = %v, want errors.Is(err, ErrNestedTenantSession) (the outer fn returned the nested call's own error unchanged)", outerErr)
	}

	if got := published.Load(); got != 0 {
		t.Errorf("published event count = %d, want 0 (the nested call must be refused before capturing or publishing anything)", got)
	}

	var count int64
	if err := db.Raw(`SELECT count(*) FROM widgets WHERE id = ? AND tenant_id = ?`, "w-1", "tenant-a").Scan(&count).Error; err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 0 {
		t.Errorf("row count for w-1 = %d, want 0 (the nested Create never ran at all)", count)
	}
}

// TestWithTenantSession_SQLite_CommitTimeFailure_RollbackAttemptClearsTheResidue
// is the coverage test go/dbkit/AGENTS.md's "commit-time failure"
// known-limitation entry recorded as missing, for the cell that entry
// reproduces: a constraint genuinely deferred to commit time (PRAGMA
// defer_foreign_keys = ON, real SQLite semantics) makes Commit() itself
// fail, and Go's database/sql has already closed the *sql.Tx the instant
// that Commit was attempted, so the rollback a Transaction wrapper defers
// for this exact failure never reaches the driver -- which, on this
// package's SQLite driver, leaves the connection holding the uncommitted
// transaction, fn's write visible to any later statement on that
// connection. WithTenantSession's explicit rollback attempt
// (rollbackAfterFailedCommit) must clear that residue; the assertion below
// -- the fn's write is NOT visible once WithTenantSession has returned its
// commit error -- failed on the pre-fix implementation (which delegated
// the lifecycle to gorm's Transaction wrapper) with the row still counted,
// and passes after.
func TestWithTenantSession_SQLite_CommitTimeFailure_RollbackAttemptClearsTheResidue(t *testing.T) {
	dsn := fmt.Sprintf("file:tenant_session_commit_failure_%d?mode=memory&cache=shared", tenantSessionNestedTestDBSeq.Add(1))

	db, err := Open(context.Background(), Options{Dialect: DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// One connection, so the statements below are deterministic about which
	// connection they ride: the pool cannot hand a later statement a
	// different, clean connection and make the residue invisible for the
	// wrong reason.
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB(): %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	// foreign_keys is a per-connection pragma; with one connection this
	// Exec pins it for every later statement.
	if err = db.Exec("PRAGMA foreign_keys = ON").Error; err != nil {
		t.Fatalf("enable foreign keys: %v", err)
	}
	if err = db.Exec(`CREATE TABLE parents (id VARCHAR(26) PRIMARY KEY)`).Error; err != nil {
		t.Fatalf("create parents: %v", err)
	}
	if err = db.Exec(`CREATE TABLE children (
		id  VARCHAR(26) PRIMARY KEY,
		pid VARCHAR(26) NOT NULL REFERENCES parents(id)
	)`).Error; err != nil {
		t.Fatalf("create children: %v", err)
	}

	ctx := ctxTenant("tenant-a")
	err = WithTenantSession(ctx, db, func(tx *gorm.DB) error {
		// Defer the FK check to commit time: the INSERT below references a
		// parent that does not exist, which is legal inside the transaction
		// and fails only when Commit() runs -- the commit-time failure cell.
		if pragmaErr := tx.Exec("PRAGMA defer_foreign_keys = ON").Error; pragmaErr != nil {
			return pragmaErr
		}
		return tx.Exec(`INSERT INTO children (id, pid) VALUES ('c-1', 'no-such-parent')`).Error
	})
	if err == nil {
		t.Fatal("WithTenantSession() error = nil, want the deferred FK violation to fail the commit")
	}

	// The write fn attempted must not be visible once WithTenantSession has
	// returned the commit failure: rollbackAfterFailedCommit's explicit
	// rollback must have cleared the residue the failed commit left on the
	// connection. This read rides the pool like any later caller statement
	// would.
	var count int64
	if err := db.Raw(`SELECT count(*) FROM children WHERE id = ?`, "c-1").Scan(&count).Error; err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 0 {
		t.Errorf("children row count after the failed commit = %d, want 0 (the commit-time-failure residue must be rolled back by WithTenantSession's own explicit rollback attempt, not left visible to later statements)", count)
	}

	// The connection must remain fully usable for a fresh session.
	if err := WithTenantSession(ctx, db, func(tx *gorm.DB) error {
		return tx.Exec(`INSERT INTO parents (id) VALUES ('p-1')`).Error
	}); err != nil {
		t.Fatalf("second WithTenantSession after the failed commit: %v", err)
	}
	if err := db.Raw(`SELECT count(*) FROM parents WHERE id = ?`, "p-1").Scan(&count).Error; err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Errorf("parents row count after the second session = %d, want 1", count)
	}
}
