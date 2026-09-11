package dbkit

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestClose_ReleasesTheHandle pins Close's contract: after it returns, the
// underlying *sql.DB is closed for good -- a later use of the handle fails
// instead of silently opening fresh connections -- and a second Close is a
// no-op, so a shutdown path that reaches Close twice does not fail on the
// second call. It lives in package dbkit (an internal white-box test file,
// like open_test.go); the SQLite dialect is registered by example_test.go's
// external test package, which this file's test binary links in.
func TestClose_ReleasesTheHandle(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "dbkit-close-test.db")
	db, err := Open(context.Background(), Options{Dialect: DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	if closeErr := Close(db); closeErr != nil {
		t.Fatalf("Close() error = %v, want nil", closeErr)
	}

	err = db.Exec("SELECT 1").Error
	if err == nil {
		t.Fatal("Exec after Close() succeeded, want the closed-database error")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("Exec after Close() error = %v, want a closed-database error", err)
	}

	// database/sql's own DB.Close is documented to be idempotent; Close must
	// inherit that, not turn a repeated shutdown step into a failure.
	if err := Close(db); err != nil {
		t.Errorf("second Close() error = %v, want nil (Close is idempotent)", err)
	}
}

// TestClose_NilDBIsAPlainError pins the guard: a nil handle is a plain
// error naming the requirement, mirroring Apply's own non-nil requirement,
// rather than a nil-pointer panic from reaching into a handle that is not
// there.
func TestClose_NilDBIsAPlainError(t *testing.T) {
	err := Close(nil)
	if err == nil {
		t.Fatal("Close(nil) succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "non-nil") {
		t.Errorf("Close(nil) error = %v, want it to name the non-nil requirement", err)
	}
}
