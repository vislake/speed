// Package testutil holds the database helpers the notification module's own
// tests share: a migrated, per-test SQLite database built from the module's
// real migration files (NewSQLite), the PostgreSQL counterpart its
// integration tier will use (NewPostgres), and the Migrate step both rest
// on. The helpers are deliberately free of any import of the notification
// module itself, so test code can migrate a database without dragging the
// module's declarations along.
package testutil

import (
	"context"
	"embed"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
)

// NewSQLite returns a migrated SQLite *gorm.DB for a unit test: a fresh,
// per-call database from dbtest.NewSQLite (so it carries dbkit's full
// wiring, isolation plugin included), with fs's sqlite/*.sql applied from
// zero through the real dbkit.MigrationRegistry.
//
// Applying the module's actual migration files, rather than an AutoMigrate
// or a hand-written CREATE TABLE, is what makes every test that uses this
// helper also a proof that those files run from zero -- the property the
// pre-commit checklist asks for and that AutoMigrate is banned for not
// providing.
func NewSQLite(t *testing.T, moduleName string, fs embed.FS) *gorm.DB {
	t.Helper()
	db := dbtest.NewSQLite(t)
	Migrate(t, db, dbkit.DialectSQLite, moduleName, fs)
	return db
}

// NewPostgres returns a migrated PostgreSQL *gorm.DB, backed by a real
// server started with testcontainers. It skips the test when no Docker
// daemon is reachable, so callers need no availability check of their own.
//
// It is the integration tier's counterpart to NewSQLite and applies the
// identical migration files from the postgres/ subdirectory, so a test can
// run the same assertions against both dialects by swapping the constructor
// alone. notification has no caller of it yet: the module's PostgreSQL
// integration tier arrives in a later block of this round, and the helper
// ships now so that tier needs nothing but its own test files.
func NewPostgres(t *testing.T, moduleName string, fs embed.FS) *gorm.DB {
	t.Helper()
	db := dbtest.NewPostgres(t)
	Migrate(t, db, dbkit.DialectPostgres, moduleName, fs)
	return db
}

// Migrate applies fs's <dialect>/*.sql files to db from zero, through
// dbkit.MigrationRegistry, failing the test on any error.
func Migrate(t *testing.T, db *gorm.DB, dialect dbkit.Dialect, moduleName string, fs embed.FS) {
	t.Helper()
	registry := dbkit.NewMigrationRegistry()
	if err := registry.Register(migrationModule{name: moduleName, fs: fs}); err != nil {
		t.Fatalf("register migrations for %q: %v", moduleName, err)
	}
	if err := registry.Apply(context.Background(), db, dialect); err != nil {
		t.Fatalf("apply %s migrations for %q: %v", dialect, moduleName, err)
	}
}

// migrationModule is the minimal pkgcore.Module dbkit.MigrationRegistry
// needs in order to apply one module's migration files. It carries nothing
// but a name and the embedded files, which is exactly why this package does
// not have to import the module whose migrations it is applying.
type migrationModule struct {
	name string
	fs   embed.FS
}

func (m migrationModule) Name() string                       { return m.name }
func (m migrationModule) DependsOn() []string                { return nil }
func (m migrationModule) Migrations() embed.FS               { return m.fs }
func (m migrationModule) Locales() embed.FS                  { return embed.FS{} }
func (m migrationModule) OpenAPISpec() []byte                { return nil }
func (m migrationModule) Register(_ *pkgcore.Registry) error { return nil }

// compile-time check that migrationModule satisfies pkgcore.Module.
var _ pkgcore.Module = migrationModule{}
