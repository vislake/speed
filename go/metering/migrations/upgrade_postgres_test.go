//go:build integration

package migrations

import (
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
)

// TestMigrations_0007_UpgradeFromA0006EraDatabase_Postgres is the
// P3-metering-E upgrade-path proof against a real PostgreSQL server: the
// same staged 0006-era-database-then-upgrade run upgrade_test.go performs
// on SQLite, on the engine whose ALTER COLUMN SET NOT NULL and
// not-null-violation behaviour genuinely differs -- the SQLite-only proof
// cannot rule out a PostgreSQL-specific failure of 0007's backfill-then-
// constraint ordering (an ALTER COLUMN SET NOT NULL that ran before the
// backfill would fail outright on a database holding any NULL row, which
// is exactly why the migration's statement order is load-bearing and
// worth pinning against the real server), nor of the real 23502
// not-null-violation error a NULL write produces there.
func TestMigrations_0007_UpgradeFromA0006EraDatabase_Postgres(t *testing.T) {
	stageRetryAfterUpgrade(t, dbtest.NewPostgres(t), dbkit.DialectPostgres)
}
