package sqlite

// busy_timeout_test.go pins the busy_timeout contract withDefaultPragmas
// declares (dialect_sqlite.go): every connection dbkit.Open returns carries
// the bounded default, ordinary writer contention waits through it and
// converges once the holder commits, a hold that outlasts it fails with the
// real busy error instead of hanging, and — the boundary spelled out in
// withDefaultPragmas' doc comment — SQLite's deadlock avoidance answers a
// read-then-write upgrade with an immediate busy error that no timeout
// setting waits out.
//
// These are contract-pinning tests, deliberately not regression tests: the
// semantics they pin are supplied today by two independent mechanisms — the
// driver's own implicit default (glebarez/go-sqlite applies
// `pragma BUSY_TIMEOUT(5000)` on every new connection) and the explicit
// `_pragma=busy_timeout(...)` parameter withDefaultPragmas appends — so
// every test below passes on the pre-change code that carried only the
// driver default too. The redundancy is the point of declaring the
// contract: dbkit states the bound so that neither a future driver change
// (dropping or altering its implicit default, or changing how it applies
// `_pragma` DSN parameters) nor a caller-supplied DSN override can silently
// remove it. Each test's doc comment says which failure it would surface;
// the declaration itself — the parameter string and its append-after-
// caller order — is pinned at string level by TestWithDefaultPragmas in
// dialect_sqlite_test.go.
//
// Every test drives two real connections to one real SQLite file through
// dbkit.Open (dialect registration is this package's own init side effect),
// exactly the shape a two-worker race in a standalone deployment has. Lock
// hold and release are sequenced by the test itself — the holder commits
// only after the contender has reported, or after a fixed grace window —
// never by sleeping until some assumed interleaving happens, so there is no
// sleep race in any direction: each test's outcome is determined by SQLite's
// lock protocol plus one generous grace window, not by which goroutine the
// scheduler happened to run first.

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit"
)

// openBusyTimeoutDB opens one dbkit SQLite connection to dsn with the pool
// capped at a single connection, so every statement this handle runs is
// guaranteed to use the same physical connection and its locks.
func openBusyTimeoutDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

// createBusyTimeoutSchema creates the one-table schema the contention tests
// write to. The table is only ever touched by these tests.
func createBusyTimeoutSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
}

type busyOutcome struct {
	err     error
	elapsed time.Duration
}

// awaitBusyOutcome waits up to cap for the contender's report and fails the
// test if it never arrives. Every report carries its own elapsed time, so a
// test that needs the outcome's timing asserts on that, never on wall time
// measured around the await.
func awaitBusyOutcome(t *testing.T, label string, outCh <-chan busyOutcome, cap time.Duration) busyOutcome {
	t.Helper()
	select {
	case res := <-outCh:
		return res
	case <-time.After(cap):
		t.Fatalf("%s never resolved within %v", label, cap)
		return busyOutcome{}
	}
}

// TestOpen_AppliesDefaultBusyTimeoutToEveryPooledConnection proves the
// pragma's reach: busy_timeout must read back as defaultBusyTimeoutMS on
// every physical connection dbkit.Open's pool can hand out, not merely on
// whichever connection happened to open first. It holds several sql.Conn
// handles open at once (never releasing one before acquiring the next),
// which forces database/sql's pool to establish that many distinct physical
// connections rather than serving every acquisition from one reused
// connection, and reads the pragma back on each one.
//
// The read-back cannot tell the bound's source apart: the driver's own
// implicit default alone would also answer 5000 today, so this test would
// pass on a connection carrying either mechanism by itself. What it pins is
// that the declared value actually reaches every pooled connection through
// the driver's DSN-parameter handling — if a future driver stopped applying
// `_pragma` parameters, or changed their syntax, the read-back would fall
// to whatever implicit default (if any) remained and this test would fail.
// That the parameter is present in the DSN at all, appended after any
// caller-supplied one, is pinned separately at the string level by
// TestWithDefaultPragmas in dialect_sqlite_test.go.
func TestOpen_AppliesDefaultBusyTimeoutToEveryPooledConnection(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "busy-timeout-test.db")

	db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB(): %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	const connections = 8
	conns := make([]*sql.Conn, connections)
	for i := range conns {
		conn, err := sqlDB.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn() #%d: %v", i, err)
		}
		conns[i] = conn
	}
	t.Cleanup(func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	if got := sqlDB.Stats().OpenConnections; got < 2 {
		t.Skip("pool reports fewer than 2 open connections -- cannot distinguish per-connection application from a one-time PRAGMA in this run alone")
	}

	for i, conn := range conns {
		var value int
		if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&value); err != nil {
			t.Fatalf("PRAGMA busy_timeout on connection #%d: %v", i, err)
		}
		if value != defaultBusyTimeoutMS {
			t.Fatalf("PRAGMA busy_timeout on connection #%d = %d ms, want %d ms (defaultBusyTimeoutMS)", i, value, defaultBusyTimeoutMS)
		}
	}
}

// TestSQLiteBusyTimeout_ContendingWriterWaitsThenSucceeds pins the
// ordinary-contention half of the contract: connection A holds an
// uncommitted write (the file's RESERVED lock, taken by its first INSERT),
// and connection B tries to write to the same file. B must wait through the
// busy handler and succeed once A commits — bounded waiting, not an
// immediate failure and not a serialization the caller must schedule
// around. A commits after a 300ms grace window, far inside the 5s timeout,
// and the elapsed lower bound distinguishes a real wait from an immediate
// answer.
//
// This is a contract-pinning test, not a regression test for the DSN
// parameter: the wait it observes is supplied today by two independent
// mechanisms — the driver's per-connection implicit default and this
// package's explicit parameter — so it passes on the pre-change code that
// carried only the driver default, and would keep passing if either
// mechanism alone survived a future change. That redundancy is the point of
// declaring the contract (see the file header). The parameter's own
// presence and reach are pinned by TestWithDefaultPragmas (string-level)
// and TestOpen_AppliesDefaultBusyTimeoutToEveryPooledConnection
// (read-back); this test pins the observable semantics the two mechanisms
// jointly deliver, and fails only if the bounded wait itself is gone — an
// immediate SQLITE_BUSY, which takes both mechanisms disappearing at once.
func TestSQLiteBusyTimeout_ContendingWriterWaitsThenSucceeds(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "busy-timeout-contend.db")
	a := openBusyTimeoutDB(t, dsn)
	b := openBusyTimeoutDB(t, dsn)
	createBusyTimeoutSchema(t, a)

	// A: hold the write lock. The INSERT has returned, so A holds RESERVED
	// from now until the commit below — the lock hold is established by the
	// statement's own return, never by timing luck.
	txA, err := a.Begin()
	if err != nil {
		t.Fatalf("A begin: %v", err)
	}
	if _, err := txA.Exec("INSERT INTO t (v) VALUES ('held')"); err != nil {
		t.Fatalf("A insert (acquire RESERVED): %v", err)
	}

	started := make(chan struct{})
	outCh := make(chan busyOutcome, 1)
	go func() {
		close(started)
		start := time.Now()
		_, err := b.Exec("INSERT INTO t (v) VALUES ('b')")
		outCh <- busyOutcome{err: err, elapsed: time.Since(start)}
	}()

	<-started
	time.Sleep(300 * time.Millisecond) // A's commit lands while B is mid-wait
	if err := txA.Commit(); err != nil {
		t.Fatalf("A commit: %v", err)
	}

	res := awaitBusyOutcome(t, "B's write", outCh, 8*time.Second)
	if res.err != nil {
		t.Fatalf("B's write failed after %v with %v -- want it to wait for A's commit and succeed", res.elapsed.Round(time.Millisecond), res.err)
	}
	if res.elapsed < 200*time.Millisecond {
		t.Fatalf("B's write succeeded after only %v -- it did not wait out A's hold at all", res.elapsed.Round(time.Millisecond))
	}
	t.Logf("B waited %v for A's commit and then succeeded", res.elapsed.Round(time.Millisecond))
}

// TestSQLiteBusyTimeout_HoldBeyondTheTimeoutFailsBounded proves the timeout
// is bounded and that its expiry is a real error, never an unbounded hang
// and never a silent retry: A holds the write lock for longer than
// defaultBusyTimeoutMS, and B's write must fail with SQLITE_BUSY only after
// waiting roughly the full timeout. The holder commits only after B has
// reported, so B's failure cannot be an artifact of A releasing early. The
// elapsed lower bound (~4s against the 5s timeout) is what distinguishes a
// bounded wait from a zero timeout, where the write would fail instantly.
//
// Like the other tests here, this pins contract semantics rather than a
// regression: the driver's implicit default supplied the same bounded wait
// before this change declared it, so the test passes on pre-change code and
// fails only if the bounded wait itself is gone — an immediate busy error,
// which takes the driver default and this package's parameter both
// disappearing.
func TestSQLiteBusyTimeout_HoldBeyondTheTimeoutFailsBounded(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "busy-timeout-bounded.db")
	a := openBusyTimeoutDB(t, dsn)
	b := openBusyTimeoutDB(t, dsn)
	createBusyTimeoutSchema(t, a)

	txA, err := a.Begin()
	if err != nil {
		t.Fatalf("A begin: %v", err)
	}
	if _, err := txA.Exec("INSERT INTO t (v) VALUES ('held')"); err != nil {
		t.Fatalf("A insert (acquire RESERVED): %v", err)
	}

	outCh := make(chan busyOutcome, 1)
	go func() {
		start := time.Now()
		_, err := b.Exec("INSERT INTO t (v) VALUES ('b')")
		outCh <- busyOutcome{err: err, elapsed: time.Since(start)}
	}()

	// A holds the lock until B reports. B can only report by failing after
	// its busy wait expires -- there is no other way out of this test for B.
	res := awaitBusyOutcome(t, "B's write", outCh, 10*time.Second)
	if res.err == nil {
		t.Fatalf("B's write succeeded after %v -- A never committed, so the busy timeout did not fire", res.elapsed.Round(time.Millisecond))
	}
	if res.elapsed < 4*time.Second {
		t.Fatalf("B's write failed after only %v with %v -- far short of the %d ms busy_timeout, so no bounded wait happened", res.elapsed.Round(time.Millisecond), res.err, defaultBusyTimeoutMS)
	}
	t.Logf("B waited ~%v and then failed with the real busy error: %v", res.elapsed.Round(time.Millisecond), res.err)

	// With B gone, A's commit succeeds immediately -- the failure B saw was
	// contention, not a wedged file.
	if err := txA.Commit(); err != nil {
		t.Fatalf("A commit after B's bounded failure: %v", err)
	}
}

// TestSQLiteBusyTimeout_ReadThenWriteUpgradeDoesNotWait pins the boundary
// withDefaultPragmas' doc comment records: a write that upgrades a
// transaction which has already read (holding the file's SHARED lock) is
// answered with an immediate SQLITE_BUSY when another connection holds the
// write lock, and no busy_timeout setting waits it out — SQLite refuses
// rather than enter the cycle where the holder cannot commit to EXCLUSIVE
// while this connection's SHARED lock stands. This is the lock shape of
// go/storage's derive gate (a read-then-write transaction), and it is why
// that path's busy failures under contention are fast and retry-converged
// rather than waiting — the same phenomenon recorded in AGENTS.md's
// "SQLite busy timeout" section. B reads first (SHARED acquired and held by
// its transaction), then writes; A holds RESERVED throughout. B must fail
// almost immediately with the busy error, in a small fraction of the
// timeout, while a plain contender in the tests above waits the same
// conflict out.
//
// This boundary holds regardless of any busy_timeout setting — SQLite's
// deadlock avoidance never consults the busy handler for an upgrade — so
// the test pins a database semantic and the accuracy of the doc comment
// that records it, not this change's own behavior: it passed before the
// default was declared and would pass under any timeout value.
func TestSQLiteBusyTimeout_ReadThenWriteUpgradeDoesNotWait(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "busy-timeout-upgrade.db")
	a := openBusyTimeoutDB(t, dsn)
	b := openBusyTimeoutDB(t, dsn)
	createBusyTimeoutSchema(t, a)

	txA, err := a.Begin()
	if err != nil {
		t.Fatalf("A begin: %v", err)
	}
	if _, err := txA.Exec("INSERT INTO t (v) VALUES ('held')"); err != nil {
		t.Fatalf("A insert (acquire RESERVED): %v", err)
	}

	txB, err := b.Begin()
	if err != nil {
		t.Fatalf("B begin: %v", err)
	}
	var v string
	if err := txB.QueryRow("SELECT v FROM t WHERE id = 1").Scan(&v); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("B read (acquire SHARED): %v", err)
	}

	outCh := make(chan busyOutcome, 1)
	go func() {
		start := time.Now()
		_, err := txB.Exec("INSERT INTO t (v) VALUES ('b')")
		outCh <- busyOutcome{err: err, elapsed: time.Since(start)}
	}()

	res := awaitBusyOutcome(t, "B's upgrade write", outCh, 8*time.Second)
	if res.err == nil {
		t.Fatalf("B's upgrade write succeeded after %v -- expected SQLite's deadlock avoidance to refuse it while A holds RESERVED", res.elapsed.Round(time.Millisecond))
	}
	if res.elapsed > 2*time.Second {
		t.Fatalf("B's upgrade write failed after %v -- the busy handler waited, contrary to SQLite's immediate-refusal semantics this test pins", res.elapsed.Round(time.Millisecond))
	}
	t.Logf("B's read-then-write upgrade failed after only %v (busy_timeout is %d ms) with: %v", res.elapsed.Round(time.Millisecond), defaultBusyTimeoutMS, res.err)

	// B's failed statement rolled back; its transaction still holds SHARED
	// from the read, so A cannot commit until B is done with it. Roll B back
	// and only then commit A.
	if err := txB.Rollback(); err != nil {
		t.Fatalf("B rollback: %v", err)
	}
	if err := txA.Commit(); err != nil {
		t.Fatalf("A commit: %v", err)
	}
}
