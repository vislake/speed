package billing

import (
	"context"
	"fmt"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

func TestCreditBalanceRepository_CreateAndFindByID(t *testing.T) {
	repo := NewCreditBalanceRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	bal := &CreditBalance{ID: "tenant-a", Available: 10, Reserved: 0}
	if err := repo.Create(ctx, bal); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := repo.FindByID(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.Available != 10 {
		t.Errorf("Available = %d, want 10", got.Available)
	}
}

// TestCreditBalanceRepository_AssertIsolated exercises
// dbkit.Repository[CreditBalance]'s own generic isolation mechanics --
// AssertIsolated needs several distinct rows per tenant to do so (it
// creates, updates and deletes more than one record under the same
// tenant), so the ids used here are synthetic and independent of the
// application-level "id equals the owning tenant's id, exactly one row
// per tenant" convention CreditService's own ensureBalance establishes
// (see CreditBalance's own doc comment) -- that invariant is upheld by
// CreditService's call pattern, not by a database constraint, so nothing
// here contradicts it.
func TestCreditBalanceRepository_AssertIsolated(t *testing.T) {
	repo := NewCreditBalanceRepository(newTestDB(t))
	n := 0
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *CreditBalance {
		n++
		return &CreditBalance{ID: fmt.Sprintf("probe-balance-%d", n)}
	})
}
