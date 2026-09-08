//go:build integration

// Package pki_test holds go/pki's PostgreSQL integration tier: an
// integration_test/ package calling the testcontainers-backed
// testutil.NewPostgres helper (go/pki/internal/testutil/db.go), exercising
// the module's migration set against a real server.
//
// Run as `go test -tags=integration ./integration_test/...` from the
// module directory with a Docker daemon reachable; the tests skip
// themselves when Docker is not available (dbtest.NewPostgres's own
// probe).
package pki_test

import (
	"testing"

	"github.com/vislake/speed/go/dbkit"

	"github.com/vislake/speed/go/pki/internal/testutil"
	"github.com/vislake/speed/go/pki/migrations"
)

// TestMigration0008_DuplicateLedgerRows_RealPostgres is the PostgreSQL leg
// of the duplicate-ledger upgrade, run against a real server started with
// testcontainers: a database whose revocation ledger holds two rows for one
// certificate must upgrade through the migration set, deduping to the
// earliest row per certificate -- and the already-applied database is
// unaffected by a further re-run. On this dialect the dedupe matters just
// as much: without it the CREATE UNIQUE INDEX refuses to build over the
// seeded duplicates and the whole migration rolls back, exactly as on
// SQLite.
func TestMigration0008_DuplicateLedgerRows_RealPostgres(t *testing.T) {
	// NewPostgres migrates moduleName's postgres/*.sql files from zero on
	// a real server, the same precondition the SQLite unit leg starts
	// from.
	db := testutil.NewPostgres(t, "pki", migrations.FS)

	// The upgrade leg runs first, then the re-apply leg on the same
	// database: after the upgrade the database is fully migrated with one
	// row per seeded certificate, which is exactly the state the re-apply
	// leg asserts a further run leaves untouched.
	testutil.AssertMigration0008UpgradeDedupesDuplicateLedger(t, db, dbkit.DialectPostgres, "pki", migrations.FS)
	testutil.AssertMigration0008ReapplyLeavesAppliedDatabaseUntouched(t, db, dbkit.DialectPostgres, "pki", migrations.FS)
}
