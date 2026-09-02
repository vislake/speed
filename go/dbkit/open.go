package dbkit

import (
	"context"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// Dialect identifies which SQL dialect a connection speaks. dbkit supports
// exactly two, matching the project's dual-dialect requirement: PostgreSQL
// for the distributed deployment mode and SQLite for the standalone
// deployment mode and local development.
type Dialect string

const (
	// DialectPostgres selects the PostgreSQL driver (gorm.io/driver/postgres).
	DialectPostgres Dialect = "postgres"

	// DialectSQLite selects the pure-Go, CGO-free SQLite driver
	// (github.com/glebarez/sqlite).
	DialectSQLite Dialect = "sqlite"
)

// Connection-pool defaults applied by every connection Open returns. They
// are named constants, not inline literals, per the project's configuration
// standard: stable domain defaults belong in package-level constants, not
// scattered magic numbers. Options has no fields to override them on
// purpose — every *gorm.DB in this codebase shares the same pool shape, so
// there is exactly one place to reconsider them.
const (
	// defaultMaxOpenConns caps the number of open connections to the
	// database. SQLite serializes writers regardless of pool size, so for
	// DialectSQLite this mainly bounds reader concurrency; for
	// DialectPostgres it bounds the load one process places on the server.
	defaultMaxOpenConns = 25

	// defaultMaxIdleConns caps the number of idle connections kept warm, so
	// a burst of traffic does not pay a fresh connection's setup cost, while
	// not holding open far more connections than are ever used at once.
	defaultMaxIdleConns = 5

	// defaultConnMaxLifetime bounds how long a pooled connection is reused
	// before it is closed and replaced, so connections do not outlive an
	// intermediating proxy/load balancer or accumulate server-side session
	// state indefinitely.
	defaultConnMaxLifetime = 30 * time.Minute
)

// Options configures Open.
type Options struct {
	// Dialect selects which SQL dialect to connect with. It must be
	// DialectPostgres or DialectSQLite; any other value (including the zero
	// value) makes Open return an apperr.Invalid error.
	Dialect Dialect

	// DSN is the driver-specific data source name: a libpq/pgx connection
	// string for DialectPostgres (for example
	// "postgres://user:pass@host:5432/db?sslmode=disable"), or a SQLite DSN
	// for DialectSQLite (a file path, or "file::memory:?cache=shared" for an
	// in-memory database).
	//
	// DSN often carries credentials. Callers must not log it, and Open never
	// includes it in a returned error or elsewhere.
	DSN string
}

// Open opens a *gorm.DB for opts.Dialect and returns it already wired with
// dbkit's mandatory safeguards: sane connection-pool limits and the
// tenant-scoping GORM plugin (see tenant_scope.go).
//
// Open is the ONLY sanctioned way to obtain a *gorm.DB anywhere in this
// codebase. No code path in dbkit returns a *gorm.DB before the
// tenant-scoping plugin has been installed on it, so no caller can end up
// holding an "unprotected" handle by accident — the plugin is active before
// Open returns, not left for the caller to add. Business modules never call
// gorm.Open, postgres.Open, or sqlite.Open directly; they call Open and
// build a dbkit.Repository[T] on top of the connection it returns.
//
// Open validates opts.Dialect, opens the matching driver, applies the
// connection-pool defaults declared above, and verifies connectivity with a
// ctx-bound ping before returning, so a caller never receives a handle it
// cannot yet use. Every failure is returned as an *apperr.Error:
// apperr.Invalid for an unrecognized Dialect, apperr.Internal for a driver
// or connectivity failure. opts.DSN is never included in a returned error.
func Open(ctx context.Context, opts Options) (*gorm.DB, error) {
	dialector, err := newDialector(opts.Dialect, opts.DSN)
	if err != nil {
		return nil, err
	}

	db, err := gorm.Open(dialector, &gorm.Config{
		// dbkit cannot depend on the observability module (see the module
		// boundary rule in AGENTS.md), so it cannot route GORM's own SQL
		// logging through the structured logger the rest of the codebase
		// uses. Silencing it here, rather than letting GORM print to
		// stdout, keeps dbkit from emitting unstructured log lines on its
		// own; a caller who wants a query log takes it from the returned
		// error and logs that through its own context logger instead.
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
		// Both drivers implement gorm.ErrorTranslator, so this lets callers
		// (in particular Repository[T]) match driver-agnostic sentinels
		// such as gorm.ErrDuplicatedKey instead of dialect-specific errors.
		TranslateError: true,
		// Open does its own ping below, bound to ctx, instead of the
		// context-less ping gorm.Open would otherwise perform.
		DisableAutomaticPing: true,
	})
	if err != nil {
		return nil, wrapConnectError(opts.Dialect, err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, wrapConnectError(opts.Dialect, err)
	}

	sqlDB.SetMaxOpenConns(defaultMaxOpenConns)
	sqlDB.SetMaxIdleConns(defaultMaxIdleConns)
	sqlDB.SetConnMaxLifetime(defaultConnMaxLifetime)

	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, wrapConnectError(opts.Dialect, err)
	}

	if err := db.Use(newTenantScopePlugin()); err != nil {
		return nil, apperr.Internal("dbkit.tenant_scope_plugin_failed").
			WithParam("dialect", string(opts.Dialect)).
			WithCause(err)
	}

	return db, nil
}

// newDialector returns the gorm.Dialector matching dialect, or an
// apperr.Invalid error when dialect is neither DialectPostgres nor
// DialectSQLite. This is the single point where Dialect is validated, so
// every path through Open rejects an unknown dialect the same way.
func newDialector(dialect Dialect, dsn string) (gorm.Dialector, error) {
	switch dialect {
	case DialectPostgres:
		return postgres.Open(dsn), nil
	case DialectSQLite:
		return sqlite.Open(dsn), nil
	default:
		return nil, apperr.Invalid("dbkit.invalid_dialect").
			WithParam("dialect", string(dialect))
	}
}

// wrapConnectError wraps a driver- or connectivity-level failure from Open
// as an apperr.Internal error carrying the dialect for context. It never
// receives or includes the DSN, so a connection string's credentials can
// never leak into a returned error.
func wrapConnectError(dialect Dialect, cause error) error {
	return apperr.Internal("dbkit.connect_failed").
		WithParam("dialect", string(dialect)).
		WithCause(cause)
}
