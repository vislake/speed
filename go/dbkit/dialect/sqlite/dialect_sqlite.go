// Package sqlite registers the pure-Go, CGO-free SQLite driver
// (github.com/glebarez/sqlite) with dbkit's dialect registry
// (dbkit.RegisterDialect), so that dbkit.Open(ctx, dbkit.Options{Dialect:
// dbkit.DialectSQLite, ...}) has a driver to build a gorm.Dialector from.
//
// This package exists purely for its init side effect. It exports nothing
// and is never referenced by name — a caller blank-imports it so the
// registration runs, and nothing else:
//
//	import _ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
//
// Splitting the driver out of dbkit's own go.mod into this subpackage
// means a consumer that only ever opens PostgreSQL (or a test binary that
// only uses dbtest.NewPostgres) never pulls github.com/glebarez/sqlite and
// its transitive dependencies into its build — see AGENTS.md's "One
// dependency, and why there is only one" section for the measured effect.
// A consumer that wants SQLite blank-imports this package once, typically
// next to its dbkit.Open call site or in its module's main package.
package sqlite

import (
	"strconv"
	"strings"

	glebarezsqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// defaultBusyTimeoutMS is the bounded busy_timeout, in milliseconds, this
// package applies to every SQLite connection dbkit.Open returns. See
// withDefaultPragmas' doc comment for the full rationale — why it is a
// fixed package default, why it is bounded, and what it can and cannot do
// for writers contending on one SQLite file.
const defaultBusyTimeoutMS = 5000

// init registers this package's dialector factory under dbkit.DialectSQLite.
// It panics (via dbkit.RegisterDialect) only on a duplicate registration —
// a programming error, never a runtime condition — so an ordinary
// single-import build never observes it.
func init() {
	dbkit.RegisterDialect(dbkit.DialectSQLite, func(dsn string) gorm.Dialector {
		return glebarezsqlite.Open(withDefaultPragmas(dsn))
	})
}

// withDefaultPragmas appends the go-sqlite driver's "_pragma" DSN query
// parameters this package applies to every connection, to dsn:
//
//	_pragma=recursive_triggers(1)
//	_pragma=busy_timeout(5000)   // defaultBusyTimeoutMS
//
// Both are per-connection settings, never per-database-file ones, and
// dbkit.Open's pool can hand out any number of physical connections
// (defaultMaxOpenConns), so setting either once after Open returns would
// leave every connection opened after that one call unprotected. Folding
// them into the DSN itself is what makes them apply to every physical
// connection glebarez/sqlite's driver opens for this *gorm.DB, for the
// lifetime of the pool: the driver re-applies every "_pragma" DSN
// parameter on each new connection it opens (verified against
// github.com/glebarez/go-sqlite's own newConn/applyQueryParams, which parse
// and apply "_pragma" params afresh per Driver.Open call, not once per
// process) — see go/dbkit/audit/AGENTS.md's "Append-only enforcement" and
// "Known limitations" sections for the reachable-only-via-raw-SQL context
// recursive_triggers closes, and AGENTS.md's "SQLite busy timeout" section
// for the contention context busy_timeout addresses.
//
// recursive_triggers: SQLite's recursive_triggers pragma defaults OFF, and
// go/dbkit/audit's append-only triggers
// (migrations/sqlite/0002_append_only_enforcement.sql) depend on it being
// ON: with it left at the default, SQLite's legacy "INSERT OR REPLACE"
// upsert silently performs its implicit conflict-row delete WITHOUT firing
// the table's DELETE triggers at all — so a raw "INSERT OR REPLACE INTO
// audit_events (...) VALUES (...)" against an existing id would overwrite
// that row's columns with no error and no trigger firing, defeating the
// append-only guarantee through a path the trigger itself cannot see.
// Ordinary UPDATE/DELETE statements are unaffected either way —
// recursive_triggers only changes whether a legacy INSERT OR REPLACE's
// implicit delete fires triggers and whether a trigger's own writes can
// fire further triggers; a plain UPDATE or DELETE fires a matching
// BEFORE/AFTER trigger regardless of this pragma, on or off. Turning it on
// is the standard, SQLite-documented setting for exactly this immutability
// case and has no other effect on this codebase today, since no other
// table in dbkit or its consumers currently carries a trigger.
//
// busy_timeout: SQLite serializes writers on one database file, and when a
// connection's write meets another connection's uncommitted write the
// loser normally gets an immediate SQLITE_BUSY error. This pragma installs
// SQLite's built-in busy handler: the losing connection waits up to
// defaultBusyTimeoutMS for the holder to commit or roll back, retrying in
// progressively longer sleeps, and only then fails with the real
// SQLITE_BUSY error — a bounded wait, never an unbounded one and never a
// silent retry: a write that still cannot proceed after the timeout
// surfaces as an ordinary error for the caller (and its retry layer, e.g.
// go/jobs' backoff) to handle.
//
// The default is deliberately fixed, like the connection-pool defaults in
// dbkit.Open — Options has no field to override it, every *gorm.DB in this
// codebase shares the same pool shape, and there is exactly one place to
// reconsider it. The value 5000ms is the driver's own historical default
// (glebarez/go-sqlite's applyQueryParams applies `pragma BUSY_TIMEOUT(5000)`
// to every new connection unless the DSN overrides it), so making it
// explicit here changes no behavior today; it turns a default that lived
// inside a dependency into a contract this package declares, documents and
// tests, so a future driver change cannot silently strip or alter it. It
// also sits last in the DSN's "_pragma" list, and the driver applies
// "_pragma" params in DSN order with the last application winning, so a
// caller-supplied `_pragma=busy_timeout(...)` in dsn is deliberately
// overridden — one default, one place to reconsider it.
//
// What a busy timeout can and cannot do is bounded by SQLite's own
// rollback-journal lock protocol. A write that starts from no lock (an
// autocommit write, or the first write of a transaction that has read
// nothing yet) waits through the busy handler and succeeds once the holder
// commits — ordinary contention. A write inside a transaction that has
// already read (holding the file's SHARED lock) and is upgrading to write
// does NOT wait: SQLite answers that upgrade with an immediate SQLITE_BUSY
// whenever another connection holds the write lock, because waiting could
// not resolve the cycle — the holder cannot commit to EXCLUSIVE while this
// connection's SHARED lock stands. No busy_timeout setting changes that
// immediate refusal; the caller's own retry layer is the only cure, which
// is why go/storage's derive gate (a read-then-write transaction) records
// fast, retry-converged busy failures under contention — see AGENTS.md's
// "SQLite busy timeout" section for the boundary spelled out.
func withDefaultPragmas(dsn string) string {
	const pragmas = "_pragma=recursive_triggers(1)&_pragma=busy_timeout("
	busy := strconv.Itoa(defaultBusyTimeoutMS)

	sep := "?"
	if strings.ContainsRune(dsn, '?') {
		sep = "&"
	}
	return dsn + sep + pragmas + busy + ")"
}
