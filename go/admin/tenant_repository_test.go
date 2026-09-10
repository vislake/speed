package admin

import (
	"context"
	"sync"
	"testing"

	"github.com/vislake/speed/go/admin/internal/testutil"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/tenancy"
)

func TestTenantRepository_Create_DuplicateTenantID_ReportsAlreadyExists(t *testing.T) {
	db := testutil.NewDB(t)
	repo := NewTenantRepository(db)
	ctx := context.Background()

	if err := repo.Create(ctx, &Tenant{TenantID: "tenant-a"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	err := repo.Create(ctx, &Tenant{TenantID: "tenant-a"})
	if !apperr.HasCode(err, ErrTenantAlreadyExists.Code) {
		t.Fatalf("Create() error = %v, want ErrTenantAlreadyExists", err)
	}
}

func TestTenantRepository_Create_EmptyTenantID_ReportsIDRequired(t *testing.T) {
	db := testutil.NewDB(t)
	repo := NewTenantRepository(db)

	err := repo.Create(context.Background(), &Tenant{})
	if !apperr.HasCode(err, ErrTenantIDRequired.Code) {
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

// TestTenantRepository_EnsureExists_DoesNotOverwriteManualRow pins the
// idempotent-population contract: a redelivered org.node.created event,
// or a tenant already registered manually, must never clobber an
// operator's own edits (DisplayName, Notes, ...).
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
	if !apperr.HasCode(err, ErrTenantNotFound.Code) {
		t.Fatalf("Get() error = %v, want ErrTenantNotFound", err)
	}
}

func TestTenantRepository_List_FiltersByStatus(t *testing.T) {
	db := testutil.NewDB(t)
	repo := NewTenantRepository(db)
	ctx := context.Background()

	if err := repo.Create(ctx, &Tenant{TenantID: "tenant-active", Status: tenancy.TenantStatusActive}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := repo.Create(ctx, &Tenant{TenantID: "tenant-suspended", Status: tenancy.TenantStatusSuspended}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	rows, err := repo.List(ctx, TenantFilter{Status: tenancy.TenantStatusSuspended})
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

	if err := repo.Create(ctx, &Tenant{TenantID: "tenant-d", Status: tenancy.TenantStatusActive}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	suspended := tenancy.TenantStatusSuspended
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

	active := tenancy.TenantStatusActive
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
	if !apperr.HasCode(err, ErrTenantNotFound.Code) {
		t.Fatalf("Update() error = %v, want ErrTenantNotFound", err)
	}
}

// TestTenantRepository_Update_ConcurrentConflictingPatches_SecondRefused
// pins the guarded-write contract: two concurrent PATCH calls that both
// read the row before either wrote back (a suspend racing a resume) must
// not both silently succeed with the second's stale read overwriting the
// first's intent.
//
// The race is forced deterministically, not by hoping two real Update
// calls interleave within a narrow timing window: both goroutines are
// handed the SAME pre-read snapshot (read via one real Get, matching
// exactly what two callers who both read the row moments apart would each
// have observed) and call applyGuardedPatch directly, concurrently, so the
// database's own conditional UPDATE -- not incidental goroutine scheduling
// -- is what decides which one lands.
func TestTenantRepository_Update_ConcurrentConflictingPatches_SecondRefused(t *testing.T) {
	db := testutil.NewDB(t)
	repo := NewTenantRepository(db)
	ctx := context.Background()

	if err := repo.Create(ctx, &Tenant{TenantID: "tenant-race", Status: tenancy.TenantStatusActive, DisplayName: "Race Co"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	observed, err := repo.Get(ctx, "tenant-race")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	suspended := tenancy.TenantStatusSuspended
	renamed := "Renamed Co"

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	results := make([]*Tenant, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		results[0], errs[0] = repo.applyGuardedPatch(ctx, "tenant-race", *observed, TenantPatch{Status: &suspended})
	}()
	go func() {
		defer wg.Done()
		<-start
		results[1], errs[1] = repo.applyGuardedPatch(ctx, "tenant-race", *observed, TenantPatch{DisplayName: &renamed})
	}()
	close(start)
	wg.Wait()

	succeeded, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case apperr.HasCode(err, ErrTenantConcurrentUpdate.Code):
			refused++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if succeeded != 1 || refused != 1 {
		t.Fatalf("got succeeded=%d refused=%d (errs=%v), want exactly one success and one ErrTenantConcurrentUpdate refusal", succeeded, refused, errs)
	}

	// The persisted row must match EXACTLY the winner's own intended
	// write -- never a hybrid of both patches, and never the loser's
	// patch silently applied on top.
	final, err := repo.Get(ctx, "tenant-race")
	if err != nil {
		t.Fatalf("final Get() error = %v", err)
	}
	var winner *Tenant
	if errs[0] == nil {
		winner = results[0]
	} else {
		winner = results[1]
	}
	if final.Status != winner.Status || final.DisplayName != winner.DisplayName {
		t.Fatalf("persisted row = %+v, want exactly the winner's own write %+v", final, winner)
	}
}
