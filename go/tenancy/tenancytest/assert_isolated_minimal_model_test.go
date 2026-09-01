package tenancytest

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest/internal/testutil"
)

// This file used to document an upstream dbkit.Repository[T].Update defect
// that independent testing of THIS package surfaced: AssertIsolated, run
// against a TenantScoped model whose ONLY fields are its "ID"/"TenantID"
// primary key (no other column at all), used to correctly report a failure
// -- but the failure was dbkit's, not AssertIsolated's, own.
//
// Root cause, confirmed by tracing gorm.io/gorm v1.31.2's own
// callbacks/update.go: Repository[T].Update issues
// tx.Where(id...).Where(tenant_id...).Select("*").Save(m). GORM's Update
// callback builds the statement's SET clause from ConvertToAssignments(stmt)
// -- which considers only NON-primary-key columns, since primary-key
// columns belong in the WHERE clause, not SET -- and when that came back
// empty, the callback took an early return (see callbacks/update.go's
// Update func) that built and executed no SQL at all, leaving
// RowsAffected at 0. dbkit.Repository[T] treated rowsAffected == 0 as "no
// such row for this tenant" and returned ErrRecordNotFound (see
// repository.go's Update), even though the row genuinely existed and was
// genuinely owned by the calling tenant.
//
// dbkit.Repository[T].Update has since closed this gap: when gorm reports
// that no SQL was built at all (not merely that RowsAffected == 0), Update
// now falls back to an explicit, still id-and-tenant-scoped existence
// check inside the same transaction before deciding between success and
// ErrRecordNotFound -- see Update's own doc comment in
// go/dbkit/repository.go, and TestRepository_Update_IDAndTenantIDOnlyModel_SucceedsAsNoOp
// / _DifferentTenant_ReturnsNotFound / _NoSuchID_ReturnsNotFound in
// go/dbkit/repository_test.go for dbkit's own fix and its regression
// coverage (including proof the fallback stays tenant-isolated: a
// cross-tenant Update attempt against this exact model shape still
// collapses to ErrRecordNotFound, never a false-positive success).
//
// What stays in THIS package, now that the dbkit gap itself is fixed at
// its source, is proof that AssertIsolated correctly PASSES bareTenantWidget
// -- the narrowest legal TenantScoped shape, a "pure marker" link/tenant
// table whose entire content IS its identity and tenant (a plausible,
// spec-legal shape for the tenant/link data domain, backend coding
// standard §3.3) -- rather than this package silently losing coverage of
// that shape now that dbkit no longer trips on it.
// TestAssertIsolated_MinimalTenantScopedModel is now a forward-looking
// regression guard, not a defect report: if dbkit's Update fallback above
// is ever weakened or removed, this is the test that notices, by failing
// on the same same_tenant_update_succeeds check that used to fail before
// the dbkit fix.

// bareTenantWidget is deliberately the narrowest possible TenantScoped
// model: nothing beyond the "ID"/"TenantID" pair dbkit.Repository[T]
// itself requires. Every OTHER fixture in this package (sprocket,
// platformSetting, decoyTenantWidget, brokenTenantWidget included)
// declares at least one additional column and so never exercises this
// path; that is precisely the gap this file closes.
type bareTenantWidget struct {
	ID       string `gorm:"column:id;primaryKey;size:64"`
	TenantID string `gorm:"column:tenant_id;primaryKey;size:64"`
}

func (b bareTenantWidget) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(b.TenantID) }

var _ dbkit.TenantScoped = bareTenantWidget{}

const createBareTenantWidgetsTableSQL = `CREATE TABLE bare_tenant_widgets (
	id VARCHAR(64) NOT NULL,
	tenant_id VARCHAR(64) NOT NULL,
	PRIMARY KEY (tenant_id, id)
)`

var bareTenantWidgetIDSeq atomic.Uint64

func newBareTenantWidget(tenant pkgcore.TenantID) *bareTenantWidget {
	return &bareTenantWidget{
		ID:       fmt.Sprintf("bare-%d", bareTenantWidgetIDSeq.Add(1)),
		TenantID: string(tenant),
	}
}

// TestAssertIsolated_MinimalTenantScopedModel proves AssertIsolated works
// end to end against the narrowest legal TenantScoped shape: a model with
// no column beyond ID/TenantID. See this file's doc comment for the
// dbkit.Repository[T].Update gap this test used to expose (fixed in
// go/dbkit/repository.go) and why this test's job, now that the fix is in
// place, is to guard against it ever regressing rather than to document
// the defect itself -- a plain, direct assertion that AssertIsolated
// passes, exactly like TestAssertIsolated_Sprocket in assert_isolated_test.go,
// rather than the subprocess-based "expect a failure" machinery this test
// used before the dbkit fix landed (that machinery is still the right tool
// for a genuinely-expected failure; see
// TestAssertIsolated_DetectsBrokenGetTenantID in assert_isolated_test.go
// for that pattern and why it exists).
//
// SQLite only, deliberately, mirroring TestAssertIsolated_Sprocket in
// assert_isolated_test.go: this file carries no build tag, so per the
// backend coding standard's testing layout rule (§13) it must stay fast and
// hermetic under a plain "go test ./...". testutil.Dialects()[0] is always
// SQLite; see that function's own doc comment for why a plain _test.go file
// must never reach index 1 (PostgreSQL, via a real disposable container).
func TestAssertIsolated_MinimalTenantScopedModel(t *testing.T) {
	db := testutil.Dialects()[0].NewDB(t) // sqlite: see doc comment above for why postgres has no place in this file
	if err := db.Exec(createBareTenantWidgetsTableSQL).Error; err != nil {
		t.Fatalf("create bare_tenant_widgets table: %v", err)
	}
	repo := dbkit.NewRepository[bareTenantWidget](db)

	AssertIsolated(t, repo, newBareTenantWidget)
}
