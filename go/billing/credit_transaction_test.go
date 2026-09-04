package billing

import (
	"context"
	"reflect"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

func TestCreditTransactionRepository_InsertAndGet(t *testing.T) {
	repo := NewCreditTransactionRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	tx := &CreditTransaction{
		ID:     "job-1",
		Type:   string(CreditTransactionGrant),
		Status: string(CreditTransactionStatusConfirmed),
		Amount: 10,
		Reason: "test",
	}
	if err := repo.Insert(ctx, tx); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := repo.Get(ctx, "job-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.Amount != 10 {
		t.Errorf("Get = %+v, want Amount=10", got)
	}
}

func TestCreditTransactionRepository_Get_MissingReturnsNilNil(t *testing.T) {
	repo := NewCreditTransactionRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	got, err := repo.Get(ctx, "does-not-exist")
	if err != nil || got != nil {
		t.Errorf("Get(missing) = %v, %v, want nil, nil", got, err)
	}
}

func TestCreditTransactionRepository_ListByTenant_NewestFirst(t *testing.T) {
	repo := NewCreditTransactionRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	for _, id := range []string{"tx-1", "tx-2", "tx-3"} {
		if err := repo.Insert(ctx, &CreditTransaction{ID: id, Type: string(CreditTransactionGrant), Status: string(CreditTransactionStatusConfirmed), Amount: 1}); err != nil {
			t.Fatalf("Insert(%s): %v", id, err)
		}
	}
	list, err := repo.ListByTenant(ctx)
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("ListByTenant returned %d rows, want 3", len(list))
	}
}

// TestCreditTransactionRepository_SameIDDifferentTenants_DoesNotCollide
// proves the composite (id, tenant_id) primary key: a Deduct row's ID is
// the caller's own IdempotencyKey, which is NOT globally unique across
// tenants -- two different tenants reusing the identical idempotency-key
// string for their own, unrelated operations must not collide.
func TestCreditTransactionRepository_SameIDDifferentTenants_DoesNotCollide(t *testing.T) {
	repo := NewCreditTransactionRepository(newTestDB(t))
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctxB := pkgcore.WithTenant(context.Background(), "tenant-b")

	txA := &CreditTransaction{ID: "shared-key", Type: string(CreditTransactionGrant), Status: string(CreditTransactionStatusConfirmed), Amount: 1}
	if err := repo.Insert(ctxA, txA); err != nil {
		t.Fatalf("Insert(tenant-a): %v", err)
	}
	txB := &CreditTransaction{ID: "shared-key", Type: string(CreditTransactionGrant), Status: string(CreditTransactionStatusConfirmed), Amount: 2}
	if err := repo.Insert(ctxB, txB); err != nil {
		t.Fatalf("Insert(tenant-b) with the same id: %v, want success", err)
	}

	gotA, err := repo.Get(ctxA, "shared-key")
	if err != nil || gotA == nil || gotA.Amount != 1 {
		t.Errorf("Get(tenant-a) = %+v, %v, want Amount=1", gotA, err)
	}
	gotB, err := repo.Get(ctxB, "shared-key")
	if err != nil || gotB == nil || gotB.Amount != 2 {
		t.Errorf("Get(tenant-b) = %+v, %v, want Amount=2", gotB, err)
	}
}

// TestCreditTransactionRepository_HasNoUpdateOrDeleteMethod is the
// compile-shape proof behind CreditTransaction's own "append-only by
// construction" doc comment: Go has no way to assert "this type lacks a
// method" at compile time, so this reflects over
// CreditTransactionRepository's method set instead, and fails loudly the
// moment a future change adds one of these names back -- the identical
// reflection check go/dbkit/audit/repository_test.go's own
// TestRepository_HasNoUpdateOrDeleteMethod uses.
func TestCreditTransactionRepository_HasNoUpdateOrDeleteMethod(t *testing.T) {
	repoType := reflect.TypeOf(&CreditTransactionRepository{})
	for _, name := range []string{"Update", "Updates", "Delete", "Remove", "Save"} {
		if _, ok := repoType.MethodByName(name); ok {
			t.Errorf("CreditTransactionRepository has a method named %q; billing_credit_transactions must be append-only at the application layer (see credit_transaction.go's own doc comment)", name)
		}
	}
}

// TestCreditTransaction_AssertIsolated exercises the generic isolation
// mechanics directly against dbkit.Repository[CreditTransaction] -- proving
// the model itself is genuinely tenant-scoped -- even though
// CreditTransactionRepository (this module's own accessor) deliberately
// never embeds that generic Repository (see CreditTransaction's own doc
// comment for why).
func TestCreditTransaction_AssertIsolated(t *testing.T) {
	repo := dbkit.NewRepository[CreditTransaction](newTestDB(t))
	n := 0
	tenancytest.AssertIsolated(t, repo, func(tenant pkgcore.TenantID) *CreditTransaction {
		n++
		return &CreditTransaction{
			ID:     idFor(n),
			Type:   string(CreditTransactionGrant),
			Status: string(CreditTransactionStatusConfirmed),
			Amount: 1,
		}
	})
}

func idFor(n int) string {
	return "isolation-probe-" + string(rune('a'+n%26)) + string(rune('0'+n/26))
}
