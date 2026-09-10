package dbtest_test

// migrate_test.go is the happy-path suite for dbtest's migration half:
// Migrate and the Migration value it consumes. The two constructors'
// migration-accepting shape is pinned in dbtest_test.go (the designated
// suite for those entry points), reusing the fixture helpers defined here
// -- both files are one test package, so the helpers are written once.

import (
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/dbkit/dbtest/migrationfixture"
)

// fixtureModuleName is the module name these tests record the shared
// fixture set under.
const fixtureModuleName = "dbtest_fixture"

// fixtureTable is the table the fixture set's migration creates.
const fixtureTable = "dbtest_fixture_items"

// assertFixtureTableExists fails the test unless the fixture's table is
// present on db, through each dialect's own catalog view.
func assertFixtureTableExists(t *testing.T, db *gorm.DB, dialect dbkit.Dialect) {
	t.Helper()

	var n int64
	var err error
	if dialect == dbkit.DialectSQLite {
		err = db.Raw(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, fixtureTable,
		).Scan(&n).Error
	} else {
		err = db.Raw(
			`SELECT COUNT(*) FROM information_schema.tables WHERE table_name = ?`, fixtureTable,
		).Scan(&n).Error
	}
	if err != nil {
		t.Fatalf("look up table %q: %v", fixtureTable, err)
	}
	if n != 1 {
		t.Errorf("table %q: found %d matching catalog rows, want 1", fixtureTable, n)
	}
}

// migrationLedgerRows returns how many schema_migrations rows were recorded
// for module.
func migrationLedgerRows(t *testing.T, db *gorm.DB, module string) int64 {
	t.Helper()

	var n int64
	if err := db.Raw(
		`SELECT COUNT(*) FROM schema_migrations WHERE module = ?`, module,
	).Scan(&n).Error; err != nil {
		t.Fatalf("count schema_migrations rows for %q: %v", module, err)
	}
	return n
}

// TestMigrate_AppliesSetFromZero proves Migrate's core contract: a set's
// sqlite/*.sql files run against an empty database, leaving both the table
// and the registry's own ledger behind -- through the real
// dbkit.MigrationRegistry, so this is also the fixture's from-zero proof.
func TestMigrate_AppliesSetFromZero(t *testing.T) {
	db := dbtest.NewSQLite(t)
	dbtest.Migrate(t, db, dbkit.DialectSQLite, dbtest.Migration{Module: fixtureModuleName, FS: migrationfixture.Migrations})

	assertFixtureTableExists(t, db, dbkit.DialectSQLite)
	if got := migrationLedgerRows(t, db, fixtureModuleName); got != 1 {
		t.Errorf("schema_migrations rows for %q = %d, want 1", fixtureModuleName, got)
	}
}

// TestMigrate_MultipleSets_SingleRegistry proves every set a caller passes
// is registered on the one registry and applied through the one Apply: both
// module names end up in the shared ledger. The fixture's IF NOT EXISTS is
// what lets one tree stand in for both sets here; real callers pass
// distinct trees.
func TestMigrate_MultipleSets_SingleRegistry(t *testing.T) {
	db := dbtest.NewSQLite(t)
	dbtest.Migrate(t, db, dbkit.DialectSQLite,
		dbtest.Migration{Module: "dbtest_fixture_a", FS: migrationfixture.Migrations},
		dbtest.Migration{Module: "dbtest_fixture_b", FS: migrationfixture.Migrations},
	)

	assertFixtureTableExists(t, db, dbkit.DialectSQLite)
	for _, module := range []string{"dbtest_fixture_a", "dbtest_fixture_b"} {
		if got := migrationLedgerRows(t, db, module); got != 1 {
			t.Errorf("schema_migrations rows for %q = %d, want 1", module, got)
		}
	}
}

// TestMigrate_AppliedTwice_IsNoOp pins that a repeated call against an
// already-migrated database changes nothing: the ledger's (module, filename)
// record is what a staged-upgrade test relies on when it applies the frozen
// older set and then the full one.
func TestMigrate_AppliedTwice_IsNoOp(t *testing.T) {
	db := dbtest.NewSQLite(t)
	set := dbtest.Migration{Module: fixtureModuleName, FS: migrationfixture.Migrations}
	dbtest.Migrate(t, db, dbkit.DialectSQLite, set)
	dbtest.Migrate(t, db, dbkit.DialectSQLite, set)

	assertFixtureTableExists(t, db, dbkit.DialectSQLite)
	if got := migrationLedgerRows(t, db, fixtureModuleName); got != 1 {
		t.Errorf("schema_migrations rows for %q after a repeated Migrate = %d, want 1 (no file re-recorded)", fixtureModuleName, got)
	}
}

// TestMigrate_Postgres runs the same proof against a real PostgreSQL
// server, exercising the set's postgres/ subdirectory. It reports itself
// skipped rather than failed on a machine with no Docker -- the skip lives
// in dbtest.NewPostgres's own probe.
func TestMigrate_Postgres(t *testing.T) {
	db := dbtest.NewPostgres(t)
	dbtest.Migrate(t, db, dbkit.DialectPostgres, dbtest.Migration{Module: fixtureModuleName, FS: migrationfixture.Migrations})

	assertFixtureTableExists(t, db, dbkit.DialectPostgres)
	if got := migrationLedgerRows(t, db, fixtureModuleName); got != 1 {
		t.Errorf("schema_migrations rows for %q = %d, want 1", fixtureModuleName, got)
	}
}
