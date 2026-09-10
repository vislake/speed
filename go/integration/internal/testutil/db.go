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
// helper also a proof that those files run from zero -- mirroring
// go/pki/internal/testutil's identical helper.
func NewSQLite(t *testing.T, moduleName string, fs embed.FS) *gorm.DB {
	t.Helper()
	db := dbtest.NewSQLite(t)
	dbtest.Migrate(t, db, dbkit.DialectSQLite, dbtest.Migration{Module: moduleName, FS: fs})
	return db
}

// NewPostgres returns a migrated PostgreSQL *gorm.DB, backed by a real
// server started with testcontainers. It skips the test when no Docker
// daemon is reachable, so callers need no availability check of their own.
// No integration_test/ package calls it yet (the module's PostgreSQL leg,
// integration_test/postgres_softdelete_test.go, opens its own database);
// the helper exists so any future leg needs no db.go of its own.
func NewPostgres(t *testing.T, moduleName string, fs embed.FS) *gorm.DB {
	t.Helper()
	db := dbtest.NewPostgres(t)
	dbtest.Migrate(t, db, dbkit.DialectPostgres, dbtest.Migration{Module: moduleName, FS: fs})
	return db
}

// Migrate applies fs's <dialect>/*.sql files to db from zero, through
// dbkit.MigrationRegistry, failing the test on any error.
func Migrate(t *testing.T, db *gorm.DB, dialect dbkit.Dialect, moduleName string, fs embed.FS) {
	t.Helper()
	dbtest.Migrate(t, db, dialect, dbtest.Migration{Module: moduleName, FS: fs})
}
