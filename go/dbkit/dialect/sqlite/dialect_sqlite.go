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
		return glebarezsqlite.Open(dsn)
	})
}
