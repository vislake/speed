package dbkit_test

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit"
	// Blank-imported so dbkit.Open has a driver to build a SQLite
	// gorm.Dialector from -- the same reason every other dbkit test file
	// that opens a real SQLite connection imports it.
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
)

// TestIsRetryableConflict_SyntheticErrors pins the classifier's contract
// against plain errors.New values shaped like each driver's real wording,
// for the cases the real-driver tests below cannot cheaply parameterize
// (an unrelated error must never be misclassified).
func TestIsRetryableConflict_SyntheticErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"sqlite busy code", errors.New(`database is locked (5) (SQLITE_BUSY)`), true},
		{"sqlite busy wording only", errors.New("database is locked"), true},
		{"postgres deadlock", errors.New(`ERROR: deadlock detected (SQLSTATE 40P01)`), true},
		{"postgres serialization failure", errors.New(`ERROR: could not serialize access due to concurrent update (SQLSTATE 40001)`), true},
		{"unrelated not-found", dbkit.ErrRecordNotFound, false},
		{"unrelated wrapped error", fmt.Errorf("dbkit: %w", errors.New("connection refused")), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := dbkit.IsRetryableConflict(tc.err); got != tc.want {
				t.Errorf("IsRetryableConflict(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsRetryableConflict_RealSQLiteBusy forces a genuine SQLITE_BUSY out of
// two real connections to the same file -- the identical two-connection
// contending-writer rig go/dbkit/dialect/sqlite/dialect_sqlite_test.go's own
// TestSQLiteBusyTimeout_HoldBeyondTheTimeoutFailsBounded uses, with a near-
// zero busy_timeout here so the test does not have to wait out the real
// 5-second default -- and proves IsRetryableConflict recognizes the actual
// error the real driver returns, not just a synthetic stand-in.
func TestIsRetryableConflict_RealSQLiteBusy(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "retry-busy.sqlite")

	a := openRetryTestDB(t, dsn)
	b := openRetryTestDB(t, dsn)
	if _, err := a.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	txA, err := a.Begin()
	if err != nil {
		t.Fatalf("A begin: %v", err)
	}
	if _, err := txA.Exec(`INSERT INTO t (v) VALUES ('held')`); err != nil {
		t.Fatalf("A insert (acquire RESERVED): %v", err)
	}
	defer func() { _ = txA.Rollback() }()

	var wg sync.WaitGroup
	var bErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, bErr = b.Exec(`INSERT INTO t (v) VALUES ('b')`)
	}()
	// Give B's write a moment to actually attempt and hit the held lock
	// before we let A go; B's own busy_timeout (set to 200ms below) bounds
	// how long this can take regardless.
	time.Sleep(50 * time.Millisecond)
	wg.Wait()

	if bErr == nil {
		t.Fatalf("B's write unexpectedly succeeded -- A's transaction was never committed or rolled back yet, so nothing should have been able to write")
	}
	if !dbkit.IsRetryableConflict(bErr) {
		t.Fatalf("IsRetryableConflict(%v) = false, want true for a real SQLITE_BUSY", bErr)
	}
}

// openRetryTestDB opens one *sql.DB connection to dsn with a short
// busy_timeout, bypassing dbkit.Open's own defaults so this test does not
// have to wait out the real 5-second production default to observe the
// busy failure.
func openRetryTestDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn+"?_pragma=busy_timeout(200)")
	if err != nil {
		t.Fatalf("open %s: %v", dsn, err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}
