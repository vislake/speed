//go:build integration

package migrations

import (
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
)

// TestMigrations_0008_UpgradeFromA0007EraDatabase_Postgres is the P1-org-11
// upgrade-path proof against a real PostgreSQL server: the same staged
// 0007-era-database-then-upgrade run upgrade_test.go performs on SQLite, on
// the engine whose partial-index and unique-violation behaviour genuinely
// differs -- the SQLite-only proof cannot rule out a PostgreSQL-specific
// failure of 0008's DROP INDEX / CREATE INDEX pair or of its extra
// predicate (the same reasoning the soft-delete round applied to 0004's
// narrowed indexes, postgres_softdelete_test.go).
func TestMigrations_0008_UpgradeFromA0007EraDatabase_Postgres(t *testing.T) {
	stageSingleRootUpgrade(t, dbtest.NewPostgres(t), dbkit.DialectPostgres)
}
