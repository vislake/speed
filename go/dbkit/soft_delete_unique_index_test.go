package dbkit

import (
	"testing"

	"github.com/vislake/speed/go/dbkit/internal/testutil"
)

// TestSoftDeleteUniqueIndex_NameReusableAfterSoftDelete_ViaPartialIndex is
// this round's proof for the unique-index interaction
// docs/internal/04-data-and-tenancy.md's delete-semantics section (§4) requires every
// soft-deletable model to decide explicitly: a soft-deleted row is still a
// real row and still occupies a plain unique constraint. This round's
// answer, for SoftDeletableWidget (testutil.SoftDeletableWidgetTableSQL),
// is a partial unique index on (tenant_id, name) WHERE deleted_at IS NULL —
// proven here to actually let a name be reused immediately after its
// holder is soft-deleted, on SQLite; the identical DDL string is exercised
// again against real PostgreSQL in
// integration_test/postgres_soft_delete_rls_test.go, which is the
// dual-dialect half of this proof. See go/dbkit/AGENTS.md's "Soft
// deletion" section for the general guidance this backs, and this
// fixture's own doc comment for why the alternative ("no reuse until
// hard-deleted") remains legitimate for a model that wants it instead.
func TestSoftDeleteUniqueIndex_NameReusableAfterSoftDelete_ViaPartialIndex(t *testing.T) {
	repo := newSoftDeletableWidgetRepo(t)
	ctx := ctxTenantActor("tenant-a", "user-1")

	original := &testutil.SoftDeletableWidget{ID: "w1", Name: "x"}
	if err := repo.Create(ctx, original); err != nil {
		t.Fatalf("Create(original) error = %v", err)
	}

	// Attempting to create a second live row with the same name, before any
	// delete, must still be rejected — proving the partial index really is
	// enforcing uniqueness among live rows, not merely absent.
	dupe := &testutil.SoftDeletableWidget{ID: "w-dupe", Name: "x"}
	if err := repo.Create(ctx, dupe); err == nil {
		t.Fatal("Create() of a second live row with the same name succeeded, want a unique-constraint violation")
	}

	if err := repo.Delete(ctx, original.ID); err != nil {
		t.Fatalf("Delete(original) error = %v", err)
	}

	// The partial index no longer covers the soft-deleted row (its
	// deleted_at is no longer NULL), so a brand new row with the same name
	// under the same tenant must now succeed.
	reborn := &testutil.SoftDeletableWidget{ID: "w2", Name: "x"}
	if err := repo.Create(ctx, reborn); err != nil {
		t.Fatalf("Create() reusing the name after soft-delete error = %v, want success (partial unique index WHERE deleted_at IS NULL)", err)
	}

	got, err := repo.FindByID(ctx, reborn.ID)
	if err != nil {
		t.Fatalf("FindByID(reborn) error = %v", err)
	}
	if got.Name != "x" {
		t.Errorf("FindByID(reborn).Name = %q, want %q", got.Name, "x")
	}
}

// TestSoftDeleteUniqueIndex_TwoTenantsCanEachHaveALiveRowWithTheSameName
// proves the partial index is still tenant-scoped via its leftmost column:
// two tenants each holding a live "x" concurrently must not collide with
// one another, exactly as an ordinary (non-partial) UNIQUE(tenant_id, name)
// index would behave.
func TestSoftDeleteUniqueIndex_TwoTenantsCanEachHaveALiveRowWithTheSameName(t *testing.T) {
	repo := newSoftDeletableWidgetRepo(t)
	ctxA := ctxTenantActor("tenant-a", "user-1")
	ctxB := ctxTenantActor("tenant-b", "user-1")

	if err := repo.Create(ctxA, &testutil.SoftDeletableWidget{ID: "a1", Name: "x"}); err != nil {
		t.Fatalf("Create(tenant-a) error = %v", err)
	}
	if err := repo.Create(ctxB, &testutil.SoftDeletableWidget{ID: "b1", Name: "x"}); err != nil {
		t.Fatalf("Create(tenant-b) error = %v, want success -- the partial index is scoped by tenant_id, its leftmost column", err)
	}
}
