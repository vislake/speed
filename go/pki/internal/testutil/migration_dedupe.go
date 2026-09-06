package testutil

import (
	"embed"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// The migration-0008 duplicate-ledger-row upgrade regression, shared by the
// SQLite unit leg (migration_dedupe_test.go) and the PostgreSQL integration
// tier (go/pki/integration_test/postgres_migration_dedupe_test.go), which
// run byte-identical scenarios against the two dialects.
//
// Migration 0008 (0008_enforce_one_ledger_row_per_certificate.sql) turns
// 0007's non-unique per-certificate index on pki_certificate_revocations
// into the uq_pki_certificate_revocations_certificate unique index the
// current RevokeCertificate arbitration (InsertIfAbsent's ON CONFLICT)
// builds on. Deployments that ran round 3's pre-arbitration
// RevokeCertificate -- the check-then-act shape (find the certificate,
// then Create the ledger row) shipped before this file existed -- against
// 0007's non-unique schema could land TWO ledger rows for one certificate
// when two callers revoked it concurrently. On upgrade, the CREATE UNIQUE
// INDEX fails on such data, and because dbkit's MigrationRegistry applies
// one module's files in a single transaction (go/dbkit/migrations.go's
// applyModule), the failure rolls 0008 and every later file back together,
// stranding the deployment at startup with 0008 unrecorded and no
// remediation path. The shipped 0008 files therefore dedupe before
// creating the index (keeping the earliest row per certificate_id), and
// these helpers prove that upgrade path against both real engines.
const (
	// migration0008Filename is 0008's file name, identical across the two
	// dialect copies, as schema_migrations records it.
	migration0008Filename = "0008_enforce_one_ledger_row_per_certificate.sql"
	// migration0009Filename is the file that shares 0008's transaction:
	// a deployment stranded at 0008 has 0009 unrecorded too, and the
	// remediated boot must re-apply both.
	migration0009Filename = "0009_widen_key_ref_columns.sql"

	// uqLedgerIndex is the unique index 0008 creates.
	uqLedgerIndex = "uq_pki_certificate_revocations_certificate"
	// plainLedgerIndex is 0007's non-unique per-certificate index, which
	// 0008's file drops after the unique index supersedes it.
	plainLedgerIndex = "idx_pki_certificate_revocations_certificate_id"
)

// AssertMigration0008UpgradeDedupesDuplicateLedger proves regression (a) of
// P0-pki-6: a database carrying two ledger rows for one certificate -- the
// duplicate shape round 3's pre-arbitration RevokeCertificate could leave
// behind -- upgrades through the module's migration set successfully and is
// left with exactly one row per certificate: the EARLIEST one (by
// created_at, id as the tiebreak), which is the row the arbitration the
// unique index provides would itself have let win.
//
// db must be a fresh database that has already had moduleName's migration
// files applied from zero (testutil.NewSQLite / testutil.NewPostgres do
// this). The helper then rewinds the ledger to the exact state a
// deployment stranded at 0008 is left in -- the module transaction that
// failed rolled 0008 and 0009 back together, so the unique index is
// absent, 0007's plain per-certificate index (which 0008's own file drops)
// is present again, and neither file is recorded in schema_migrations --
// seeds the duplicates, re-applies the migration set through the real
// dbkit.MigrationRegistry, and asserts the remediated end state.
//
// Against the pre-fix 0008 file this fails at the re-apply: the CREATE
// UNIQUE INDEX refuses to build over the seeded duplicates.
func AssertMigration0008UpgradeDedupesDuplicateLedger(t *testing.T, db *gorm.DB, dialect dbkit.Dialect, moduleName string, fs embed.FS) {
	t.Helper()

	// 1. Rewind to the stranded deployment's ledger state.
	exec(t, db, "DROP INDEX "+uqLedgerIndex)
	exec(t, db, "CREATE INDEX "+plainLedgerIndex+" ON pki_certificate_revocations (certificate_id)")
	exec(t, db,
		"DELETE FROM schema_migrations WHERE module = ? AND filename IN (?, ?)",
		moduleName, migration0008Filename, migration0009Filename)

	// 2. Seed the duplicate shape. For certificate-dup-a the LATER-created
	// row deliberately carries the lexicographically SMALLER id, so a
	// dedupe that kept MIN(id) instead of the earliest created_at would
	// fail the survivor assertion below rather than pass it.
	seedLedgerRows(t, db,
		ledgerSeedRow{id: "aaaaaaaa-0000-0000-0000-000000000001", certificateID: "certificate-dup-a", createdAt: "2026-09-05 10:00:00"},
		ledgerSeedRow{id: "bbbbbbbb-0000-0000-0000-000000000002", certificateID: "certificate-dup-a", createdAt: "2026-09-05 09:00:00"},
		ledgerSeedRow{id: "cccccccc-0000-0000-0000-000000000003", certificateID: "certificate-dup-b", createdAt: "2026-09-05 09:00:00"},
		ledgerSeedRow{id: "dddddddd-0000-0000-0000-000000000004", certificateID: "certificate-dup-b", createdAt: "2026-09-05 10:00:00"},
		ledgerSeedRow{id: "eeeeeeee-0000-0000-0000-000000000005", certificateID: "certificate-clean", createdAt: "2026-09-05 09:00:00"},
	)

	// 3. The remediated boot: re-apply the module's migration set through
	// the real registry. This is the assertion the pre-fix file fails.
	Migrate(t, db, dialect, moduleName, fs)

	// 4. The remediated end state: one row per certificate, the earliest
	// row the survivor, the unique index in place, 0007's superseded index
	// gone, and both files recorded again.
	for _, certID := range []string{"certificate-dup-a", "certificate-dup-b", "certificate-clean"} {
		if n := ledgerRowCountForCertificate(t, db, certID); n != 1 {
			t.Errorf("certificate %q has %d ledger rows after the upgrade, want exactly 1", certID, n)
		}
	}
	for _, want := range []struct{ certID, wantID string }{
		{"certificate-dup-a", "bbbbbbbb-0000-0000-0000-000000000002"},
		{"certificate-dup-b", "cccccccc-0000-0000-0000-000000000003"},
		{"certificate-clean", "eeeeeeee-0000-0000-0000-000000000005"},
	} {
		if got := ledgerRowIDForCertificate(t, db, want.certID); got != want.wantID {
			t.Errorf("surviving ledger row for %q = %q, want the earliest-created row %q", want.certID, got, want.wantID)
		}
	}
	if !ledgerIndexExists(t, db, dialect, uqLedgerIndex) {
		t.Errorf("unique index %q missing after the upgrade", uqLedgerIndex)
	}
	if ledgerIndexExists(t, db, dialect, plainLedgerIndex) {
		t.Errorf("0007 index %q still present after the upgrade, want it dropped by 0008", plainLedgerIndex)
	}
	if n := appliedMigrationCount(t, db, moduleName, migration0008Filename, migration0009Filename); n != 2 {
		t.Errorf("schema_migrations records %d of the re-applied files, want both 0008 and 0009 recorded", n)
	}
}

// AssertMigration0008ReapplyLeavesAppliedDatabaseUntouched proves regression
// (b) of P0-pki-6: a database that already applied 0008 -- ledger rows
// included -- is unaffected by a later migration run. dbkit's registry
// records applied files by (module, filename) and never re-executes or
// re-compares a recorded file's content, so 0008's in-place dedupe edit
// reaches only databases that never recorded the file (the stranded class
// AssertMigration0008UpgradeDedupesDuplicateLedger exercises) and is a
// strict no-op here: re-running the set must not attempt 0008's DDL again
// (its index already exists), must not touch the ledger's rows, and must
// not duplicate any schema_migrations record.
//
// db must be a fresh, fully migrated database (testutil.NewSQLite does
// this). A ledger row is seeded before the re-run so row preservation is
// asserted against real content, not an empty table.
func AssertMigration0008ReapplyLeavesAppliedDatabaseUntouched(t *testing.T, db *gorm.DB, dialect dbkit.Dialect, moduleName string, fs embed.FS) {
	t.Helper()

	seedLedgerRows(t, db,
		ledgerSeedRow{id: "ffffffff-0000-0000-0000-000000000006", certificateID: "certificate-fresh", createdAt: "2026-09-05 09:00:00"},
	)

	beforeRows := ledgerRowCountForCertificate(t, db, "certificate-fresh")
	beforeRecords := appliedMigrationCount(t, db, moduleName)

	Migrate(t, db, dialect, moduleName, fs)

	if got := ledgerRowCountForCertificate(t, db, "certificate-fresh"); got != beforeRows {
		t.Errorf("ledger rows for the seeded certificate = %d after the re-run, want the pre-run %d untouched", got, beforeRows)
	}
	if got := appliedMigrationCount(t, db, moduleName); got != beforeRecords {
		t.Errorf("schema_migrations holds %d records after the re-run, want the pre-run %d (no file re-recorded)", got, beforeRecords)
	}
	if !ledgerIndexExists(t, db, dialect, uqLedgerIndex) {
		t.Errorf("unique index %q missing after the no-op re-run", uqLedgerIndex)
	}
}

// ledgerSeedRow is one revocation-ledger row the scenarios seed.
type ledgerSeedRow struct {
	id            string
	certificateID string
	createdAt     string
}

// seedLedgerRows inserts the given rows with the remaining columns fixed to
// the same values a real CertificateRevocation row carries (see
// go/pki/model.go). The created_at values are fixed literals in one format,
// so the ORDER BY created_at in 0008's dedupe compares them identically on
// both dialects.
func seedLedgerRows(t *testing.T, db *gorm.DB, rows ...ledgerSeedRow) {
	t.Helper()
	values := make([]string, len(rows))
	args := make([]any, 0, len(rows)*8)
	for i, r := range rows {
		values[i] = "(?, ?, ?, ?, ?, ?, ?, ?)"
		args = append(args,
			r.id, r.certificateID, "authority-1", "ab12cd34ef", "tenant-acme",
			"2026-09-05 09:30:00", "key_compromise", r.createdAt)
	}
	exec(t, db,
		"INSERT INTO pki_certificate_revocations "+
			"(id, certificate_id, authority_id, serial, tenant_id, revoked_at, revocation_reason, created_at) "+
			"VALUES "+strings.Join(values, ", "),
		args...)
}

// exec runs one statement, failing the test on any error.
func exec(t *testing.T, db *gorm.DB, sql string, args ...any) {
	t.Helper()
	if err := db.Exec(sql, args...).Error; err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// ledgerRowCountForCertificate counts the ledger rows of one certificate.
func ledgerRowCountForCertificate(t *testing.T, db *gorm.DB, certificateID string) int64 {
	t.Helper()
	var n int64
	if err := db.Raw(
		"SELECT COUNT(*) FROM pki_certificate_revocations WHERE certificate_id = ?", certificateID,
	).Scan(&n).Error; err != nil {
		t.Fatalf("count ledger rows for %q: %v", certificateID, err)
	}
	return n
}

// ledgerRowIDForCertificate returns the single ledger row id of one
// certificate, failing the test when the row is missing or duplicated.
func ledgerRowIDForCertificate(t *testing.T, db *gorm.DB, certificateID string) string {
	t.Helper()
	var ids []string
	if err := db.Raw(
		"SELECT id FROM pki_certificate_revocations WHERE certificate_id = ?", certificateID,
	).Scan(&ids).Error; err != nil {
		t.Fatalf("read ledger rows for %q: %v", certificateID, err)
	}
	if len(ids) != 1 {
		t.Fatalf("certificate %q has %d ledger rows, want exactly 1", certificateID, len(ids))
	}
	return ids[0]
}

// ledgerIndexExists reports whether the named index exists, through each
// dialect's own catalog view.
func ledgerIndexExists(t *testing.T, db *gorm.DB, dialect dbkit.Dialect, name string) bool {
	t.Helper()
	var names []string
	var err error
	if dialect == dbkit.DialectSQLite {
		err = db.Raw(
			"SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?", name,
		).Scan(&names).Error
	} else {
		err = db.Raw(
			"SELECT indexname FROM pg_indexes WHERE schemaname = current_schema() AND indexname = ?", name,
		).Scan(&names).Error
	}
	if err != nil {
		t.Fatalf("look up index %q: %v", name, err)
	}
	return len(names) == 1
}

// appliedMigrationCount counts schema_migrations records for moduleName,
// for exactly the given filenames when any are named, or for every file of
// the module otherwise.
func appliedMigrationCount(t *testing.T, db *gorm.DB, moduleName string, filenames ...string) int64 {
	t.Helper()
	var n int64
	var err error
	if len(filenames) == 0 {
		err = db.Raw(
			"SELECT COUNT(*) FROM schema_migrations WHERE module = ?", moduleName,
		).Scan(&n).Error
	} else {
		placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(filenames)), ", ")
		args := make([]any, 0, len(filenames)+1)
		args = append(args, moduleName)
		for _, f := range filenames {
			args = append(args, f)
		}
		err = db.Raw(
			"SELECT COUNT(*) FROM schema_migrations WHERE module = ? AND filename IN ("+placeholders+")",
			args...,
		).Scan(&n).Error
	}
	if err != nil {
		t.Fatalf("count schema_migrations records: %v", err)
	}
	return n
}
