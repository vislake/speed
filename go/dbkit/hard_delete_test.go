package dbkit

import (
	"context"
	"errors"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// hardDeleteTestPurpose is the SystemPurpose the HardDelete tests grant
// themselves. Tests exercise the gate, not the grant's legitimacy, so this
// fixture purpose is declared from the test process rather than from a
// module's Register, exactly as tenancy's own system-context tests do.
const hardDeleteTestPurpose pkgcore.SystemPurpose = "dbkit.test.hard_delete"

// isHardDeleteRefused reports whether err is ErrHardDeleteRequiresSystemContext,
// matched by Code rather than by identity, on the same convention
// isRecordNotFound in repository_test.go follows (apperr.WithParam always
// derives a new *apperr.Error, so pointer identity is not stable).
func isHardDeleteRefused(err error) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == ErrHardDeleteRequiresSystemContext.Code
}

// hardDeleteSystemCtx returns a context for tenant that passes HardDelete's
// system-context gate: a tenant, an actor, and a granted system context
// whose reason names hardDeleteTestPurpose. RegisterSystemPurpose is
// idempotent and mutex-guarded, so the registration here is a no-op from the
// second grant on. Which actor id the reason carries is not load-bearing:
// HardDelete never reads who holds the system context, only that one exists.
func hardDeleteSystemCtx(t *testing.T, tenant, actor string) context.Context {
	t.Helper()
	pkgcore.RegisterSystemPurpose(hardDeleteTestPurpose)
	ctx := pkgcore.WithActor(
		ctxTenant(tenant),
		pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: actor},
	)
	elevated, err := pkgcore.WithSystemContext(ctx, pkgcore.SystemReason{
		Actor:   actor,
		Purpose: hardDeleteTestPurpose,
		Ticket:  "dbkit-hard-delete-unit-test",
	})
	if err != nil {
		t.Fatalf("WithSystemContext() error = %v", err)
	}
	return elevated
}

// rawSoftDeletableWidgetCount returns the physical row count of
// soft_deletable_widgets, read through db.Raw so no GORM callback — in
// particular the soft-delete auto-scope plugin — can hide a row from it, on
// the same ground-truth convention rawSoftDeletableWidgetRow in
// repository_test.go sets.
func rawSoftDeletableWidgetCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Raw(`SELECT COUNT(*) FROM soft_deletable_widgets`).Scan(&n).Error; err != nil {
		t.Fatalf("raw soft_deletable_widgets COUNT(*): %v", err)
	}
	return n
}

func TestRepository_HardDelete_PlainTenantContext_Refused(t *testing.T) {
	repo := newSoftDeletableWidgetRepo(t)
	ctx := ctxTenantActor("tenant-a", "user-1")

	w := &testutil.SoftDeletableWidget{ID: "w1", Name: "gadget"}
	if err := repo.Create(ctx, w); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := repo.HardDelete(ctx, w.ID); !isHardDeleteRefused(err) {
		t.Fatalf("HardDelete() on a plain tenant-scoped context error = %v, want ErrHardDeleteRequiresSystemContext", err)
	}
	if found, _, _ := rawSoftDeletableWidgetRow(t, repo.db, w.ID); !found {
		t.Fatal("row gone after a refused HardDelete; the gate must refuse before the database is touched")
	}
}

func TestRepository_HardDelete_BareContext_Refused(t *testing.T) {
	repo := newSoftDeletableWidgetRepo(t)

	err := repo.HardDelete(context.Background(), "w1")
	if !isHardDeleteRefused(err) {
		t.Fatalf("HardDelete() on a bare context error = %v, want ErrHardDeleteRequiresSystemContext (the gate runs before the tenant check: a bare context must report the system-context refusal, not pkgcore.ErrNoTenant)", err)
	}
}

func TestRepository_HardDelete_SystemContextWithoutTenant_FailsClosed(t *testing.T) {
	repo := newSoftDeletableWidgetRepo(t)

	pkgcore.RegisterSystemPurpose(hardDeleteTestPurpose)
	sysCtx, err := pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{
		Actor:   "retention-job",
		Purpose: hardDeleteTestPurpose,
		Ticket:  "dbkit-hard-delete-unit-test",
	})
	if err != nil {
		t.Fatalf("WithSystemContext() error = %v", err)
	}

	err = repo.HardDelete(sysCtx, "w1")
	if !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Fatalf("HardDelete() with a system context but no tenant error = %v, want pkgcore.ErrNoTenant (a system context never substitutes for a tenant)", err)
	}
}

func TestRepository_HardDelete_SystemContext_PhysicallyDeletesNeverDeletedRow(t *testing.T) {
	repo := newSoftDeletableWidgetRepo(t)
	ctx := ctxTenantActor("tenant-a", "user-1")

	w := &testutil.SoftDeletableWidget{ID: "w1", Name: "gadget"}
	if err := repo.Create(ctx, w); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	sysCtx := hardDeleteSystemCtx(t, "tenant-a", "retention-job")
	if err := repo.HardDelete(sysCtx, w.ID); err != nil {
		t.Fatalf("HardDelete() error = %v", err)
	}
	if n := rawSoftDeletableWidgetCount(t, repo.db); n != 0 {
		t.Fatalf("raw soft_deletable_widgets COUNT(*) = %d after HardDelete, want 0 (the row must be physically gone, not merely marked)", n)
	}

	if err := repo.HardDelete(sysCtx, w.ID); !isRecordNotFound(err) {
		t.Fatalf("second HardDelete() error = %v, want ErrRecordNotFound", err)
	}
}

func TestRepository_HardDelete_SystemContextOnSoftDeletedRow_PhysicallyDeletes(t *testing.T) {
	repo := newSoftDeletableWidgetRepo(t)
	ctx := ctxTenantActor("tenant-a", "user-1")

	w := &testutil.SoftDeletableWidget{ID: "w1", Name: "gadget"}
	if err := repo.Create(ctx, w); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := repo.Delete(ctx, w.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	found, deletedAt, _ := rawSoftDeletableWidgetRow(t, repo.db, w.ID)
	if !found || deletedAt == nil {
		t.Fatalf("after Delete() the row must still exist with deleted_at set (mark-delete); found = %v, deletedAt = %v", found, deletedAt)
	}

	if err := repo.HardDelete(hardDeleteSystemCtx(t, "tenant-a", "retention-job"), w.ID); err != nil {
		t.Fatalf("HardDelete() on a soft-deleted row error = %v", err)
	}
	if found, _, _ := rawSoftDeletableWidgetRow(t, repo.db, w.ID); found {
		t.Fatal("soft-deleted row still physically present after HardDelete; the query-only auto-scope must not turn the DELETE into a no-op")
	}
}

func TestRepository_HardDelete_CrossTenant_SystemContextDoesNotEscapeTenantScope(t *testing.T) {
	repo := newSoftDeletableWidgetRepo(t)

	w := &testutil.SoftDeletableWidget{ID: "w1", Name: "gadget"}
	if err := repo.Create(ctxTenantActor("tenant-a", "user-a"), w); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	err := repo.HardDelete(hardDeleteSystemCtx(t, "tenant-b", "platform-admin"), w.ID)
	if !isRecordNotFound(err) {
		t.Fatalf("HardDelete() of tenant-a's row from tenant-b with a system context error = %v, want ErrRecordNotFound (a system context never escapes the ctx tenant's own rows)", err)
	}
	if found, _, _ := rawSoftDeletableWidgetRow(t, repo.db, w.ID); !found {
		t.Fatal("tenant-a's row is gone after tenant-b's system-context HardDelete; HardDelete must never be a cross-tenant eraser")
	}
}

func TestRepository_HardDelete_NonSoftDeletableModel_MatchesDeleteSemantics(t *testing.T) {
	repo := newWidgetRepo(t)
	ctx := ctxTenant("tenant-a")

	w := &testutil.Widget{ID: "w1", Name: "gadget", Value: 7}
	if err := repo.Create(ctx, w); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// The gate applies to every T, not only to SoftDeletable ones: Delete on
	// a plain tenant context is the sanctioned physical delete, but
	// HardDelete on the same context is refused.
	if err := repo.HardDelete(ctx, w.ID); !isHardDeleteRefused(err) {
		t.Fatalf("HardDelete() on a plain tenant-scoped context error = %v, want ErrHardDeleteRequiresSystemContext", err)
	}

	sysCtx := hardDeleteSystemCtx(t, "tenant-a", "retention-job")
	if err := repo.HardDelete(sysCtx, w.ID); err != nil {
		t.Fatalf("HardDelete() error = %v", err)
	}
	if _, err := repo.FindByID(ctx, w.ID); !isRecordNotFound(err) {
		t.Fatalf("FindByID() after HardDelete() error = %v, want ErrRecordNotFound", err)
	}
	var n int64
	if err := repo.db.Raw(`SELECT COUNT(*) FROM widgets`).Scan(&n).Error; err != nil {
		t.Fatalf("raw widgets COUNT(*): %v", err)
	}
	if n != 0 {
		t.Fatalf("raw widgets COUNT(*) = %d after HardDelete, want 0 (the row must be physically gone)", n)
	}

	if err := repo.HardDelete(sysCtx, w.ID); !isRecordNotFound(err) {
		t.Fatalf("second HardDelete() error = %v, want ErrRecordNotFound", err)
	}
}
