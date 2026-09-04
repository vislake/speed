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
// helper also a proof that those files run from zero -- mirroring
// go/pki/internal/testutil's identical helper.
func NewSQLite(t *testing.T, moduleName string, fs embed.FS) *gorm.DB {
	t.Helper()
	db := dbtest.NewSQLite(t)
	Migrate(t, db, dbkit.DialectSQLite, moduleName, fs)
	return db
}

// NewPostgres returns a migrated PostgreSQL *gorm.DB, backed by a real
// server started with testcontainers. It skips the test when no Docker
// daemon is reachable, so callers need no availability check of their own.
// No integration_test/ package calls this yet in round 1 (see AGENTS.md's
// Known limitations); it is included now so a later round's PostgreSQL leg
// needs no db.go of its own.
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
