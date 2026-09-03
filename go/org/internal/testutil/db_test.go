package testutil

import (
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/org/migrations"
)

// TestNewSQLite_AppliesOrgMigrationsFromZero proves the helper does what
// every other test in this module relies on it for: run org's real,
// versioned migration files from an empty database, leaving the org_nodes
// table and its three indexes in place. AutoMigrate is banned, so a broken
// migration file must fail here rather than silently be papered over.
func TestNewSQLite_AppliesOrgMigrationsFromZero(t *testing.T) {
	db := NewSQLite(t, "org", migrations.FS)

	var tables []string
	if err := db.Raw(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'org_nodes'`,
	).Scan(&tables).Error; err != nil {
		t.Fatalf("query sqlite_master for tables: %v", err)
	}
	if len(tables) != 1 {
		t.Fatalf("org_nodes table: got %d matching rows, want 1", len(tables))
	}

	var indexes []string
	if err := db.Raw(
		`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'org_nodes' AND name LIKE ?`,
		"%org_nodes%",
	).Scan(&indexes).Error; err != nil {
		t.Fatalf("query sqlite_master for indexes: %v", err)
	}
	want := map[string]bool{
		"idx_org_nodes_tenant_path":   false,
		"idx_org_nodes_tenant_parent": false,
		"uq_org_nodes_sibling_name":   false,
	}
	for _, name := range indexes {
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("index %q was not created by the migration", name)
		}
	}
}

// TestMigrate_AppliedTwice_IsNoOp proves migrations are idempotent: a second
// Apply over an already-migrated database must not attempt the CREATE TABLE
// again. dbkit's bookkeeping table is what makes this true; the test pins
// that org's files do not defeat it.
func TestMigrate_AppliedTwice_IsNoOp(t *testing.T) {
	db := NewSQLite(t, "org", migrations.FS)
	Migrate(t, db, dbkit.DialectSQLite, "org", migrations.FS)
}
