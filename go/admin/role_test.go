package admin

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
)

// rolesSystemDomainForbiddenCode is the wire code ("<module>.<reason>")
// every RoleService tenant-naming write must answer with when the request
// names rbac.SystemDomain as the tenant to write into -- the P1-2 closure
// this file's escalation test below pins. It is deliberately a literal,
// not the sentinel's own Code: the regression must compile -- and fail at
// runtime with the escalation succeeding -- against the UNFIXED code,
// which has no sentinel to reference yet. The parity between this literal
// and the real sentinel's Code is asserted at the top of
// TestRoleService_SystemDomain_RolesManageOnlyCaller_CannotEscalate so the
// two cannot silently drift apart.
const rolesSystemDomainForbiddenCode = "admin.roles_system_domain_forbidden"

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

// seedRolesManageOnlyCaller grants callerID exactly one permission under
// rbac.SystemDomain -- admin:roles_manage -- the way a host bootstrap seeds
// a limited role-manager operator: directly against rbac.Service under a
// system-tenant context, never through RoleService. That direct path is the
// out-of-band route every system-domain write must take once RoleService
// refuses the pseudo-tenant (RoleService's own doc comment); it stays open
// precisely so hosts can still seed and revoke platform-operator grants at
// bootstrap.
func seedRolesManageOnlyCaller(t *testing.T, env testAdminEnv, callerID string) {
	t.Helper()
	systemCtx := pkgcore.WithTenant(context.Background(), rbac.SystemDomain)
	if _, err := env.RBAC.DefineRole(systemCtx, rbac.RoleDefinition{
		Key:         "seed-role-manager",
		Permissions: []string{PermissionRolesManage},
	}); err != nil {
		t.Fatalf("seed the role-manager role in the system domain: %v", err)
	}
	if err := env.RBAC.AssignRole(systemCtx, rbac.Subject{TenantID: rbac.SystemDomain, UserID: callerID}, "seed-role-manager", rbac.Scope{}); err != nil {
		t.Fatalf("seed the role-manager binding: %v", err)
	}
}

// TestRoleService_SystemDomain_RolesManageOnlyCaller_CannotEscalate pins
// the closure of the roles/define/bind escalation the P1-2 finding names: a
// caller holding ONLY admin:roles_manage under rbac.SystemDomain must not
// be able to obtain any other admin:* permission through the role surface
// by naming the system pseudo-tenant as the tenant to write into. Before
// the fix every write below succeeded: DefineRole/AssignRole took the
// tenant from the request and rejected only the empty string, so a
// roles_manage-only caller could define a role carrying admin:impersonate
// in the system domain and bind it to themselves -- collapsing the nine
// admin permission boundaries D1 draws into one (the role catalog is
// single and global, admin's own admin:* permissions included). After the
// fix every RoleService tenant-naming write refuses rbac.SystemDomain with
// ErrRolesSystemDomainForbidden, and no grant ever takes effect (Can
// answers false throughout).
func TestRoleService_SystemDomain_RolesManageOnlyCaller_CannotEscalate(t *testing.T) {
	if got := ErrRolesSystemDomainForbidden.Code; got != rolesSystemDomainForbiddenCode {
		t.Fatalf("ErrRolesSystemDomainForbidden.Code = %q, want the pinned wire code %q", got, rolesSystemDomainForbiddenCode)
	}
	env := buildTestAdminModule(t)
	env.Admin.AttachRBAC(env.RBAC)
	roles := env.Admin.Roles()

	const caller = "user-role-manager-escalator"
	seedRolesManageOnlyCaller(t, env, caller)
	ctx := rbac.WithSubject(context.Background(), rbac.Subject{TenantID: rbac.SystemDomain, UserID: caller})
	sub := rbac.Subject{TenantID: rbac.SystemDomain, UserID: caller}
	canImpersonate := func() bool {
		t.Helper()
		can, err := env.RBAC.Can(context.Background(), sub, "impersonate", "admin")
		if err != nil {
			t.Fatalf("Can(admin:impersonate) error = %v", err)
		}
		return can
	}

	// The define half of the escalation: a role carrying admin:impersonate
	// inside the system domain. Refused, and nothing is granted.
	_, err := roles.DefineRole(ctx, string(rbac.SystemDomain), rbac.RoleDefinition{
		Key:         "self-escalation",
		Permissions: []string{PermissionImpersonate},
	})
	if !isCode(err, rolesSystemDomainForbiddenCode) {
		t.Fatalf("DefineRole(system domain, admin:impersonate) error = %v, want %s", err, rolesSystemDomainForbiddenCode)
	}
	if canImpersonate() {
		t.Fatal("caller can impersonate after a refused system-domain define, want false")
	}

	// The bind half of the escalation must be refused even when the
	// powerful role ALREADY exists in the system domain, seeded out of band
	// by a full operator holding the permission it grants (the shape the
	// reference app's platform-staff carve-outs take): binding it -- to
	// themselves or to anyone -- collapses the same boundary defining it
	// does.
	if _, err := env.RBAC.DefineRole(pkgcore.WithTenant(context.Background(), rbac.SystemDomain), rbac.RoleDefinition{
		Key:         "staff-carved-impersonator",
		Permissions: []string{PermissionImpersonate},
	}); err != nil {
		t.Fatalf("seed a staff-carved system-domain role: %v", err)
	}
	if err := roles.AssignRole(ctx, string(rbac.SystemDomain), caller, "staff-carved-impersonator", ""); !isCode(err, rolesSystemDomainForbiddenCode) {
		t.Fatalf("AssignRole(system domain) error = %v, want %s", err, rolesSystemDomainForbiddenCode)
	}
	if canImpersonate() {
		t.Fatal("caller can impersonate after a refused system-domain bind, want false")
	}

	// Revoke, restore and ensure-builtin are tenant-naming writes into the
	// same internal domain and refused the same way: a surface any
	// admin:roles_manage holder can reach must not touch the system
	// domain's bindings at all -- not only refrain from adding to them.
	if err := roles.RevokeRole(ctx, string(rbac.SystemDomain), caller, "seed-role-manager", ""); !isCode(err, rolesSystemDomainForbiddenCode) {
		t.Fatalf("RevokeRole(system domain) error = %v, want %s", err, rolesSystemDomainForbiddenCode)
	}
	if err := roles.RestoreRole(ctx, string(rbac.SystemDomain), caller, "seed-role-manager", ""); !isCode(err, rolesSystemDomainForbiddenCode) {
		t.Fatalf("RestoreRole(system domain) error = %v, want %s", err, rolesSystemDomainForbiddenCode)
	}
	if err := roles.EnsureBuiltinRoles(ctx, string(rbac.SystemDomain)); !isCode(err, rolesSystemDomainForbiddenCode) {
		t.Fatalf("EnsureBuiltinRoles(system domain) error = %v, want %s", err, rolesSystemDomainForbiddenCode)
	}
}

// TestRoleService_RealTenant_RolesManageOnlyCaller_ManagesRolesStill is
// the refusal's positive half: the SAME roles_manage-only caller can still
// run the whole define/assign/revoke lifecycle against an ordinary tenant
// id, where the role surface's job genuinely lies. The P1-2 closure is
// scoped to the system pseudo-tenant alone -- the platform's internal
// domain, not a tenant any admin:roles_manage holder may write roles into
// -- so role management for a real tenant is unchanged.
func TestRoleService_RealTenant_RolesManageOnlyCaller_ManagesRolesStill(t *testing.T) {
	env := buildTestAdminModule(t)
	env.Admin.AttachRBAC(env.RBAC)
	roles := env.Admin.Roles()

	const caller = "user-role-manager-tenant"
	const tenant = "tenant-role-managed"
	seedRolesManageOnlyCaller(t, env, caller)
	ctx := rbac.WithSubject(context.Background(), rbac.Subject{TenantID: rbac.SystemDomain, UserID: caller})

	const managedUser = "user-managed-in-tenant"
	sub := rbac.Subject{TenantID: pkgcore.TenantID(tenant), UserID: managedUser}
	canRead := func() bool {
		t.Helper()
		can, err := env.RBAC.Can(context.Background(), sub, "read", "rbac")
		if err != nil {
			t.Fatalf("Can(rbac:read) error = %v", err)
		}
		return can
	}

	if _, err := roles.DefineRole(ctx, tenant, rbac.RoleDefinition{
		Key:         "tenant-custom",
		Permissions: []string{rbac.PermissionRead},
	}); err != nil {
		t.Fatalf("DefineRole(real tenant) error = %v", err)
	}
	if err := roles.AssignRole(ctx, tenant, managedUser, "tenant-custom", ""); err != nil {
		t.Fatalf("AssignRole(real tenant) error = %v", err)
	}
	if !canRead() {
		t.Fatal("Can(rbac:read) after binding in the real tenant = false, want true")
	}
	if err := roles.RevokeRole(ctx, tenant, managedUser, "tenant-custom", ""); err != nil {
		t.Fatalf("RevokeRole(real tenant) error = %v", err)
	}
	if canRead() {
		t.Fatal("Can(rbac:read) after revoke in the real tenant = true, want false")
	}
}
