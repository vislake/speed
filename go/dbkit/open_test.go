package dbkit

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// This file (package dbkit, an internal white-box test file) cannot
// blank-import a dbkit/dialect subpackage itself -- dialect/postgres and
// dialect/sqlite both import dbkit, so that would be an import cycle. Both
// dialects it needs are instead registered by example_test.go, an external
// (package dbkit_test) file in this same directory whose test binary this
// file is linked into.

func TestOpen_SQLiteTempFileDSN_OpensAndPings(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "dbkit-open-test.db")

	db, err := Open(context.Background(), Options{Dialect: DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if db == nil {
		t.Fatal("Open() returned a nil *gorm.DB alongside a nil error")
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB() error = %v", err)
	}
	if err := sqlDB.PingContext(context.Background()); err != nil {
		t.Errorf("Ping() error = %v, want the connection Open returned to be reachable", err)
	}

	// Open's central contract: nothing it returns may ever be "unprotected".
	// This does not assert which plugin or how many; that is
	// tenant_scope.go's own tests to make. It only guards the invariant that
	// Open itself is responsible for: at least one plugin (the
	// tenant-scoping one) is always installed before Open returns.
	if len(db.Plugins) == 0 {
		t.Error("Open() returned a *gorm.DB with no plugins installed; the tenant-scoping plugin must always be active")
	}
}

func TestOpen_InvalidDialect_ReturnsInvalidError(t *testing.T) {
	tests := []struct {
		name    string
		dialect Dialect
	}{
		{name: "unrecognized dialect value", dialect: Dialect("mysql")},
		{name: "empty dialect (zero value)", dialect: Dialect("")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, err := Open(context.Background(), Options{Dialect: tt.dialect, DSN: "irrelevant"})
			if err == nil {
				t.Fatal("Open() error = nil, want an error for an unrecognized dialect")
			}
			if db != nil {
				t.Errorf("Open() *gorm.DB = %v, want nil alongside a non-nil error", db)
			}

			appErr, ok := apperr.As(err)
			if !ok {
				t.Fatalf("Open() error = %v (%T), want an *apperr.Error", err, err)
			}
			if appErr.Status != http.StatusBadRequest {
				t.Errorf("Open() error Status = %d, want %d (apperr.Invalid)", appErr.Status, http.StatusBadRequest)
			}
			if appErr.Code != "dbkit.invalid_dialect" {
				t.Errorf("Open() error Code = %q, want %q", appErr.Code, "dbkit.invalid_dialect")
			}
		})
	}
}

func TestOpen_PostgresGarbageDSN_ReturnsError(t *testing.T) {
	// A garbage DSN must fail fast without a real Postgres server: either
	// gorm.Open itself rejects it while parsing the connection config, or
	// the ctx-bound ping that follows fails to reach anything. The timeout
	// here is a safety net so this test cannot hang the suite if some
	// future driver version ever treats the DSN as well-formed but merely
	// unreachable. Proving Open's plumbing surfaces a driver failure as a
	// usable error is all this case is responsible for; exercising a real
	// PostgreSQL server is the independent integration test phase's job.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	db, err := Open(ctx, Options{Dialect: DialectPostgres, DSN: "not-a-valid-postgres-dsn"})
	if err == nil {
		t.Fatal("Open() error = nil, want an error for a garbage Postgres DSN")
	}
	if db != nil {
		t.Errorf("Open() *gorm.DB = %v, want nil alongside a non-nil error", db)
	}

	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("Open() error = %v (%T), want an *apperr.Error", err, err)
	}
	if appErr.Status != http.StatusInternalServerError {
		t.Errorf("Open() error Status = %d, want %d (apperr.Internal)", appErr.Status, http.StatusInternalServerError)
	}
	if appErr.Code != "dbkit.connect_failed" {
		t.Errorf("Open() error Code = %q, want %q", appErr.Code, "dbkit.connect_failed")
	}
}

// TestOpen_Name_MatchesDialect pins the exact string db.Name() reports for
// each dialect Open supports, against the literal strings "sqlite" and
// "postgres" — not against the DialectSQLite/DialectPostgres constants —
// because tenant_session.go's isPostgres detection is exactly this
// comparison: "db.Name() == string(DialectPostgres)". Comparing against the
// literal here means this test fails on either half of that comparison
// silently drifting: a GORM driver upgrade that changed what its
// Dialector.Name() returns, or a future edit to the DialectPostgres/
// DialectSQLite constants themselves. Either one would make WithTenantSession
// silently stop setting (or start wrongly attempting) PostgreSQL's
// RLS-backing session GUC — a real, quiet isolation regression — and this
// fast, dependency-free unit test exists to catch it on every commit,
// instead of relying solely on the Docker-backed integration suite to
// eventually notice.
//
// The sqlite case round-trips through the real Open(...) entry point, since
// SQLite needs no live server and Open can run in full (dial, ping and all)
// inside a plain unit test, exactly like TestOpen_SQLiteTempFileDSN_OpensAndPings
// above.
//
// The postgres case cannot do the same: Open's own contract is to verify
// connectivity with a ctx-bound ping before ever returning (see Open's doc
// comment), and this module's unit tier carries no external dependency such
// as a running PostgreSQL server (backend coding standards §13, "Unit |
// None") — that live-server path is already exercised end-to-end by
// integration_test/postgres_tenant_isolation_test.go and its siblings. So
// this instead calls newDialector, the exact line of Open's own body that
// produces the Dialector whose Name() Open (and in turn WithTenantSession)
// reads, without the network-dependent ping that follows it in Open itself.
// This is the identical technique WithTenantSession's own doc comment
// describes using to confirm this same contract by hand: "opening each
// dialector directly and calling Name() on it".
func TestOpen_Name_MatchesDialect(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		dsn := filepath.Join(t.TempDir(), "dbkit-open-name-test.db")
		db, err := Open(context.Background(), Options{Dialect: DialectSQLite, DSN: dsn})
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		if got, want := db.Name(), "sqlite"; got != want {
			t.Errorf("db.Name() = %q, want %q", got, want)
		}
	})

	t.Run("postgres", func(t *testing.T) {
		dialector, err := newDialector(DialectPostgres, "irrelevant")
		if err != nil {
			t.Fatalf("newDialector() error = %v", err)
		}
		if got, want := dialector.Name(), "postgres"; got != want {
			t.Errorf("Dialector.Name() = %q, want %q", got, want)
		}
	})
}
