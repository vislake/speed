package tenancytest

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest/internal/testutil"
)

// This file investigates a combination the package's own doc comment and
// assert_isolated_test.go never consider: what happens when the *gorm.DB
// underlying the *dbkit.Repository[T] handed to AssertIsolated was itself
// obtained through a context.Context that pkgcore.WithSystemContext had
// already elevated -- e.g. a caller that does
// db.WithContext(systemElevatedCtx) before ever constructing the
// Repository, on the theory that "this repository operates with elevated
// privileges by default."
//
// The short answer, proved below: it does not matter, and AssertIsolated
// works identically either way -- because that combination cannot actually
// smuggle anything through. Every dbkit.Repository[T] method
// (Create/FindByID/Update/Delete/List) ignores whatever context its own
// *gorm.DB happened to be constructed or last used with; each one instead
// calls dbkit.WithTenantSession(ctx, r.db, fn), which itself calls
// r.db.WithContext(ctx) using the ctx PARAMETER PASSED TO THAT METHOD CALL
// (AssertIsolated's own ctxA/ctxB), not r.db's own stored context. GORM's
// WithContext replaces a session's Statement.Context outright rather than
// merging it with whatever the receiver already carried (confirmed by the
// decoy-tenant assertions below, not merely asserted from reading GORM's
// source), so a context value set on the base *gorm.DB before
// dbkit.NewRepository[T] ever saw it has no path left by which it could
// reach an actual query. "Elevating" the base db this way is not dangerous
// -- it is simply inert, silently discarded on the first real call.
//
// This means the combination the question asks about does not really
// "make sense" as a way to grant a Repository[T] elevated privileges: there
// is no such thing as an "elevated Repository[T]" today, only individual
// elevated CONTEXT VALUES passed to individual method calls (and, per
// system_context_repository_test.go in the parent tenancy package, even
// those have no effect on Repository[T] yet either). A caller trying to
// build a "privileged repository" by pre-elevating db is relying on
// behavior that silently does nothing, which is a plausible real mistake --
// exactly the kind AssertIsolated ought to remain correct in the presence
// of, which is what this file confirms rather than assumes.

// decoyTenantWidget is a tenant-scoped fixture local to this file, distinct
// from sprocket and brokenTenantWidget so this investigation does not lean
// on either of their setups. It deliberately carries a non-key Note field
// -- see assert_isolated_minimal_model_test.go in this same package for why
// that is load-bearing here rather than cosmetic: a TenantScoped model
// whose ONLY fields are its "ID"/"TenantID" primary key trips an unrelated
// dbkit.Repository[T].Update defect this file has nothing to do with
// investigating, and every other fixture in this package (sprocket,
// platformSetting) already avoids it the same way, by happening to declare
// an extra column.
type decoyTenantWidget struct {
	ID       string `gorm:"column:id;primaryKey;size:64"`
	TenantID string `gorm:"column:tenant_id;primaryKey;size:64"`
	Note     string `gorm:"column:note;size:64"`
}

func (d decoyTenantWidget) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(d.TenantID) }

var _ dbkit.TenantScoped = decoyTenantWidget{}

const createDecoyTenantWidgetsTableSQL = `CREATE TABLE decoy_tenant_widgets (
	id VARCHAR(64) NOT NULL,
	tenant_id VARCHAR(64) NOT NULL,
	note VARCHAR(64) NOT NULL DEFAULT '',
	PRIMARY KEY (tenant_id, id)
)`

var decoyTenantWidgetIDSeq atomic.Uint64

func newDecoyTenantWidget(tenant pkgcore.TenantID) *decoyTenantWidget {
	return &decoyTenantWidget{
		ID:       fmt.Sprintf("decoy-widget-%d", decoyTenantWidgetIDSeq.Add(1)),
		TenantID: string(tenant),
		Note:     "gadget",
	}
}

// decoySystemPurpose is registered once for every subtest in this file; see
// pkgcore.RegisterSystemPurpose's own doc comment for why registering the
// same purpose more than once is a safe no-op.
const decoySystemPurpose = pkgcore.SystemPurpose("tenancytest.assert_isolated_system_context.decoy")

// TestAssertIsolated_SystemCtxElevatedDB proves
// AssertIsolated's own guarantees hold unchanged when the *gorm.DB behind
// the Repository[T] it is given was built from a base context that was
// itself elevated via pkgcore.WithSystemContext AND ALSO carried a decoy
// tenant no test-driven call ever uses. If that base context leaked into
// any real query -- through some future refactor of WithTenantSession that
// starts trusting r.db's own stored context instead of always deriving from
// the per-call ctx -- rows would end up written under decoyTenant instead
// of the tenants AssertIsolated itself derives from t.Name(), and
// AssertIsolated's own subtests (same_tenant_find_succeeds in particular)
// would fail on their own. The explicit decoy-tenant row count check at the
// end additionally rules out a subtler failure AssertIsolated's own
// assertions would not catch by themselves: rows silently ALSO becoming
// visible under decoyTenant without disturbing tenant A/B's own view of
// their data.
//
// SQLite only -- see TestAssertIsolated_Sprocket's doc comment
// (assert_isolated_test.go) for why a plain _test.go file must not reach
// testutil.Dialects()'s postgres entry. The postgres leg of this same
// scenario runs from
// tenancytest/integration_test/postgres_assert_isolated_system_context_test.go,
// behind //go:build integration.
func TestAssertIsolated_SystemCtxElevatedDB(t *testing.T) {
	pkgcore.RegisterSystemPurpose(decoySystemPurpose)

	const decoyTenant = pkgcore.TenantID("tenancytest-decoy-tenant-should-never-be-used")
	decoyCtx := pkgcore.WithTenant(context.Background(), decoyTenant)
	elevatedDecoyCtx, err := pkgcore.WithSystemContext(decoyCtx, pkgcore.SystemReason{
		Actor: "test-setup", Purpose: decoySystemPurpose, Ticket: "SUP-DECOY",
	})
	if err != nil {
		t.Fatalf("pkgcore.WithSystemContext(decoy) error = %v, want success", err)
	}

	baseDB := testutil.Dialects()[0].NewDB(t) // sqlite: see doc comment above for why postgres runs only in integration_test/
	if err = baseDB.Exec(createDecoyTenantWidgetsTableSQL).Error; err != nil {
		t.Fatalf("create decoy_tenant_widgets table: %v", err)
	}

	// The critical setup step: build the Repository[T] from a *gorm.DB
	// whose OWN base session context is already elevated and already
	// tenant-scoped to the decoy tenant, before AssertIsolated ever calls
	// Create/FindByID/List with its own, unrelated per-call contexts.
	elevatedBase := baseDB.WithContext(elevatedDecoyCtx)
	repo := dbkit.NewRepository[decoyTenantWidget](elevatedBase)

	AssertIsolated(t, repo, newDecoyTenantWidget)

	// Explicit confirmation, beyond whatever AssertIsolated itself already
	// checked: the decoy tenant baked into the base db's context never
	// received any row at all. Reading it back uses an ordinary,
	// non-elevated context for the decoy tenant -- deliberately not
	// elevatedDecoyCtx again -- so this List call is itself a plain,
	// unprivileged same-tenant read.
	decoyRows, err := repo.List(pkgcore.WithTenant(context.Background(), decoyTenant))
	if err != nil {
		t.Fatalf("List(decoy tenant) error = %v", err)
	}
	if len(decoyRows) != 0 {
		t.Errorf("List(decoy tenant) = %d rows, want 0; the Repository's base *gorm.DB context (elevated, scoped to the decoy tenant) must never leak into an AssertIsolated-driven write", len(decoyRows))
	}
}
