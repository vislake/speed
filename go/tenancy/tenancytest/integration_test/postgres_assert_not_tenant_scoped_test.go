//go:build integration

package tenancytest_test

import (
	"fmt"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/tenancy/tenancytest"
	"github.com/vislake/speed/go/tenancy/tenancytest/internal/testutil"
)

// platformSetting mirrors the unexported fixture of the same name in
// assert_not_tenant_scoped_test.go (package tenancytest, one directory up)
// -- see postgres_assert_isolated_test.go's doc comment on sprocket for why
// this package defines its own copy rather than sharing one.
type platformSetting struct {
	Key   string `gorm:"column:key;primaryKey;size:64"`
	Value string `gorm:"column:value;size:255"`
}

const createPlatformSettingsTableSQL = `CREATE TABLE platform_settings (
	key VARCHAR(64) NOT NULL PRIMARY KEY,
	value VARCHAR(255) NOT NULL DEFAULT ''
)`

var platformSettingKeySeq atomic.Uint64

// TestAssertNotTenantScoped_PlatformSetting_Postgres is the postgres-dialect
// leg of tenancytest.TestAssertNotTenantScoped_PlatformSetting
// (assert_not_tenant_scoped_test.go, package tenancytest); see that test's
// doc comment for what it proves and postgres_assert_isolated_test.go's own
// doc comment for why the postgres leg lives here instead of there.
func TestAssertNotTenantScoped_PlatformSetting_Postgres(t *testing.T) {
	db := testutil.Dialects()[1].NewDB(t) // postgres: see postgres_assert_isolated_test.go's doc comment
	if err := db.Exec(createPlatformSettingsTableSQL).Error; err != nil {
		t.Fatalf("create platform_settings table: %v", err)
	}

	createFn := func(db *gorm.DB) error {
		key := fmt.Sprintf("setting-%d", platformSettingKeySeq.Add(1))
		return db.Create(&platformSetting{Key: key, Value: "x"}).Error
	}
	findFn := func(db *gorm.DB) (int64, error) {
		var n int64
		err := db.Model(&platformSetting{}).Count(&n).Error
		return n, err
	}

	tenancytest.AssertNotTenantScoped(t, db, platformSetting{}, createFn, findFn)
}
