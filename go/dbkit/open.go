package dbkit

import (
	"context"
	"sync"
	"time"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/vislake/speed/go/pkgcore"
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

	// AuditBus, when non-nil, installs dbkit's automatic write-capture
	// plugin (audit_capture.go): every Create, Update or Delete against a
	// model implementing Auditable publishes a WriteCapturedEvent on it.
	// The zero value, nil, is what every call site that existed before this
	// field did — and still does when it does not opt in — so leaving it
	// unset installs no capture at all, exactly as before. A real host
	// wires reg.EventBus() here once the audit persister module
	// (go/dbkit/audit) is part of its module set; see that package's
	// AGENTS.md for the end-to-end wiring.
	AuditBus pkgcore.EventBus
}

// Open opens a *gorm.DB for opts.Dialect and returns it already wired with
// dbkit's mandatory safeguards: sane connection-pool limits, the
// tenant-scoping GORM plugin (see tenant_scope.go), and the soft-delete
// auto-scope GORM plugin (see soft_delete.go) — both installed
// unconditionally, like tenant scoping, since each is a per-model opt-in
// marker interface rather than a global switch: a model implementing
// neither TenantScoped nor SoftDeletable is completely unaffected by
// either.
//
// Open is the ONLY sanctioned way to obtain a *gorm.DB anywhere in this
// codebase. No code path in dbkit returns a *gorm.DB before the
// tenant-scoping and soft-delete plugins have been installed on it, so no
// caller can end up holding an "unprotected" handle by accident — both
// plugins are active before Open returns, not left for the caller to add.
// Business modules never call gorm.Open, postgres.Open, or sqlite.Open
// directly; they call Open and build a dbkit.Repository[T] on top of the
// connection it returns.
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

	if err := db.Use(newSoftDeleteScopePlugin()); err != nil {
		return nil, apperr.Internal("dbkit.soft_delete_scope_plugin_failed").
			WithParam("dialect", string(opts.Dialect)).
			WithCause(err)
	}

	if opts.AuditBus != nil {
		if err := db.Use(newAuditCapturePlugin(opts.AuditBus)); err != nil {
			return nil, apperr.Internal("dbkit.audit_capture_plugin_failed").
				WithParam("dialect", string(opts.Dialect)).
				WithCause(err)
		}
	}

	return db, nil
}

// dialectMu guards dialectRegistry.
var dialectMu sync.RWMutex

// dialectRegistry maps a Dialect to the factory that builds its
// gorm.Dialector from a DSN. It starts empty: dbkit's own go.mod no longer
// imports either driver directly (see "One dependency, and why there is
// only one" in AGENTS.md), so a driver is only registered when its
// dbkit/dialect subpackage is blank-imported.
var dialectRegistry = map[Dialect]func(dsn string) gorm.Dialector{}

// RegisterDialect registers factory as the gorm.Dialector builder for
// dialect, mirroring database/sql.Register's driver-registration pattern.
// It is called from a dbkit/dialect subpackage's own init() — never
// directly by a business module or application — so that dbkit's own
// go.mod carries neither database driver as a direct dependency, and a
// consumer that wants only one dialect's dependencies imports only that
// dialect's subpackage:
//
//	import _ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
//
// RegisterDialect panics if dialect is already registered. A duplicate
// registration can only be a programming error — two packages registering
// the same Dialect name — never a runtime condition a caller could
// encounter or would want to recover from, the same unrecoverable-wiring-
// error convention pkgcore/builtin_implementations.go's mustRegister
// documents for the identical situation.
func RegisterDialect(dialect Dialect, factory func(dsn string) gorm.Dialector) {
	dialectMu.Lock()
	defer dialectMu.Unlock()

	if _, exists := dialectRegistry[dialect]; exists {
		panic("dbkit: RegisterDialect called twice for dialect " + string(dialect))
	}
	dialectRegistry[dialect] = factory
}

// newDialector returns the gorm.Dialector matching dialect, or an
// apperr.Invalid error when dialect is neither DialectPostgres nor
// DialectSQLite, or when the matching dialect subpackage was never
// blank-imported. This is the single point where Dialect is validated, so
// every path through Open rejects an unknown or unregistered dialect the
// same way.
func newDialector(dialect Dialect, dsn string) (gorm.Dialector, error) {
	if dialect != DialectPostgres && dialect != DialectSQLite {
		return nil, apperr.Invalid("dbkit.invalid_dialect").
			WithParam("dialect", string(dialect))
	}

	dialectMu.RLock()
	factory, ok := dialectRegistry[dialect]
	dialectMu.RUnlock()

	if !ok {
		return nil, apperr.Invalid("dbkit.invalid_dialect").
			WithParam("dialect", string(dialect)).
			WithParam("hint", "no driver registered for this dialect -- blank-import its package, e.g. _ \"github.com/vislake/speed/go/dbkit/dialect/sqlite\" or _ \"github.com/vislake/speed/go/dbkit/dialect/postgres\"")
	}
	return factory(dsn), nil
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
