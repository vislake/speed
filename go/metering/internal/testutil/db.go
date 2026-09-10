package testutil

import (
	"embed"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
)

// NewSQLite returns a migrated SQLite *gorm.DB for a unit test: a fresh,
// per-call, temp-file database from dbtest, with fs's sqlite/*.sql applied
// from zero through the real dbkit.MigrationRegistry.
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

// NewPostgres returns a migrated PostgreSQL *gorm.DB, backed by a real
// server started with testcontainers. It skips the test when no Docker
// daemon is reachable, so callers need no availability check of their
// own.
//
// It is the integration tier's counterpart to NewSQLite and applies the
// identical migration files from the postgres/ subdirectory, so a test
// can run the same assertions against both dialects by swapping the
// constructor alone. It is what go/metering/integration_test/ calls from
// every test of the module's PostgreSQL tier, so the tier needs no db.go
// of its own -- mirroring go/pki/internal/testutil's identical choice for
// its own helper.
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
