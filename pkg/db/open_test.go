package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// These cases live in package db rather than beside the others in db_test: the
// opener is unexported, and the text it produces has to be read on its own. A
// host's wrap around it prefixes the module name, and the module name is where
// the dialect would appear, so a case reading that text from outside would
// agree with an implementation that named no dialect at all.

// locatorSecret stands where a locator carries credentials. Nothing this
// package reports may contain it: these errors reach the startup diagnostics,
// and the diagnostics are written to a stream the host cannot redirect.
const locatorSecret = "hunter2-must-not-be-echoed"

// sqliteSpec is an implementation of the shape every subpackage has, bound to
// the pure-Go SQLite driver.
//
// The driver is the real one rather than a stand-in: what these cases observe
// is the driver's behaviour — that it refuses a locator, and what its refusal
// says — and a stub would agree with any expectation. The implementation
// subpackage is not imported, because its init registers a module in the
// process registry and would take part in every other case in this binary.
func sqliteSpec() Spec {
	return Spec{
		ModuleName:      sqliteNamespace,
		ConfigNamespace: sqliteNamespace,
		Dialect:         SQLite,
		Dialector:       sqlite.Open,
	}
}

// sqliteSection is the configuration this implementation runs on, with the
// pool parameters an assembly that configured only a locator has.
func sqliteSection(dsn string) lockingConfig {
	return lockingConfig{Config: Config{
		DSN:             dsn,
		MaxOpenConns:    defaultMaxOpenConns,
		MaxIdleConns:    defaultMaxIdleConns,
		ConnMaxLifetime: defaultConnMaxLifetime,
	}}
}

// unopenableDSN is a locator the driver refuses: the directory it names does
// not exist. The secret stands where a locator carries its credentials.
func unopenableDSN(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), locatorSecret, "db.sqlite")
}

// poolOf reaches the connection pool behind a session.
func poolOf(t *testing.T, handle *gorm.DB) *sql.DB {
	t.Helper()
	pool, err := handle.DB()
	if err != nil {
		t.Fatalf("reaching the connection pool: %v", err)
	}
	return pool
}

// releaseAtEnd gives a pool back when the case ends, so that a case which
// asserted on what the pool did does not leave it open for the rest of the run.
func releaseAtEnd(t *testing.T, pool *sql.DB) {
	t.Helper()
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("releasing the connection pool: %v", err)
		}
	})
}

// TestConnectFailureIsErrConnectFailed pins the sentinel a locator the driver
// cannot open reports.
func TestConnectFailureIsErrConnectFailed(t *testing.T) {
	_, err := openHandle(t.Context(), sqliteSpec(), sqliteSection(unopenableDSN(t)))
	if !errors.Is(err, ErrConnectFailed) {
		t.Fatalf("opening a locator the driver refuses gave %v, want ErrConnectFailed", err)
	}
}

// TestConnectFailureDoesNotEchoTheLocator keeps the credentials a locator
// carries out of the error text, which is written to the startup diagnostics
// and which the host cannot redirect.
//
// The dialect and the step are what has to be in the text instead: without
// them the reader knows a connection failed and nothing about which one or
// where to look.
func TestConnectFailureDoesNotEchoTheLocator(t *testing.T) {
	_, err := openHandle(t.Context(), sqliteSpec(), sqliteSection(unopenableDSN(t)))
	if err == nil {
		t.Fatal("opening a locator whose directory does not exist succeeded")
	}
	text := err.Error()
	if strings.Contains(text, locatorSecret) {
		t.Errorf("the error echoes the locator, which carries the credentials it is refused "+
			"for: %v", err)
	}
	if !strings.Contains(text, string(SQLite)) {
		t.Errorf("the error does not name the dialect: %v", err)
	}
	if !strings.Contains(text, "opening") {
		t.Errorf("the error does not name the step that failed: %v", err)
	}
}

// TestConnectFailureNamesTheConnectivityStep pins the second step a connection
// establishment has, and that it is bound to the context the stage received.
//
// A cancelled context is what makes the step observable: the driver's own open
// answers an already-cancelled context — SQLite is opened through a file, with
// no round trip to bind — so an implementation that hands the session back
// without checking that the database answers passes every other case here and
// fails this one. Where a host wants a bound on the wait, that context is the
// only place to put it: the driver's timeouts are written in the locator and
// this package declares no input item for them.
func TestConnectFailureNamesTheConnectivityStep(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := openHandle(ctx, sqliteSpec(), sqliteSection(filepath.Join(t.TempDir(), "cancelled.db")))
	if !errors.Is(err, ErrConnectFailed) {
		t.Fatalf("connecting under a cancelled context gave %v, want ErrConnectFailed", err)
	}
	if text := err.Error(); !strings.Contains(text, "connectivity check") {
		t.Errorf("the error does not name the connectivity check as the step that failed: %v", err)
	}
}

// TestPoolParametersReachTheConnection pins that the largest number of
// connections the pool opens is the configured one.
//
// Left unset it is not a conservative default: database/sql reads 0 as no limit
// at all, so a handle that dropped the parameter grows a connection per
// concurrent statement and holds each one for good.
func TestPoolParametersReachTheConnection(t *testing.T) {
	cfg := sqliteSection(filepath.Join(t.TempDir(), "pool.db"))
	cfg.MaxOpenConns = 7

	handle, err := openHandle(t.Context(), sqliteSpec(), cfg)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	pool := poolOf(t, handle)
	releaseAtEnd(t, pool)
	if got := pool.Stats().MaxOpenConnections; got != 7 {
		t.Errorf("the pool opens at most %d connections, want the configured 7", got)
	}
}

// TestIdleConnectionsReachTheConnection pins the other end of the pool: how
// many connections are kept once they are no longer in use.
//
// It is observed by holding more connections at once than the configuration
// keeps, then giving them all back: what stays idle is the configured number,
// and an implementation that dropped the parameter keeps the number
// database/sql defaults to, which is neither the configured value nor a safe
// one to leave in its place.
//
// The value is deliberately not 2: that is the default an unset pool carries,
// and a case that configured it would pass on an implementation that never
// touched the parameter.
func TestIdleConnectionsReachTheConnection(t *testing.T) {
	ctx := t.Context()
	cfg := sqliteSection(filepath.Join(t.TempDir(), "idle.db"))
	cfg.MaxOpenConns = 8
	cfg.MaxIdleConns = 3

	handle, err := openHandle(ctx, sqliteSpec(), cfg)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	pool := poolOf(t, handle)
	releaseAtEnd(t, pool)

	held := make([]*sql.Conn, 0, 8)
	for range 8 {
		conn, err := pool.Conn(ctx)
		if err != nil {
			t.Fatalf("taking connection %d of 8: %v", len(held)+1, err)
		}
		held = append(held, conn)
	}
	for _, conn := range held {
		if err := conn.Close(); err != nil {
			t.Fatalf("giving a connection back: %v", err)
		}
	}
	if got := pool.Stats().Idle; got != 3 {
		t.Errorf("the pool kept %d connections idle, want the configured 3", got)
	}
}

// TestConnMaxLifetimeReachesTheConnection pins the third parameter: how long a
// connection is reused before it is replaced.
//
// A lifetime of a nanosecond expires every connection the moment it is given
// back, so a ping is enough to replace one. An implementation that dropped the
// parameter replaces nothing, ever: the connection stays for the life of the
// process, past whatever the server or a proxy between them is willing to keep.
func TestConnMaxLifetimeReachesTheConnection(t *testing.T) {
	ctx := t.Context()
	cfg := sqliteSection(filepath.Join(t.TempDir(), "lifetime.db"))
	cfg.ConnMaxLifetime = time.Nanosecond

	handle, err := openHandle(ctx, sqliteSpec(), cfg)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	pool := poolOf(t, handle)
	releaseAtEnd(t, pool)

	for round := 1; round <= 3; round++ {
		if err := pool.PingContext(ctx); err != nil {
			t.Fatalf("pinging after round %d: %v", round, err)
		}
	}
	if got := pool.Stats().MaxLifetimeClosed; got == 0 {
		t.Error("no connection was replaced once its lifetime had passed, so conn-max-lifetime " +
			"never reached the pool")
	}
}
