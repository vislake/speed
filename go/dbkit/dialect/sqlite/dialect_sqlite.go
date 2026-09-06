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
	"strings"

	glebarezsqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// init registers this package's dialector factory under dbkit.DialectSQLite.
// It panics (via dbkit.RegisterDialect) only on a duplicate registration —
// a programming error, never a runtime condition — so an ordinary
// single-import build never observes it.
func init() {
	dbkit.RegisterDialect(dbkit.DialectSQLite, func(dsn string) gorm.Dialector {
		return glebarezsqlite.Open(withRecursiveTriggers(dsn))
	})
}

// withRecursiveTriggers appends the go-sqlite driver's "_pragma" DSN query
// parameter that turns PRAGMA recursive_triggers on, to dsn.
//
// SQLite's recursive_triggers pragma defaults OFF, and go/dbkit/audit's
// append-only triggers (migrations/sqlite/0002_append_only_enforcement.sql)
// depend on it being ON: with it left at the default, SQLite's legacy
// "INSERT OR REPLACE" upsert silently performs its implicit conflict-row
// delete WITHOUT firing the table's DELETE triggers at all — so a raw
// "INSERT OR REPLACE INTO audit_events (...) VALUES (...)" against an
// existing id would overwrite that row's columns with no error and no
// trigger firing, defeating the append-only guarantee through a path the
// trigger itself cannot see. Recursive_triggers is a per-connection
// setting, never a per-database-file one, and dbkit.Open's pool can hand
// out any number of physical connections (defaultMaxOpenConns), so setting
// it once after Open returns would leave every connection opened after
// that one call unprotected. Folding it into the DSN itself is what makes
// it apply to every physical connection glebarez/sqlite's driver opens for
// this *gorm.DB, for the lifetime of the pool: the driver re-applies every
// "_pragma" DSN parameter on each new connection it opens (verified against
// github.com/glebarez/go-sqlite's own newConn/applyQueryParams, which parse
// and apply "_pragma" params afresh per Driver.Open call, not once per
// process) — see go/dbkit/audit/AGENTS.md's "Append-only enforcement" and
// "Known limitations" sections for the reachable-only-via-raw-SQL context
// this closes.
//
// Ordinary UPDATE/DELETE statements are unaffected either way —
// recursive_triggers only changes whether a legacy INSERT OR REPLACE's
// implicit delete fires triggers and whether a trigger's own writes can
// fire further triggers; a plain UPDATE or DELETE fires a matching
// BEFORE/AFTER trigger regardless of this pragma, on or off. Turning it on
// is the standard, SQLite-documented setting for exactly this immutability
// case and has no other effect on this codebase today, since no other
// table in dbkit or its consumers currently carries a trigger.
func withRecursiveTriggers(dsn string) string {
	const pragma = "_pragma=recursive_triggers(1)"

	sep := "?"
	if strings.ContainsRune(dsn, '?') {
		sep = "&"
	}
	return dsn + sep + pragma
}
