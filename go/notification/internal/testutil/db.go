// Package testutil holds the database helpers the notification module's own
// tests share: a migrated, per-test SQLite database built from the module's
// real migration files (NewSQLite), and the Migrate step it rests on --
// each a thin spelling of dbkit/dbtest's shared mechanism (dbtest.Migrate).
// The helpers are deliberately free of any import of the notification
// module itself, so test code can migrate a database without dragging the
// module's declarations along.
package testutil

import (
	"embed"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
)

// NewSQLite returns a migrated SQLite *gorm.DB for a unit test: a fresh,
// per-call database from dbtest, with fs's sqlite/*.sql applied from zero
// through the real dbkit.MigrationRegistry.
//
// Applying the module's actual migration files, rather than an AutoMigrate
// or a hand-written CREATE TABLE, is what makes every test that uses this
// helper also a proof that those files run from zero -- the property the
// pre-commit checklist asks for and that AutoMigrate is banned for not
// providing.
func NewSQLite(t *testing.T, moduleName string, fs embed.FS) *gorm.DB {
	t.Helper()
	db := dbtest.NewSQLite(t)
	dbtest.Migrate(t, db, dbkit.DialectSQLite, dbtest.Migration{Module: moduleName, FS: fs})
	return db
}

// Migrate applies fs's <dialect>/*.sql files to db from zero, through
// dbkit.MigrationRegistry, failing the test on any error.
func Migrate(t *testing.T, db *gorm.DB, dialect dbkit.Dialect, moduleName string, fs embed.FS) {
	t.Helper()
	dbtest.Migrate(t, db, dialect, dbtest.Migration{Module: moduleName, FS: fs})
}
