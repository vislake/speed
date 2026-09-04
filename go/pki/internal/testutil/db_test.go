package testutil

import (
	"testing"

	"github.com/vislake/speed/go/dbkit"

	"github.com/vislake/speed/go/pki/migrations"
)

// TestNewSQLite_AppliesPKIMigrationsFromZero proves the helper does what
// every other test in this module relies on it for: run pki's real,
// versioned migration files from an empty database, leaving all four tables
// and a sample of their indexes in place. AutoMigrate is banned, so a
// broken migration file must fail here rather than silently be papered
// over.
func TestNewSQLite_AppliesPKIMigrationsFromZero(t *testing.T) {
	db := NewSQLite(t, "pki", migrations.FS)

	wantTables := []string{
		"pki_signing_keys",
		"pki_authorities",
		"pki_certificates",
		"pki_local_keys",
	}
	for _, table := range wantTables {
		var names []string
		if err := db.Raw(
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&names).Error; err != nil {
			t.Fatalf("query sqlite_master for table %q: %v", table, err)
		}
		if len(names) != 1 {
			t.Errorf("table %q: got %d matching rows, want 1", table, len(names))
		}
	}

	wantIndexes := []string{
		"uq_pki_signing_keys_active_purpose",
		"idx_pki_signing_keys_not_after",
		"idx_pki_authorities_parent_id",
		"idx_pki_certificates_tenant_authority",
		"idx_pki_local_keys_not_after",
	}
	var gotIndexes []string
	if err := db.Raw(
		`SELECT name FROM sqlite_master WHERE type = 'index'`,
	).Scan(&gotIndexes).Error; err != nil {
		t.Fatalf("query sqlite_master for indexes: %v", err)
	}
	got := make(map[string]bool, len(gotIndexes))
	for _, name := range gotIndexes {
		got[name] = true
	}
	for _, name := range wantIndexes {
		if !got[name] {
			t.Errorf("index %q was not created by the migrations", name)
		}
	}
}

// TestMigrate_AppliedTwice_IsNoOp proves migrations are idempotent: a second
// Apply over an already-migrated database must not attempt the CREATE TABLE
// again. dbkit's bookkeeping table is what makes this true; the test pins
// that pki's files do not defeat it.
func TestMigrate_AppliedTwice_IsNoOp(t *testing.T) {
	db := NewSQLite(t, "pki", migrations.FS)
	Migrate(t, db, dbkit.DialectSQLite, "pki", migrations.FS)
}
