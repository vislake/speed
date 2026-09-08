package testutil

import (
	"testing"

	"github.com/vislake/speed/go/dbkit"

	"github.com/vislake/speed/go/pki/migrations"
)

// TestMigration0008_DuplicateLedgerRows_UpgradeKeepsOneEarliestRow is the
// SQLite unit leg of the duplicate-ledger upgrade: a database whose
// revocation ledger holds two rows for one certificate -- the duplicate
// shape a check-then-act ledger write can leave behind -- must upgrade
// through the module's migration set and be left with exactly one row per
// certificate. A plain CREATE UNIQUE INDEX would refuse to build over the
// seeded duplicates and roll the module's transaction back, stranding the
// upgrade; the migration collapses the duplicates first. The PostgreSQL
// leg runs the same scenario against a real server in
// go/pki/integration_test/postgres_migration_dedupe_test.go.
func TestMigration0008_DuplicateLedgerRows_UpgradeKeepsOneEarliestRow(t *testing.T) {
	db := NewSQLite(t, "pki", migrations.FS)
	AssertMigration0008UpgradeDedupesDuplicateLedger(t, db, dbkit.DialectSQLite, "pki", migrations.FS)
}

// TestMigration0008_DuplicateLedgerRows_ReapplyLeavesAppliedDatabaseUntouched
// is the SQLite unit leg of the re-apply guarantee: a database that already
// applied 0008 is unaffected by a later migration run -- the registry
// records applied files by (module, filename) and never re-runs a recorded
// file, so a re-apply is a strict no-op over real ledger content.
func TestMigration0008_DuplicateLedgerRows_ReapplyLeavesAppliedDatabaseUntouched(t *testing.T) {
	db := NewSQLite(t, "pki", migrations.FS)
	AssertMigration0008ReapplyLeavesAppliedDatabaseUntouched(t, db, dbkit.DialectSQLite, "pki", migrations.FS)
}
