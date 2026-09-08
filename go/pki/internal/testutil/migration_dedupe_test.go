package testutil

import (
	"testing"

	"github.com/vislake/speed/go/dbkit"

	"github.com/vislake/speed/go/pki/migrations"
)

// TestMigration0008_DuplicateLedgerRows_UpgradeKeepsOneEarliestRow is the
// SQLite unit leg of the duplicate-ledger regression (a): a database whose
// revocation ledger holds two rows for one certificate -- the duplicate
// shape a check-then-act ledger write could leave behind -- must upgrade
// through the module's migration set and be left with exactly one row per
// certificate. This test fails against the pre-fix 0008 file: its CREATE
// UNIQUE INDEX refuses to build over the seeded duplicates, and the module
// transaction rolls back, stranding the upgrade. The PostgreSQL leg runs
// the same scenario against a real server in
// go/pki/integration_test/postgres_migration_dedupe_test.go.
func TestMigration0008_DuplicateLedgerRows_UpgradeKeepsOneEarliestRow(t *testing.T) {
	db := NewSQLite(t, "pki", migrations.FS)
	AssertMigration0008UpgradeDedupesDuplicateLedger(t, db, dbkit.DialectSQLite, "pki", migrations.FS)
}

// TestMigration0008_DuplicateLedgerRows_ReapplyLeavesAppliedDatabaseUntouched
// is the SQLite unit leg of the duplicate-ledger regression (b): a database
// that already applied 0008 is unaffected by a later migration run -- the
// registry records applied files by (module, filename) and never re-runs a
// recorded file, so a re-apply is a strict no-op over real ledger content.
func TestMigration0008_DuplicateLedgerRows_ReapplyLeavesAppliedDatabaseUntouched(t *testing.T) {
	db := NewSQLite(t, "pki", migrations.FS)
	AssertMigration0008ReapplyLeavesAppliedDatabaseUntouched(t, db, dbkit.DialectSQLite, "pki", migrations.FS)
}
