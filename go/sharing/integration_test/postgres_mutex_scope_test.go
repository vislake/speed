//go:build integration

// This file is go/sharing's PostgreSQL proof for the writeMu scope
// decision (concurrency.go's own doc comment): the in-process mutex
// around guarded writes exists for SQLite's single-writer file lock and
// must NOT be taken on PostgreSQL, where the database's own row locks
// plus the bounded conflict retry (concurrency.go's withTxRetry) are the
// honest mechanism.
// The SQLite half of the same decision -- the mutex stays engaged there,
// so the module's writers never contend for the file lock -- is pinned by
// the unit tier (concurrency_test.go's
// TestShareRepository_GuardedWritesStaySerializedOnSQLite, which runs on
// every plain `go test`); this file exists because the PostgreSQL half
// needs a real server to be honest about. A single-writer premise that
// does not exist on PostgreSQL cannot be proven against SQLite.
//
// The proof is deterministic, not a timing measurement: the test holds one
// tenant's share row locked at the database level (SELECT ... FOR UPDATE
// from its own connection), so tenant A's guarded write -- whenever it
// reaches its UPDATE -- blocks inside its transaction on that row lock. A
// process-wide mutex held while blocked would queue tenant B's guarded
// write behind the in-process lock until A's transaction is released;
// without one, B's write -- against its own tenant's unlocked row --
// completes while A is still blocked. Whether B
// completed is an event, not a duration. A's arrival inside its blocked
// transaction is observed (not assumed) by polling pg_locks for its
// ungranted lock request, so B is never launched before A is genuinely
// mid-write.
package sharing_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/sharing"
	"github.com/vislake/speed/go/sharing/internal/testutil"
	"github.com/vislake/speed/go/sharing/migrations"
)

// TestPostgres_GuardedWrites_UnrelatedTenantsNotSerialized proves two
// unrelated tenants' guarded writes proceed concurrently on a real
// PostgreSQL server -- one never blocks behind the other's in-process
// mutex. Before the fix runGuardedWrite wrapped every guarded transaction
// in one process-wide, cross-tenant mutex (writeMu) regardless of dialect,
// so tenant B's write was serialized behind tenant A's: with A's
// transaction parked on the row lock this test holds, B could not
// complete at all (fails before: B blocked, test times out). After the
// fix PostgreSQL guarded writes take no in-process mutex -- the retry
// path (withTxRetry) is what absorbs the rare real conflict -- so B
// completes while A is still parked, and both tenants' writes land
// correctly once A's lock is released.
func TestPostgres_GuardedWrites_UnrelatedTenantsNotSerialized(t *testing.T) {
	db := testutil.NewPostgres(t, "sharing", migrations.FS)

	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	mod := sharing.NewModule(db)
	if err := mod.Register(reg); err != nil {
		t.Fatalf("sharing.Module.Register: %v", err)
	}
	svc := mod.Service()

	tenantA := pkgcore.TenantID("tenant-a")
	tenantB := pkgcore.TenantID("tenant-b")
	ctxA := pkgcore.WithTenant(context.Background(), tenantA)
	ctxB := pkgcore.WithTenant(context.Background(), tenantB)

	shareA, err := svc.Create(ctxA, sharing.CreateParams{ResourceRef: "storage:obj-a"})
	if err != nil {
		t.Fatalf("Create (tenant A): %v", err)
	}
	shareB, err := svc.Create(ctxB, sharing.CreateParams{ResourceRef: "storage:obj-b"})
	if err != nil {
		t.Fatalf("Create (tenant B): %v", err)
	}

	// Park tenant A's share row under this connection's own row lock: A's
	// guarded UPDATE -- whenever it reaches it -- blocks on this lock, so A
	// is guaranteed to be mid-transaction (and would, under a process-wide
	// mutex, hold it) for as long as the test needs.
	hold := db.Begin()
	defer hold.Rollback() //nolint:errcheck -- rollback after the test's own releases
	if err := hold.Exec("SELECT id FROM sharing_shares WHERE id = $1 FOR UPDATE", shareA.Share.ID).Error; err != nil {
		t.Fatalf("SELECT ... FOR UPDATE on tenant A's row: %v", err)
	}

	accessErrA := make(chan error, 1)
	go func() {
		_, err := svc.Access(ctxA, shareA.Token, sharing.AccessParams{})
		accessErrA <- err
	}()

	// Wait until A's blocked UPDATE is genuinely visible in pg_locks (its
	// ungranted request against the row lock the test holds) before
	// launching B -- observation, not an assumption about scheduling.
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB(): %v", err)
	}
	waitForBlockedWriter(t, sqlDB)

	accessErrB := make(chan error, 1)
	go func() {
		_, err := svc.Access(ctxB, shareB.Token, sharing.AccessParams{})
		accessErrB <- err
	}()

	// B must complete while A is still parked: on PostgreSQL no process
	// mutex serializes unrelated tenants' writes. The window is generous --
	// a completed B is an event, and a genuinely stuck B is the failure.
	select {
	case err := <-accessErrB:
		if err != nil {
			t.Fatalf("tenant B's Access failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("tenant B's guarded write did not complete while tenant A's write was parked on its row lock -- the process-wide cross-tenant mutex is serializing unrelated tenants on PostgreSQL")
	}

	// Release A's row lock: A's write completes, and both tenants' granted
	// views land exactly once.
	if err := hold.Rollback().Error; err != nil {
		t.Fatalf("rollback of the row lock: %v", err)
	}
	select {
	case err := <-accessErrA:
		if err != nil {
			t.Fatalf("tenant A's Access failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tenant A's Access did not complete after its row lock was released")
	}

	gotA, err := svc.Get(ctxA, shareA.Share.ID)
	if err != nil {
		t.Fatalf("Get (tenant A): %v", err)
	}
	if gotA.ViewCount != 1 {
		t.Errorf("tenant A ViewCount = %d, want 1", gotA.ViewCount)
	}
	gotB, err := svc.Get(ctxB, shareB.Share.ID)
	if err != nil {
		t.Fatalf("Get (tenant B): %v", err)
	}
	if gotB.ViewCount != 1 {
		t.Errorf("tenant B ViewCount = %d, want 1", gotB.ViewCount)
	}
}

// waitForBlockedWriter polls pg_locks until some backend's ungranted lock
// request appears -- the signature of tenant A's UPDATE waiting on the row
// lock this test holds. A bounded poll keeps a genuinely broken test (a
// writer that never reaches its UPDATE) from hanging; a query error is
// failed fast, since count(*) always returns a row and the poll connection
// is the same live pool A and B write through.
func waitForBlockedWriter(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := sqlDB.QueryRow("SELECT count(*) FROM pg_locks WHERE NOT granted").Scan(&waiting); err != nil {
			t.Fatalf("query pg_locks: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no ungranted PostgreSQL lock appeared within 10s -- tenant A's write never reached its row-locked UPDATE (broken test, not a fix regression)")
}
