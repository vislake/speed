// Package testutil holds shared test helpers for go/admin's own test
// files -- a dedicated package rather than helpers scattered across test
// files, per root CLAUDE.md's testing rule.
package testutil

import (
	"embed"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/admin/migrations"
)

// migrationModule is the minimal pkgcore.Module NewDB feeds to
// dbkit.MigrationRegistry, carrying admin's real migration files. It
// exists because the registry works in terms of modules, and building the
// real admin.Module here would create an import cycle (this package is
// imported BY admin's own tests).
type migrationModule struct{}

func (migrationModule) Name() string                     { return "admin" }
func (migrationModule) DependsOn() []string              { return nil }
func (migrationModule) Migrations() embed.FS             { return migrations.FS }
func (migrationModule) Locales() embed.FS                { return embed.FS{} }
func (migrationModule) OpenAPISpec() []byte              { return nil }
func (migrationModule) Register(*pkgcore.Registry) error { return nil }

// NewDB returns a fresh in-memory SQLite database with every admin
// migration applied from zero.
func NewDB(t *testing.T) *gorm.DB {
	t.Helper()

	db := dbtest.NewSQLite(t)

	registry := dbkit.NewMigrationRegistry()
	if err := registry.Register(migrationModule{}); err != nil {
		t.Fatalf("register admin's migrations: %v", err)
	}
	if err := registry.Apply(t.Context(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("apply admin's migrations from zero: %v", err)
	}
	return db
}
