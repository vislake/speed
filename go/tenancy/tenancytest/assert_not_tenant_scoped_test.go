package tenancytest

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest/internal/testutil"
)

// platformSetting is a minimal fixture standing in for platform-domain data
// (docs/internal/04-data-and-tenancy.md's data-domain table): global,
// shared, read/written the same way no matter who is asking, and — the
// property under test — carrying no GetTenantID method at all. This is
// exactly the shape AssertNotTenantScoped exists to certify.
type platformSetting struct {
	Key   string `gorm:"column:key;primaryKey;size:64"`
	Value string `gorm:"column:value;size:255"`
}

const createPlatformSettingsTableSQL = `CREATE TABLE platform_settings (
	key VARCHAR(64) NOT NULL PRIMARY KEY,
	value VARCHAR(255) NOT NULL DEFAULT ''
)`

var platformSettingKeySeq atomic.Uint64

// TestAssertNotTenantScoped_PlatformSetting proves AssertNotTenantScoped
// works end to end against a small non-tenant-scoped fixture of this
// package's own.
//
// SQLite only -- see TestAssertIsolated_Sprocket's doc comment
// (assert_isolated_test.go) for why a plain _test.go file must not reach
// testutil.Dialects()'s postgres entry. The postgres leg of this same
// scenario runs from
// tenancytest/integration_test/postgres_assert_not_tenant_scoped_test.go,
// behind //go:build integration.
func TestAssertNotTenantScoped_PlatformSetting(t *testing.T) {
	db := testutil.Dialects()[0].NewDB(t) // sqlite: see doc comment above
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

	AssertNotTenantScoped(t, db, platformSetting{}, createFn, findFn)
}

// accidentallyScopedSetting is structurally identical to platformSetting
// except it (accidentally, per its name) also implements
// dbkit.TenantScoped. It exists solely for
// TestAssertNotTenantScoped_DetectsAccidentallyImplementedTenantScoped.
type accidentallyScopedSetting struct {
	Key      string `gorm:"column:key;primaryKey;size:64"`
	TenantID string `gorm:"column:tenant_id;size:64"`
}

// GetTenantID makes accidentallyScopedSetting satisfy dbkit.TenantScoped —
// the exact mistake AssertNotTenantScoped's first check exists to catch.
func (a accidentallyScopedSetting) GetTenantID() pkgcore.TenantID {
	return pkgcore.TenantID(a.TenantID)
}

var _ dbkit.TenantScoped = accidentallyScopedSetting{}

// runAccidentallyScopedHelperEnv is the environment variable
// TestAssertNotTenantScoped_DetectsAccidentallyImplementedTenantScoped sets,
// and its _Helper counterpart checks — see
// TestAssertIsolated_DetectsBrokenGetTenantID's doc comment for why a
// subprocess, rather than a plain t.Run, is required to observe a
// deliberately-failing scenario in this package's own tests.
const runAccidentallyScopedHelperEnv = "TENANCYTEST_RUN_ACCIDENTALLY_SCOPED_HELPER"

// TestAssertNotTenantScoped_DetectsAccidentallyImplementedTenantScoped_Helper
// is not meant to run as part of the ordinary suite — it skips itself
// unless runAccidentallyScopedHelperEnv is set — and exists solely to be
// invoked as a subprocess by
// TestAssertNotTenantScoped_DetectsAccidentallyImplementedTenantScoped.
//
// db, createFn and findFn are all nil: AssertNotTenantScoped's own doc
// comment promises the dbkit.TenantScoped check runs, and fails the test,
// before any of the three is ever touched, so passing nils isolates exactly
// that promise — if the check ran *after* trying to use db, this would
// panic on a nil pointer instead of failing the way the test expects, which
// is itself a useful signal were this ever to regress.
func TestAssertNotTenantScoped_DetectsAccidentallyImplementedTenantScoped_Helper(t *testing.T) {
	if os.Getenv(runAccidentallyScopedHelperEnv) != "1" {
		t.Skip("only meant to run as TestAssertNotTenantScoped_DetectsAccidentallyImplementedTenantScoped's subprocess")
	}
	AssertNotTenantScoped(t, nil, accidentallyScopedSetting{}, nil, nil)
}

// TestAssertNotTenantScoped_DetectsAccidentallyImplementedTenantScoped is
// AssertNotTenantScoped's first negative test: proof that it actually
// rejects a model which implements dbkit.TenantScoped, rather than only
// ever passing. See TestAssertIsolated_DetectsBrokenGetTenantID's doc
// comment for why this runs its failing scenario as a subprocess
// (testutil.ExpectFailingSubprocess) instead of a plain t.Run.
func TestAssertNotTenantScoped_DetectsAccidentallyImplementedTenantScoped(t *testing.T) {
	output, failed := testutil.ExpectFailingSubprocess(t,
		"TestAssertNotTenantScoped_DetectsAccidentallyImplementedTenantScoped_Helper",
		runAccidentallyScopedHelperEnv)
	if !failed {
		t.Fatalf("AssertNotTenantScoped passed against a model that implements dbkit.TenantScoped; want it to fail immediately. Helper subprocess output:\n%s", output)
	}
	if !strings.Contains(output, "implements dbkit.TenantScoped") {
		t.Errorf("helper subprocess failed, as wanted, but apparently not because the model implements dbkit.TenantScoped; output:\n%s", output)
	}
}

// filteredSetting carries a tenant_id column but, unlike
// accidentallyScopedSetting, implements no GetTenantID method at all — so
// it passes AssertNotTenantScoped's static dbkit.TenantScoped check. It
// exists solely for
// TestAssertNotTenantScoped_DetectsHandRolledTenantFiltering, which pairs
// it with a findFn that hand-filters by tenant anyway.
type filteredSetting struct {
	Key      string `gorm:"column:key;primaryKey;size:64"`
	TenantID string `gorm:"column:tenant_id;size:64"`
}

const createFilteredSettingsTableSQL = `CREATE TABLE filtered_settings (
	key VARCHAR(64) NOT NULL PRIMARY KEY,
	tenant_id VARCHAR(64) NOT NULL DEFAULT ''
)`

var filteredSettingKeySeq atomic.Uint64

// runHandRolledFilteringHelperEnv is the environment variable
// TestAssertNotTenantScoped_DetectsHandRolledTenantFiltering sets, and its
// _Helper counterpart checks — see
// TestAssertIsolated_DetectsBrokenGetTenantID's doc comment for why a
// subprocess, rather than a plain t.Run, is required here.
const runHandRolledFilteringHelperEnv = "TENANCYTEST_RUN_HAND_ROLLED_FILTERING_HELPER"

// TestAssertNotTenantScoped_DetectsHandRolledTenantFiltering_Helper is not
// meant to run as part of the ordinary suite — it skips itself unless
// runHandRolledFilteringHelperEnv is set — and exists solely to be invoked
// as a subprocess by TestAssertNotTenantScoped_DetectsHandRolledTenantFiltering.
//
// filteredSetting itself implements no dbkit.TenantScoped, so it sails
// through AssertNotTenantScoped's static interface check — deliberately,
// since that is not what this scenario is about (see
// TestAssertNotTenantScoped_DetectsAccidentallyImplementedTenantScoped for
// that check). Its findFn instead hand-filters by whatever tenant happens
// to be in context, simulating a real bug class the interface check alone
// cannot see: identity/platform data queried through code that someone
// hand-wrote a "WHERE tenant_id = ?" into, entirely outside dbkit's own
// plugin and Repository[T] (the backend coding standard's §3.2 forbids
// exactly this, but a test suite's job is to catch it when it happens
// anyway). AssertNotTenantScoped's delta-based, multi-context comparison
// must catch this too, not just a model that opted into TenantScoped
// honestly.
func TestAssertNotTenantScoped_DetectsHandRolledTenantFiltering_Helper(t *testing.T) {
	if os.Getenv(runHandRolledFilteringHelperEnv) != "1" {
		t.Skip("only meant to run as TestAssertNotTenantScoped_DetectsHandRolledTenantFiltering's subprocess")
	}

	db := testutil.Dialects()[0].NewDB(t) // sqlite: this fixture needs no dialect-specific behavior
	if err := db.Exec(createFilteredSettingsTableSQL).Error; err != nil {
		t.Fatalf("create filtered_settings table: %v", err)
	}

	createFn := func(db *gorm.DB) error {
		tenant, _ := pkgcore.TenantFromContext(db.Statement.Context)
		key := fmt.Sprintf("filtered-%d", filteredSettingKeySeq.Add(1))
		return db.Create(&filteredSetting{Key: key, TenantID: string(tenant)}).Error
	}
	findFn := func(db *gorm.DB) (int64, error) {
		q := db.Model(&filteredSetting{})
		if tenant, ok := pkgcore.TenantFromContext(db.Statement.Context); ok {
			q = q.Where("tenant_id = ?", string(tenant))
		}
		var n int64
		err := q.Count(&n).Error
		return n, err
	}

	AssertNotTenantScoped(t, db, filteredSetting{}, createFn, findFn)
}

// TestAssertNotTenantScoped_DetectsHandRolledTenantFiltering is
// AssertNotTenantScoped's second negative test, proving its behavioral
// check has real teeth beyond the static dbkit.TenantScoped check that
// TestAssertNotTenantScoped_DetectsAccidentallyImplementedTenantScoped
// already covers. See TestAssertIsolated_DetectsBrokenGetTenantID's doc
// comment for why this runs its failing scenario as a subprocess
// (testutil.ExpectFailingSubprocess) instead of a plain t.Run.
func TestAssertNotTenantScoped_DetectsHandRolledTenantFiltering(t *testing.T) {
	output, failed := testutil.ExpectFailingSubprocess(t,
		"TestAssertNotTenantScoped_DetectsHandRolledTenantFiltering_Helper",
		runHandRolledFilteringHelperEnv)
	if !failed {
		t.Fatalf("AssertNotTenantScoped passed against a findFn that hand-filters by tenant; want it to fail (the count must not depend on which tenant, if any, is in context). Helper subprocess output:\n%s", output)
	}
	if !strings.Contains(output, "findFn result depends on which tenant") {
		t.Errorf("helper subprocess failed, as wanted, but apparently not for the expected reason; output:\n%s", output)
	}
}
