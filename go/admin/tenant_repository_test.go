package admin

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/admin/internal/testutil"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

func TestTenantRepository_Create_DuplicateTenantID_ReportsAlreadyExists(t *testing.T) {
	db := testutil.NewDB(t)
	repo := NewTenantRepository(db)
	ctx := context.Background()

	if err := repo.Create(ctx, &Tenant{TenantID: "tenant-a"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	err := repo.Create(ctx, &Tenant{TenantID: "tenant-a"})
	if !isCode(err, ErrTenantAlreadyExists.Code) {
		t.Fatalf("Create() error = %v, want ErrTenantAlreadyExists", err)
	}
}

func TestTenantRepository_Create_EmptyTenantID_ReportsIDRequired(t *testing.T) {
	db := testutil.NewDB(t)
	repo := NewTenantRepository(db)

	err := repo.Create(context.Background(), &Tenant{})
	if !isCode(err, ErrTenantIDRequired.Code) {
		t.Fatalf("Create() error = %v, want ErrTenantIDRequired", err)
	}
}

func TestTenantRepository_EnsureExists_IsIdempotent(t *testing.T) {
	db := testutil.NewDB(t)
	repo := NewTenantRepository(db)
	ctx := context.Background()

	created1, err := repo.EnsureExists(ctx, "tenant-b")
	if err != nil {
		t.Fatalf("EnsureExists() error = %v", err)
	}
	if !created1 {
		t.Fatal("EnsureExists() created = false on first call, want true")
	}

	created2, err := repo.EnsureExists(ctx, "tenant-b")
	if err != nil {
		t.Fatalf("EnsureExists() second call error = %v, want nil (idempotent)", err)
	}
	if created2 {
		t.Fatal("EnsureExists() created = true on second call, want false")
	}

	rows, err := repo.List(ctx, TenantFilter{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("List() returned %d rows, want exactly 1 (no duplicate row from the redelivery)", len(rows))
	}
}

// TestTenantRepository_EnsureExists_DoesNotOverwriteManualRow is the
// regression test D3's own design requires: a redelivered
// org.node.created event, or a tenant already registered manually, must
// never clobber an operator's own edits (DisplayName, Notes, ...).
func TestTenantRepository_EnsureExists_DoesNotOverwriteManualRow(t *testing.T) {
	db := testutil.NewDB(t)
	repo := NewTenantRepository(db)
	ctx := context.Background()

	if err := repo.Create(ctx, &Tenant{TenantID: "tenant-c", DisplayName: "Acme Corp", CreatedBy: "operator-1"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	created, err := repo.EnsureExists(ctx, "tenant-c")
	if err != nil {
		t.Fatalf("EnsureExists() error = %v", err)
	}
	if created {
		t.Fatal("EnsureExists() created = true, want false (row already existed manually)")
	}

	got, err := repo.Get(ctx, "tenant-c")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.DisplayName != "Acme Corp" || got.CreatedBy != "operator-1" {
		t.Fatalf("Get() = %+v, want the manually-created row untouched", got)
	}
}

func TestTenantRepository_Get_Missing_ReportsNotFound(t *testing.T) {
	db := testutil.NewDB(t)
	repo := NewTenantRepository(db)

	_, err := repo.Get(context.Background(), "does-not-exist")
	if !isCode(err, ErrTenantNotFound.Code) {
		t.Fatalf("Get() error = %v, want ErrTenantNotFound", err)
	}
}

func TestTenantRepository_List_FiltersByStatus(t *testing.T) {
	db := testutil.NewDB(t)
	repo := NewTenantRepository(db)
	ctx := context.Background()

	if err := repo.Create(ctx, &Tenant{TenantID: "tenant-active", Status: TenantStatusActive}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := repo.Create(ctx, &Tenant{TenantID: "tenant-suspended", Status: TenantStatusSuspended}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	rows, err := repo.List(ctx, TenantFilter{Status: TenantStatusSuspended})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(rows) != 1 || rows[0].TenantID != "tenant-suspended" {
		t.Fatalf("List(status=suspended) = %+v, want exactly tenant-suspended", rows)
	}
}

// TestTenantRepository_Update_SuspendThenResume_TracksSuspendedAt pins the
// SuspendedAt derivation rule Update's own doc comment describes: it is
// stamped on a fresh suspension and cleared on resume, never taken as a
// caller-supplied value.
func TestTenantRepository_Update_SuspendThenResume_TracksSuspendedAt(t *testing.T) {
	db := testutil.NewDB(t)
	repo := NewTenantRepository(db)
	ctx := context.Background()

	if err := repo.Create(ctx, &Tenant{TenantID: "tenant-d", Status: TenantStatusActive}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	suspended := TenantStatusSuspended
	reason := "billing overdue"
	got, err := repo.Update(ctx, "tenant-d", TenantPatch{Status: &suspended, SuspendedReason: &reason})
	if err != nil {
		t.Fatalf("Update(suspend) error = %v", err)
	}
	if got.SuspendedAt == nil {
		t.Fatal("Update(suspend) left SuspendedAt nil, want it stamped")
	}
	if got.SuspendedReason != reason {
		t.Fatalf("Update(suspend) SuspendedReason = %q, want %q", got.SuspendedReason, reason)
	}

	active := TenantStatusActive
	got, err = repo.Update(ctx, "tenant-d", TenantPatch{Status: &active})
	if err != nil {
		t.Fatalf("Update(resume) error = %v", err)
	}
	if got.SuspendedAt != nil {
		t.Fatal("Update(resume) left SuspendedAt set, want nil")
	}
}

func TestTenantRepository_Update_Missing_ReportsNotFound(t *testing.T) {
	db := testutil.NewDB(t)
	repo := NewTenantRepository(db)

	_, err := repo.Update(context.Background(), "does-not-exist", TenantPatch{})
	if !isCode(err, ErrTenantNotFound.Code) {
		t.Fatalf("Update() error = %v, want ErrTenantNotFound", err)
	}
}

// isCode reports whether err is an *apperr.Error with the given Code --
// the classify-by-Code convention this whole codebase uses (apperr's
// WithParam/WithCause derive new values, so exported sentinels are
// templates, never singletons == or errors.Is could match).
func isCode(err error, code string) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == code
}
