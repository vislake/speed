//go:build integration

package tenancytest_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
	"github.com/vislake/speed/go/tenancy/tenancytest/internal/testutil"
)

// decoyTenantWidget mirrors the unexported fixture of the same name in
// assert_isolated_system_context_test.go (package tenancytest, one
// directory up) -- see postgres_assert_isolated_test.go's doc comment on
// sprocket for why this package defines its own copy rather than sharing
// one.
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

// decoySystemPurpose is this package's own registration, distinct from the
// unit tier's identically-purposed constant in
// assert_isolated_system_context_test.go: the two run as separate test
// binaries (different packages), so nothing requires the string values to
// match, and keeping them textually distinct avoids any confusion if a
// system-purpose audit log ever sees both. See pkgcore.RegisterSystemPurpose's
// own doc comment for why registering a purpose more than once is a safe
// no-op.
const decoySystemPurpose = pkgcore.SystemPurpose("tenancytest.assert_isolated_system_context.decoy_postgres")

// TestAssertIsolated_SystemCtxElevatedDB_Postgres is the postgres-dialect
// leg of tenancytest.TestAssertIsolated_SystemCtxElevatedDB
// (assert_isolated_system_context_test.go, package tenancytest); see that
// test's doc comment for what it proves and postgres_assert_isolated_test.go's
// own doc comment for why the postgres leg lives here instead of there.
func TestAssertIsolated_SystemCtxElevatedDB_Postgres(t *testing.T) {
	pkgcore.RegisterSystemPurpose(decoySystemPurpose)

	const decoyTenant = pkgcore.TenantID("tenancytest-decoy-tenant-should-never-be-used")
	decoyCtx := pkgcore.WithTenant(context.Background(), decoyTenant)
	elevatedDecoyCtx, err := pkgcore.WithSystemContext(decoyCtx, pkgcore.SystemReason{
		Actor: "test-setup", Purpose: decoySystemPurpose, Ticket: "SUP-DECOY",
	})
	if err != nil {
		t.Fatalf("pkgcore.WithSystemContext(decoy) error = %v, want success", err)
	}

	baseDB := testutil.Dialects()[1].NewDB(t) // postgres: see postgres_assert_isolated_test.go's doc comment
	if err = baseDB.Exec(createDecoyTenantWidgetsTableSQL).Error; err != nil {
		t.Fatalf("create decoy_tenant_widgets table: %v", err)
	}

	// The critical setup step: build the Repository[T] from a *gorm.DB
	// whose OWN base session context is already elevated and already
	// tenant-scoped to the decoy tenant, before AssertIsolated ever calls
	// Create/FindByID/List with its own, unrelated per-call contexts.
	elevatedBase := baseDB.WithContext(elevatedDecoyCtx)
	repo := dbkit.NewRepository[decoyTenantWidget](elevatedBase)

	tenancytest.AssertIsolated(t, repo, newDecoyTenantWidget)

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
