package dbtest

import (
	"embed"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// Migration declares one module's embedded migration set for Migrate: the
// module name dbkit.MigrationRegistry records the set's files under, and
// the embed.FS carrying them in the "<dialect>/*.sql" layout every
// pkgcore.Module's Migrations() is expected to have (see
// MigrationRegistry's own doc comment).
//
// It is deliberately a small data type rather than a pkgcore.Module: the
// registry consumes modules, but a test that only needs a schema rarely has
// the real module to hand -- constructing one usually needs seams and
// services the test is not exercising, and inside the module's own test
// files naming it would import-cycle the test binary. Migration carries
// exactly the two facts the registry reads for a module with no
// dependencies, which is all a test database needs: a name and a file tree.
type Migration struct {
	// Module is the name the set's files are recorded under in
	// schema_migrations, exactly as a registered pkgcore.Module's Name()
	// would be.
	Module string
	// FS is the module's migration tree, holding <dialect>/*.sql files
	// (for example "sqlite/0001_create_widgets.sql"), one subdirectory
	// per dialect.
	FS embed.FS
}

// Migrate applies every migration set in migrations to db, for dialect,
// from zero, through a real dbkit.MigrationRegistry -- the same mechanism a
// host's Boot applies, so a database this helper prepares is
// schema-identical to the one a started process would have prepared, and a
// test using it doubles as proof that the module's migration files run from
// zero.
//
// All sets are registered on one registry, so they share one Apply: the
// sets are applied in the order given (Migration declares no DependsOn, so
// the registry's dependency sort sees every set as independent and keeps
// the given order stable -- a test whose sets depend on one another's
// tables states that by ordering its arguments). Apply's ledger makes a
// repeated call against the same database a no-op for files already
// recorded, so staging an upgrade is two Migrate calls: the frozen older
// set first, then the full set.
//
// db is normally the fresh database one of this package's constructors
// returned; nothing about the call requires that, so a test may equally
// apply a set to a connection it opened itself.
func Migrate(t *testing.T, db *gorm.DB, dialect dbkit.Dialect, migrations ...Migration) {
	t.Helper()

	registry := dbkit.NewMigrationRegistry()
	for _, m := range migrations {
		if err := registry.Register(migrationModule(m)); err != nil {
			t.Fatalf("dbtest: register migration set for module %q: %v", m.Module, err)
		}
	}
	if err := registry.Apply(t.Context(), db, dialect); err != nil {
		t.Fatalf("dbtest: apply %s migrations: %v", dialect, err)
	}
}

// migrationModule adapts one Migration to the migratable shape
// MigrationRegistry consumes. Only Name and Migrations carry information:
// DependsOn is nil per Migration's own doc comment.
type migrationModule Migration

func (m migrationModule) Name() string         { return m.Module }
func (migrationModule) DependsOn() []string    { return nil }
func (m migrationModule) Migrations() embed.FS { return m.FS }
