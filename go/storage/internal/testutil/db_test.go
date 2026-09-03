package testutil

import (
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/storage/migrations"
)

// TestNewSQLite_AppliesStorageMigrationsFromZero proves the helper does
// what every other test in this module relies on it for: run storage's
// real, versioned migration files from an empty database, leaving both
// tables and their supporting indexes in place. AutoMigrate is banned, so
// a broken migration file must fail here rather than silently be papered
// over.
func TestNewSQLite_AppliesStorageMigrationsFromZero(t *testing.T) {
	db := NewSQLite(t, "storage", migrations.FS)

	var tables []string
	if err := db.Raw(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name IN ('objects', 'object_derivatives')`,
	).Scan(&tables).Error; err != nil {
		t.Fatalf("query sqlite_master for tables: %v", err)
	}
	if len(tables) != 2 {
		t.Fatalf("tables: got %d matching rows (%v), want objects and object_derivatives", len(tables), tables)
	}

	// 0001's cursor-listing index lives on the table the listing scans
	// (objects); 0002's unique index guards duplicate derivative kinds per
	// object. Both must exist for the behaviors they support to work.
	var indexes []string
	if err := db.Raw(
		`SELECT name FROM sqlite_master WHERE type = 'index' AND name IN ('idx_objects_tenant_created', 'uq_object_derivatives_object_kind')`,
	).Scan(&indexes).Error; err != nil {
		t.Fatalf("query sqlite_master for indexes: %v", err)
	}
	want := map[string]bool{
		"idx_objects_tenant_created":        false,
		"uq_object_derivatives_object_kind": false,
	}
	for _, name := range indexes {
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("index %q was not created by the migrations", name)
		}
	}
}

// TestMigrate_AppliedTwice_IsNoOp proves migrations are idempotent: a second
// Apply over an already-migrated database must not attempt the CREATE TABLE
// again. dbkit's bookkeeping table is what makes this true; the test pins
// that storage's files do not defeat it.
func TestMigrate_AppliedTwice_IsNoOp(t *testing.T) {
	db := NewSQLite(t, "storage", migrations.FS)
	Migrate(t, db, dbkit.DialectSQLite, "storage", migrations.FS)
}
