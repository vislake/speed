// Package postgres registers the PostgreSQL driver
// (gorm.io/driver/postgres) with dbkit's dialect registry
// (dbkit.RegisterDialect), so that dbkit.Open(ctx, dbkit.Options{Dialect:
// dbkit.DialectPostgres, ...}) has a driver to build a gorm.Dialector
// from.
//
// This package exists purely for its init side effect. It exports nothing
// and is never referenced by name — a caller blank-imports it so the
// registration runs, and nothing else:
//
//	import _ "github.com/vislake/speed/go/dbkit/dialect/postgres"
//
// Splitting the driver out of dbkit's own go.mod into this subpackage
// means a consumer that only ever opens SQLite (or a test binary that
// only uses dbtest.NewSQLite) never pulls gorm.io/driver/postgres and its
// transitive dependencies into its build — see AGENTS.md's "One
// dependency, and why there is only one" section for the measured effect.
// A consumer that wants PostgreSQL blank-imports this package once,
// typically next to its dbkit.Open call site or in its module's main
// package.
package postgres

import (
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// init registers this package's dialector factory under
// dbkit.DialectPostgres. It panics (via dbkit.RegisterDialect) only on a
// duplicate registration — a programming error, never a runtime condition
// — so an ordinary single-import build never observes it.
func init() {
	dbkit.RegisterDialect(dbkit.DialectPostgres, func(dsn string) gorm.Dialector {
		return postgres.Open(dsn)
	})
}
