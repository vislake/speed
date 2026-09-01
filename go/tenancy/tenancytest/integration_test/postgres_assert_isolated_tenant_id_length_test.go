//go:build integration

package tenancytest_test

import (
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/tenancy/tenancytest"
	"github.com/vislake/speed/go/tenancy/tenancytest/internal/testutil"
)

// TestAssertIsolated_LongDescriptiveSubtestName_WorksAcrossDialects_Postgres
// is the postgres-dialect leg of
// tenancytest.TestAssertIsolated_LongDescriptiveSubtestName_WorksAcrossDialects
// (assert_isolated_tenant_id_length_test.go, package tenancytest); see that
// test's doc comment for what it proves -- PostgreSQL is the dialect that
// actually enforces VARCHAR(64) and is what surfaced the tenant-id-length
// bug being regression-tested in the first place, so this leg matters more
// than most -- and postgres_assert_isolated_test.go's own doc comment for
// why it lives here instead of in the unit tier.
//
// Reuses the sprocket fixture (type, newSprocket, createSprocketsTableSQL)
// declared in postgres_assert_isolated_test.go: same package, same
// directory, so no re-declaration is needed the way the unit tier's own
// AGENTS.md/doc-comment guidance requires across separate packages.
func TestAssertIsolated_LongDescriptiveSubtestName_WorksAcrossDialects_Postgres(t *testing.T) {
	db := testutil.Dialects()[1].NewDB(t) // postgres: see postgres_assert_isolated_test.go's doc comment
	if err := db.Exec(createSprocketsTableSQL).Error; err != nil {
		t.Fatalf("create sprockets table: %v", err)
	}
	repo := dbkit.NewRepository[sprocket](db)

	t.Run("ADeliberatelyLongAndDescriptiveSubtestNameLikeTheProjectConventionAsksFor", func(t *testing.T) {
		tenancytest.AssertIsolated(t, repo, newSprocket)
	})
}
