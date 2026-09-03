package rbac

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// Every table this module owns is tenant-owned -- rbac_roles and
// rbac_role_permissions are tenant data, rbac_role_bindings is link data
// (docs/internal/04-data-and-tenancy.md) -- so all three repositories run
// tenancytest.AssertIsolated and none runs AssertNotTenantScoped. The
// absence of the reverse assertion is deliberate and is recorded in
// AGENTS.md: rbac has no identity-domain or platform-domain table for it
// to assert against. The permission catalog, which IS platform-scoped, has
// no table at all.

func TestRoleRepository_IsTenantIsolated(t *testing.T) {
	repo := NewRoleRepository(newRBACTestDB(t))
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *Role {
		id := uuid.NewString()
		return &Role{ID: id, Key: "role-" + id, DescriptionKey: "rbac.role.custom"}
	})
}

func TestRolePermissionRepository_IsTenantIsolated(t *testing.T) {
	repo := NewRolePermissionRepository(newRBACTestDB(t))
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *RolePermission {
		id := uuid.NewString()
		return &RolePermission{ID: id, RoleID: uuid.NewString(), Permission: "notes:read"}
	})
}

func TestRoleBindingRepository_IsTenantIsolated(t *testing.T) {
	repo := NewRoleBindingRepository(newRBACTestDB(t))
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *RoleBinding {
		id := uuid.NewString()
		return &RoleBinding{ID: id, UserID: uuid.NewString(), RoleID: uuid.NewString()}
	})
}

func TestRoleRepository_ByKey_FindsTheTenantsOwnRole(t *testing.T) {
	repo := NewRoleRepository(newRBACTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	want := &Role{ID: uuid.NewString(), Key: "admin", Builtin: true}
	if err := repo.Create(ctx, want); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.ByKey(ctx, "admin")
	if err != nil {
		t.Fatalf("ByKey: %v", err)
	}
	if got.ID != want.ID {
		t.Fatalf("ByKey returned role %q, want %q", got.ID, want.ID)
	}
	if got.GetTenantID() != pkgcore.TenantID("tenant-a") {
		t.Fatalf("ByKey returned a role owned by %q, want tenant-a", got.GetTenantID())
	}
}

func TestRoleRepository_ByKey_AnotherTenantsRole_IsNotFound(t *testing.T) {
	// The key "admin" exists -- in another tenant. The answer must be
	// indistinguishable from "no such role", or a caller could enumerate
	// another tenant's role keys.
	repo := NewRoleRepository(newRBACTestDB(t))
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctxB := pkgcore.WithTenant(context.Background(), "tenant-b")

	if err := repo.Create(ctxA, &Role{ID: uuid.NewString(), Key: "admin"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, errExisting := repo.ByKey(ctxB, "admin")
	assertErrorCode(t, errExisting, "rbac.role_not_found")

	_, errAbsent := repo.ByKey(ctxB, "never-defined-anywhere")
	assertErrorCode(t, errAbsent, "rbac.role_not_found")
}

func TestRolePermissionRepository_ByRole_DoesNotCrossTenants(t *testing.T) {
	repo := NewRolePermissionRepository(newRBACTestDB(t))
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctxB := pkgcore.WithTenant(context.Background(), "tenant-b")

	// The same role id in two tenants, each granting a different
	// permission: a filtered read that forgot the tenant would return both.
	roleID := uuid.NewString()
	if err := repo.Create(ctxA, &RolePermission{ID: uuid.NewString(), RoleID: roleID, Permission: "notes:read"}); err != nil {
		t.Fatalf("Create in tenant-a: %v", err)
	}
	if err := repo.Create(ctxB, &RolePermission{ID: uuid.NewString(), RoleID: roleID, Permission: "notes:write"}); err != nil {
		t.Fatalf("Create in tenant-b: %v", err)
	}

	gotA, err := repo.ByRole(ctxA, roleID)
	if err != nil {
		t.Fatalf("ByRole in tenant-a: %v", err)
	}
	if len(gotA) != 1 || gotA[0].Permission != "notes:read" {
		t.Fatalf("ByRole in tenant-a = %+v, want exactly the tenant's own notes:read grant", gotA)
	}

	gotB, err := repo.ByRole(ctxB, roleID)
	if err != nil {
		t.Fatalf("ByRole in tenant-b: %v", err)
	}
	if len(gotB) != 1 || gotB[0].Permission != "notes:write" {
		t.Fatalf("ByRole in tenant-b = %+v, want exactly the tenant's own notes:write grant", gotB)
	}
}

func TestRoleBindingRepository_ByUser_DoesNotCrossTenants(t *testing.T) {
	// One person belongs to several tenants -- that is the whole reason
	// users are not tenant-scoped -- so the same user id legitimately has
	// bindings in more than one tenant. A read must return only the
	// tenant's own.
	repo := NewRoleBindingRepository(newRBACTestDB(t))
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctxB := pkgcore.WithTenant(context.Background(), "tenant-b")

	userID := uuid.NewString()
	roleInA, roleInB := uuid.NewString(), uuid.NewString()
	if err := repo.Create(ctxA, &RoleBinding{ID: uuid.NewString(), UserID: userID, RoleID: roleInA}); err != nil {
		t.Fatalf("Create in tenant-a: %v", err)
	}
	if err := repo.Create(ctxB, &RoleBinding{ID: uuid.NewString(), UserID: userID, RoleID: roleInB}); err != nil {
		t.Fatalf("Create in tenant-b: %v", err)
	}

	gotA, err := repo.ByUser(ctxA, userID)
	if err != nil {
		t.Fatalf("ByUser in tenant-a: %v", err)
	}
	if len(gotA) != 1 || gotA[0].RoleID != roleInA {
		t.Fatalf("ByUser in tenant-a = %+v, want exactly the binding to %q", gotA, roleInA)
	}

	gotB, err := repo.ByUser(ctxB, userID)
	if err != nil {
		t.Fatalf("ByUser in tenant-b: %v", err)
	}
	if len(gotB) != 1 || gotB[0].RoleID != roleInB {
		t.Fatalf("ByUser in tenant-b = %+v, want exactly the binding to %q", gotB, roleInB)
	}
}

func TestRoleBindingRepository_ByRole_DoesNotCrossTenants(t *testing.T) {
	repo := NewRoleBindingRepository(newRBACTestDB(t))
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctxB := pkgcore.WithTenant(context.Background(), "tenant-b")

	roleID := uuid.NewString()
	userInA := uuid.NewString()
	if err := repo.Create(ctxA, &RoleBinding{ID: uuid.NewString(), UserID: userInA, RoleID: roleID}); err != nil {
		t.Fatalf("Create in tenant-a: %v", err)
	}
	if err := repo.Create(ctxB, &RoleBinding{ID: uuid.NewString(), UserID: uuid.NewString(), RoleID: roleID}); err != nil {
		t.Fatalf("Create in tenant-b: %v", err)
	}

	gotA, err := repo.ByRole(ctxA, roleID)
	if err != nil {
		t.Fatalf("ByRole in tenant-a: %v", err)
	}
	if len(gotA) != 1 || gotA[0].UserID != userInA {
		t.Fatalf("ByRole in tenant-a = %+v, want exactly the tenant's own binding", gotA)
	}
}

func TestFilteredReads_NoTenantContext_FailClosed(t *testing.T) {
	// Every filtered read resolves the tenant before touching the
	// database, exactly as dbkit.Repository[T] does, and reports pkgcore's
	// own sentinel unmodified so a caller that already classifies it keeps
	// working. It must never fall back to an unfiltered query.
	db := newRBACTestDB(t)
	ctx := context.Background()

	if _, err := NewRoleRepository(db).ByKey(ctx, "admin"); !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Fatalf("RoleRepository.ByKey without a tenant = %v, want pkgcore.ErrNoTenant", err)
	}
	if _, err := NewRolePermissionRepository(db).ByRole(ctx, "r1"); !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Fatalf("RolePermissionRepository.ByRole without a tenant = %v, want pkgcore.ErrNoTenant", err)
	}
	if _, err := NewRoleBindingRepository(db).ByUser(ctx, "u1"); !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Fatalf("RoleBindingRepository.ByUser without a tenant = %v, want pkgcore.ErrNoTenant", err)
	}
	if _, err := NewRoleBindingRepository(db).ByRole(ctx, "r1"); !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Fatalf("RoleBindingRepository.ByRole without a tenant = %v, want pkgcore.ErrNoTenant", err)
	}
}

func TestFilteredReads_NoMatch_ReturnEmptyNotError(t *testing.T) {
	// "Grants nothing" is a normal answer, not a failure: a subject with no
	// bindings and a role with no permissions must both read as empty.
	db := newRBACTestDB(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	perms, err := NewRolePermissionRepository(db).ByRole(ctx, uuid.NewString())
	if err != nil {
		t.Fatalf("ByRole on an unknown role: %v", err)
	}
	if len(perms) != 0 {
		t.Fatalf("ByRole on an unknown role returned %+v, want no rows", perms)
	}

	bindings, err := NewRoleBindingRepository(db).ByUser(ctx, uuid.NewString())
	if err != nil {
		t.Fatalf("ByUser on an unknown user: %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("ByUser on an unknown user returned %+v, want no rows", bindings)
	}
}

// assertErrorCode fails the test unless err is an *apperr.Error carrying
// code. Errors here are matched on their Code, never by identity: every
// WithParam derives a new instance (apperr's own doc comment).
func assertErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error with code %q, got nil", code)
	}
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("error %v is not an *apperr.Error, want code %q", err, code)
	}
	if appErr.Code != code {
		t.Fatalf("error code = %q, want %q", appErr.Code, code)
	}
}
