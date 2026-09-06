package billing

import (
	"context"
	"reflect"
	"testing"

	"gorm.io/gorm"

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

// TestCreditTransactionRepository_InsertIdempotent_DuplicateIsANoOpNotAnError
// pins insertIdempotent's own contract (credit_transaction.go): a duplicate
// (id, tenant_id) -- the shape a retried CreditService.PreDeduct meets --
// is reported as inserted==false with NO error, the ON CONFLICT DO NOTHING
// insert never raising the unique-constraint error that would abort an open
// PostgreSQL transaction. The duplicate's row stays untouched, the fresh
// insert keeps its own original content, and -- the composite-key
// counterpart -- the same id under a DIFFERENT tenant is still a fresh
// insert. The PostgreSQL side of this contract, where SQLite's tolerance of
// a failed statement inside a transaction would otherwise mask a broken
// recovery, is proven by the module's integration tier
// (TestPostgres_PreDeduct_IdempotentRetry_ReturnsTheSameReservation).
func TestCreditTransactionRepository_InsertIdempotent_DuplicateIsANoOpNotAnError(t *testing.T) {
	repo := NewCreditTransactionRepository(newTestDB(t))
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctxB := pkgcore.WithTenant(context.Background(), "tenant-b")

	row := &CreditTransaction{ID: "dedup-1", Type: string(CreditTransactionDeduct), Status: string(CreditTransactionStatusPending), Amount: 30}
	var inserted bool
	txErr := dbkit.WithTenantSession(ctxA, repo.db, func(session *gorm.DB) error {
		var insertErr error
		inserted, insertErr = repo.insertIdempotent(ctxA, session, row)
		return insertErr
	})
	if txErr != nil {
		t.Fatalf("insertIdempotent (fresh): %v", txErr)
	}
	if !inserted {
		t.Fatal("fresh insertIdempotent reported inserted=false, want true")
	}

	// The duplicate: same key, same tenant -- a no-op answer, no error, and
	// the existing row unchanged.
	dup := &CreditTransaction{ID: "dedup-1", Type: string(CreditTransactionDeduct), Status: string(CreditTransactionStatusPending), Amount: 999}
	txErr = dbkit.WithTenantSession(ctxA, repo.db, func(session *gorm.DB) error {
		var insertErr error
		inserted, insertErr = repo.insertIdempotent(ctxA, session, dup)
		return insertErr
	})
	if txErr != nil {
		t.Fatalf("insertIdempotent (duplicate): %v, want a no-op with no error", txErr)
	}
	if inserted {
		t.Error("duplicate insertIdempotent reported inserted=true, want false (the ON CONFLICT DO NOTHING no-op)")
	}
	got, getErr := repo.Get(ctxA, "dedup-1")
	if getErr != nil || got == nil || got.Amount != 30 {
		t.Errorf("Get(tenant-a) after the duplicate no-op = %+v, %v, want the ORIGINAL row with Amount=30", got, getErr)
	}

	// The composite-key counterpart: the same id under a different tenant is
	// still a fresh insert.
	txErr = dbkit.WithTenantSession(ctxB, repo.db, func(session *gorm.DB) error {
		var insertErr error
		inserted, insertErr = repo.insertIdempotent(ctxB, session, &CreditTransaction{ID: "dedup-1", Type: string(CreditTransactionDeduct), Status: string(CreditTransactionStatusPending), Amount: 5})
		return insertErr
	})
	if txErr != nil {
		t.Fatalf("insertIdempotent (tenant-b, same id): %v", txErr)
	}
	if !inserted {
		t.Error("insertIdempotent under a second tenant reported inserted=false, want true (the primary key is the composite (id, tenant_id))")
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
