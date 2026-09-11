package dbkit

import (
	"context"
	"path/filepath"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit/internal/migrationfixture/basemodule"
	"github.com/vislake/speed/go/dbkit/internal/migrationfixture/derivedmodule"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// TestDBComponents_WellFormed runs both database descriptors through the
// component contract: the naming convention, the decodable schema, the
// typed tokens.
func TestDBComponents_WellFormed(t *testing.T) {
	t.Parallel()
	componenttest.AssertWellFormed(t, dbSQLiteComponent)
	componenttest.AssertWellFormed(t, dbPostgresComponent)
}

// TestDBComponents_DeclareCapabilities pins the two declarations: SQLite is
// the standalone dialect (0 -- a distinct file is not shared state a second
// replica may use, and a distributed-mode assembly selecting it must fail
// the capability check), PostgreSQL is the distributed one
// (MultiReplicaSafe | SurvivesRestart).
func TestDBComponents_DeclareCapabilities(t *testing.T) {
	t.Parallel()
	if dbSQLiteComponent.Capabilities != 0 {
		t.Errorf("db.sqlite capabilities = %v, want 0: it is the standalone dialect", dbSQLiteComponent.Capabilities)
	}
	if want := pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart; dbPostgresComponent.Capabilities != want {
		t.Errorf("db.postgres capabilities = %v, want %v", dbPostgresComponent.Capabilities, want)
	}
}

// fixtureBaseProduct and fixtureDerivedProduct are the products the two
// fixture components below provide, so the derived one's Requires gives the
// dependency edge that orders it after the base one.
type (
	fixtureBaseProduct    struct{}
	fixtureDerivedProduct struct{}
)

// fixtureComponents are two host components carrying the migration-fixture
// sets this package's own registry tests use, wired through Requires so the
// derived set must be applied after the base one (its DDL builds on the base
// table).
func fixtureComponents() []pkgcore.Component {
	return []pkgcore.Component{
		{
			Name:       "fixture.base",
			Provides:   []any{(*fixtureBaseProduct)(nil)},
			Migrations: basemodule.Migrations,
			New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
				return &fixtureBaseProduct{}, nil
			},
		},
		{
			Name:       "fixture.derived",
			Provides:   []any{(*fixtureDerivedProduct)(nil)},
			Requires:   []pkgcore.Requirement{{Token: (*fixtureBaseProduct)(nil)}},
			Migrations: derivedmodule.Migrations,
			New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
				return &fixtureDerivedProduct{}, nil
			},
		},
	}
}

// TestDBComponent_ConnectsAppliesMigrationsAndCloses drives the whole db
// component lifecycle in one assembly: New connects, Verify applies every
// selected component's migration set through ApplyMigrations
// (dependency-ordered, ledged by component name), a second ApplyMigrations
// is a no-op, and Close releases the connection.
func TestDBComponent_ConnectsAppliesMigrationsAndCloses(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "db-component-test.db")

	reg := pkgcore.NewComponentRegistry()
	for _, c := range fixtureComponents() {
		if err := reg.Register(c); err != nil {
			t.Fatalf("Register(%q) error = %v", c.Name, err)
		}
	}
	// Selecting derived before base on purpose: the Requires edge must
	// reorder them, so the base set lands first whatever the composition's
	// own order.
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{
			"db.sqlite":       map[string]any{"dsn": dsn},
			"fixture.derived": nil,
			"fixture.base":    nil,
		},
	}))

	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := reg.Construct(ctx); err != nil {
		t.Fatalf("Construct() error = %v", err)
	}
	if err := reg.Verify(ctx); err != nil {
		t.Fatalf("Verify() error = %v, want the selected migration sets applied", err)
	}

	db, err := pkgcore.Get[*gorm.DB](reg)
	if err != nil {
		t.Fatalf("Get[*gorm.DB] error = %v", err)
	}

	// Both fixture sets ran, in dependency order: the derived migration
	// seeds a row into the base table, which only works when the base set
	// was applied first.
	var seeded int64
	if err := db.Table("base_items").Where("id = ?", "seed-from-derived").Count(&seeded).Error; err != nil {
		t.Fatalf("count seeded row: %v", err)
	}
	if seeded != 1 {
		t.Errorf("derived seeding row count = %d, want 1: the derived set must run after the base set", seeded)
	}

	// The ledger records both components under their own names.
	var ledgerRows int64
	if err := db.Table(schemaMigrationsTable).Where("module IN ?", []string{"fixture.base", "fixture.derived"}).Count(&ledgerRows).Error; err != nil {
		t.Fatalf("count ledger rows: %v", err)
	}
	if ledgerRows == 0 {
		t.Error("schema_migrations carries no rows for the fixture components, want the applied sets recorded")
	}

	// A repeated application is a no-op: nothing left to apply.
	if err := ApplyMigrations(ctx, reg); err != nil {
		t.Fatalf("second ApplyMigrations() error = %v, want the ledger to make it a no-op", err)
	}

	if err := reg.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v, want the connection released", err)
	}
	if err := db.Exec("SELECT 1").Error; err == nil {
		t.Error("query after Close() succeeded, want the connection closed")
	}
}

// TestApplyMigrations_RequiresAConnection pins the guard: with no *gorm.DB
// put, ApplyMigrations reports the missing requirement instead of panicking.
func TestApplyMigrations_RequiresAConnection(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	err := ApplyMigrations(context.Background(), reg)
	if err == nil {
		t.Fatal("ApplyMigrations() without a connection succeeded, want the missing-requirement error")
	}
}
