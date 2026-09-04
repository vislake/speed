package testutil

import (
	"testing"

	"github.com/vislake/speed/go/dbkit"

	"github.com/vislake/speed/go/metering/migrations"
)

// TestNewSQLite_AppliesMeteringMigrationsFromZero proves the helper does
// what every other test in this module relies on it for: run metering's
// real, versioned migration files from an empty database, leaving both
// tables and a sample of their indexes in place. AutoMigrate is banned, so
// a broken migration file must fail here rather than silently be papered
// over.
func TestNewSQLite_AppliesMeteringMigrationsFromZero(t *testing.T) {
	db := NewSQLite(t, "metering", migrations.FS)

	wantTables := []string{
		"metering_usage_summaries",
		"metering_outbox_records",
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
		"idx_metering_usage_summaries_period",
		"idx_metering_usage_summaries_feature",
		"uq_metering_outbox_records_tenant_idempotency",
		"idx_metering_outbox_records_status_created",
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

// TestMigrate_AppliedTwice_IsNoOp proves migrations are idempotent: a
// second Apply over an already-migrated database must not attempt the
// CREATE TABLE again. dbkit's bookkeeping table is what makes this true;
// the test pins that metering's files do not defeat it.
func TestMigrate_AppliedTwice_IsNoOp(t *testing.T) {
	db := NewSQLite(t, "metering", migrations.FS)
	Migrate(t, db, dbkit.DialectSQLite, "metering", migrations.FS)
}
