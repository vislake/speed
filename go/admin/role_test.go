package admin

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
)

// TestRoleService_BeforeAttachRBAC_FailsClosed pins D8's fail-closed
// contract: every method refuses with ErrRBACServiceRequired, never a
// nil-service panic, until Module.AttachRBAC has been called.
func TestRoleService_BeforeAttachRBAC_FailsClosed(t *testing.T) {
	svc := NewRoleService()

	if _, err := svc.DeclaredPermissions(); !isCode(err, ErrRBACServiceRequired.Code) {
		t.Errorf("DeclaredPermissions() error = %v, want %s", err, ErrRBACServiceRequired.Code)
	}
	if _, err := svc.DefineRole(context.Background(), "tenant-a", rbac.RoleDefinition{Key: "custom"}); !isCode(err, ErrRBACServiceRequired.Code) {
		t.Errorf("DefineRole() error = %v, want %s", err, ErrRBACServiceRequired.Code)
	}
	if err := svc.AssignRole(context.Background(), "tenant-a", "user-1", "custom", ""); !isCode(err, ErrRBACServiceRequired.Code) {
		t.Errorf("AssignRole() error = %v, want %s", err, ErrRBACServiceRequired.Code)
	}
	if err := svc.RevokeRole(context.Background(), "tenant-a", "user-1", "custom", ""); !isCode(err, ErrRBACServiceRequired.Code) {
		t.Errorf("RevokeRole() error = %v, want %s", err, ErrRBACServiceRequired.Code)
	}
	if err := svc.RestoreRole(context.Background(), "tenant-a", "user-1", "custom", ""); !isCode(err, ErrRBACServiceRequired.Code) {
		t.Errorf("RestoreRole() error = %v, want %s", err, ErrRBACServiceRequired.Code)
	}
	if err := svc.EnsureBuiltinRoles(context.Background(), "tenant-a"); !isCode(err, ErrRBACServiceRequired.Code) {
		t.Errorf("EnsureBuiltinRoles() error = %v, want %s", err, ErrRBACServiceRequired.Code)
	}
}

// TestRoleService_DeclaredPermissions_IsTheFrozenCatalog proves D8's
// checklist read against a REAL, Attach()-ed rbac.Service: the catalog
// includes admin's own newly-declared permissions (PermissionRolesManage
// among them) alongside rbac's and every other module's, since it is one
// shared, frozen snapshot -- not a per-module view.
func TestRoleService_DeclaredPermissions_IsTheFrozenCatalog(t *testing.T) {
	env := buildTestAdminModule(t)
	env.Admin.AttachRBAC(env.RBAC)

	perms, err := env.Admin.Roles().DeclaredPermissions()
	if err != nil {
		t.Fatalf("DeclaredPermissions() error = %v", err)
	}

	want := PermissionRolesManage
	found := false
	for _, p := range perms {
		if p == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("DeclaredPermissions() = %v, want it to include %q", perms, want)
	}
}

// TestRoleService_DefineRole_EmptyTenantID_Refused pins the up-front
// tenantID validation: an empty tenantID is refused with the coded
// ErrTenantIDRequired BEFORE rbac.Service.DefineRole is ever reached
// (which would otherwise surface only the raw, uncoded pkgcore.ErrNoTenant).
func TestRoleService_DefineRole_EmptyTenantID_Refused(t *testing.T) {
	env := buildTestAdminModule(t)
	env.Admin.AttachRBAC(env.RBAC)

	_, err := env.Admin.Roles().DefineRole(context.Background(), "", rbac.RoleDefinition{Key: "custom", Permissions: []string{PermissionAccess}})
	if !isCode(err, ErrTenantIDRequired.Code) {
		t.Fatalf("DefineRole() with empty tenantID error = %v, want %s", err, ErrTenantIDRequired.Code)
	}
}

// TestRoleService_DefineAssignRevokeRestore_EndToEnd is D8's full-lifecycle
// proof, driven entirely through RoleService against a REAL, Attach()-ed
// rbac.Service (no mocks, no fakes): define a custom role scoped to a
// real tenant, assign it to a real user, confirm the grant took effect
// through rbac's own Can, revoke it, confirm it no longer applies, then
// restore it and confirm it applies again.
func TestRoleService_DefineAssignRevokeRestore_EndToEnd(t *testing.T) {
	env := buildTestAdminModule(t)
	env.Admin.AttachRBAC(env.RBAC)
	roles := env.Admin.Roles()

	const tenant = "tenant-role-flow"
	const user = "user-role-flow"

	role, err := roles.DefineRole(context.Background(), tenant, rbac.RoleDefinition{
		Key:         "flow-role",
		Permissions: []string{PermissionAccess},
	})
	if err != nil {
		t.Fatalf("DefineRole() error = %v", err)
	}
	if role.Key != "flow-role" || role.TenantID != tenant {
		t.Fatalf("DefineRole() = %+v, want Key=flow-role TenantID=%s", role, tenant)
	}

	sub := rbac.Subject{TenantID: pkgcore.TenantID(tenant), UserID: user}
	canBeforeAssign, err := env.RBAC.Can(context.Background(), sub, "access", "admin")
	if err != nil {
		t.Fatalf("Can() before assign error = %v", err)
	}
	if canBeforeAssign {
		t.Fatal("Can() before AssignRole = true, want false")
	}

	if assignErr := roles.AssignRole(context.Background(), tenant, user, "flow-role", ""); assignErr != nil {
		t.Fatalf("AssignRole() error = %v", assignErr)
	}
	canAfterAssign, err := env.RBAC.Can(context.Background(), sub, "access", "admin")
	if err != nil {
		t.Fatalf("Can() after assign error = %v", err)
	}
	if !canAfterAssign {
		t.Fatal("Can() after AssignRole = false, want true")
	}

	if revokeErr := roles.RevokeRole(context.Background(), tenant, user, "flow-role", ""); revokeErr != nil {
		t.Fatalf("RevokeRole() error = %v", revokeErr)
	}
	canAfterRevoke, err := env.RBAC.Can(context.Background(), sub, "access", "admin")
	if err != nil {
		t.Fatalf("Can() after revoke error = %v", err)
	}
	if canAfterRevoke {
		t.Fatal("Can() after RevokeRole = true, want false")
	}

	if restoreErr := roles.RestoreRole(context.Background(), tenant, user, "flow-role", ""); restoreErr != nil {
		t.Fatalf("RestoreRole() error = %v", restoreErr)
	}
	canAfterRestore, err := env.RBAC.Can(context.Background(), sub, "access", "admin")
	if err != nil {
		t.Fatalf("Can() after restore error = %v", err)
	}
	if !canAfterRestore {
		t.Fatal("Can() after RestoreRole = false, want true")
	}
}

// TestRoleService_EnsureBuiltinRoles_MaterializesBuiltinRoles proves the
// wrapped EnsureBuiltinRoles reaches the real rbac.Service: after calling
// it for a fresh tenant, that tenant holds rbac's own built-in Owner role
// ready to assign.
func TestRoleService_EnsureBuiltinRoles_MaterializesBuiltinRoles(t *testing.T) {
	env := buildTestAdminModule(t)
	env.Admin.AttachRBAC(env.RBAC)

	const tenant = "tenant-builtin-flow"
	if err := env.Admin.Roles().EnsureBuiltinRoles(context.Background(), tenant); err != nil {
		t.Fatalf("EnsureBuiltinRoles() error = %v", err)
	}

	if err := env.Admin.Roles().AssignRole(context.Background(), tenant, "user-builtin-flow", rbac.BuiltinRoleOwner, ""); err != nil {
		t.Fatalf("AssignRole(BuiltinRoleOwner) after EnsureBuiltinRoles error = %v", err)
	}
}
