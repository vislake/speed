package testutil

import (
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/notification/migrations"
)

// TestNewSQLite_AppliesNotificationMigrationsFromZero proves the helper does
// what every other test in this module relies on it for: run notification's
// real, versioned migration files from an empty database, leaving the
// in_app_messages table and its two indexes in place. AutoMigrate is banned,
// so a broken migration file must fail here rather than silently be papered
// over.
func TestNewSQLite_AppliesNotificationMigrationsFromZero(t *testing.T) {
	db := NewSQLite(t, "notification", migrations.FS)

	var tables []string
	if err := db.Raw(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'in_app_messages'`,
	).Scan(&tables).Error; err != nil {
		t.Fatalf("query sqlite_master for tables: %v", err)
	}
	if len(tables) != 1 {
		t.Fatalf("in_app_messages table: got %d matching rows, want 1", len(tables))
	}

	var indexes []string
	if err := db.Raw(
		`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'in_app_messages' AND name LIKE ?`,
		"%in_app_messages%",
	).Scan(&indexes).Error; err != nil {
		t.Fatalf("query sqlite_master for indexes: %v", err)
	}
	want := map[string]bool{
		"uq_in_app_messages_dedupe_key":                false,
		"idx_in_app_messages_tenant_recipient_created": false,
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
// that notification's files do not defeat it.
func TestMigrate_AppliedTwice_IsNoOp(t *testing.T) {
	db := NewSQLite(t, "notification", migrations.FS)
	Migrate(t, db, dbkit.DialectSQLite, "notification", migrations.FS)
}
