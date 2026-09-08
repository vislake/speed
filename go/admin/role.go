package admin

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
)

// RoleService is the role-management runtime: a thin wrapper over
// rbac.Service's already-real role-management writes
// (DefineRole/AssignRole/RevokeRole/RestoreRole/EnsureBuiltinRoles) plus
// the DeclaredPermissions read, giving go/admin an HTTP surface for role
// management -- rbac itself mounts no HTTP routes by design: role
// management is the operations console's own surface, not rbac's.
//
// # Why this is NOT a WithXxx(*rbac.Module) construction-time option
//
// Every other downstream module admin depends on (authn, org, compliance,
// notification) is wired through a WithXxx(*Module) Option applied before
// Module.Register runs. rbac cannot be wired the same way: the
// *rbac.Service this wrapper needs to call does not exist until the HOST
// calls rbacModule.Attach(reg) -- which, by rbac's own documented
// contract, must run strictly AFTER pkgcore.Kernel.Bootstrap returns,
// because Attach freezes the snapshot of every permission every module
// declared. admin's own Module.Register runs DURING Bootstrap (before
// every module has necessarily finished registering), so it can never
// safely call rbacModule.Attach itself -- doing so would freeze the
// catalog before some other, later-registering module had its own turn
// to declare permissions, corrupting the catalog for the whole host.
//
// The wiring seam is therefore Module.AttachRBAC, a distinct, POST-
// BOOTSTRAP call the host makes once, immediately after its own
// rbacModule.Attach(registry) succeeds -- see that method's own doc
// comment. Until it is called, every method here fails closed with
// ErrRBACServiceRequired rather than a nil-service panic or a silently
// absent surface.
//
// # What this surface manages -- and what it never does
//
// RoleService manages role catalogs and bindings for the platform's real
// tenants. It deliberately does NOT administer rbac.SystemDomain, the
// pseudo-tenant every admin:* permission is evaluated in: the surface is
// gated on admin:roles_manage, and no permission is fine-grained enough to
// separate "may manage a customer tenant's roles" from "may delegate
// platform-operator authority", so the only safe default is refusing the
// system domain outright (checkTenantWritable, ErrRolesSystemDomainForbidden)
// -- the granting-side twin of ErrImpersonationTargetForbidden's
// subject-side refusal. Hosts seed and revoke system-domain grants out of
// band, directly against rbac.Service under a system-tenant context, as
// the reference app's seedDemoPlatformStaff does.
type RoleService struct {
	svc *rbac.Service
}

// NewRoleService returns a RoleService with no rbac.Service attached yet.
func NewRoleService() *RoleService { return &RoleService{} }

// attach gives the service the rbac.Service its methods delegate to. See
// Module.AttachRBAC's own doc comment for when the host must call this.
func (s *RoleService) attach(svc *rbac.Service) { s.svc = svc }

// require returns the attached rbac.Service, or ErrRBACServiceRequired
// when Module.AttachRBAC has not been called yet.
func (s *RoleService) require() (*rbac.Service, error) {
	if s.svc == nil {
		return nil, ErrRBACServiceRequired
	}
	return s.svc, nil
}

// checkTenantWritable is the up-front validation every tenant-naming write
// runs before rbac.Service is reached: an empty tenantID is refused with
// ErrTenantIDRequired, and rbac.SystemDomain -- the platform-operations
// pseudo-tenant every admin:* permission is evaluated in -- is refused
// with ErrRolesSystemDomainForbidden. The system tenant is the platform's
// internal domain, NOT a tenant any admin:roles_manage-gated surface may
// write roles or bindings into: the role catalog is single and global with
// no domain partitioning, so accepting it as an ordinary request tenant
// would let a roles_manage-only caller define a role carrying
// admin:impersonate (or any other admin:*) inside the system domain and
// bind it to themselves, collapsing the admin permission boundaries into
// one (the granting-side twin of ErrImpersonationTargetForbidden's own
// subject-side refusal -- see that sentinel's doc comment). Hosts seed
// and revoke system-domain grants out of band, directly against
// rbac.Service under a system-tenant context (the shape the reference
// app's seedDemoPlatformStaff takes), which this refusal leaves untouched.
func (s *RoleService) checkTenantWritable(tenantID string) error {
	if tenantID == "" {
		return ErrTenantIDRequired
	}
	if pkgcore.TenantID(tenantID) == rbac.SystemDomain {
		return ErrRolesSystemDomainForbidden
	}
	return nil
}

// DeclaredPermissions returns every permission any module declared --
// rbac.Service.DeclaredPermissions's frozen catalog snapshot -- the
// checklist a role-management UI renders when defining a new role. This
// answers a DIFFERENT question than any subject's own granted
// permissions (rbac.Service.ListPermissions, not exposed here): what the
// platform knows how to grant at all, not what one subject already holds.
func (s *RoleService) DeclaredPermissions() ([]string, error) {
	svc, err := s.require()
	if err != nil {
		return nil, err
	}
	return svc.DeclaredPermissions(), nil
}

// DefineRole creates or updates a role inside tenantID, delegating to
// rbac.Service.DefineRole. tenantID comes from the request body -- an
// admin:roles_manage-gated operator names which tenant's role catalog to
// edit -- rather than from a caller's own token, exactly like the
// impersonation pipeline's TargetTenantID: admin's own routes deliberately
// do not sit downstream of tenancy.Middleware, since these are platform
// operations ABOUT a tenant, not scoped to the caller's own one.
//
// tenantID must name a real tenant's catalog: rbac.SystemDomain is
// refused with ErrRolesSystemDomainForbidden (checkTenantWritable), the
// system tenant being the platform's internal domain, never a tenant an
// admin:roles_manage caller may write roles into -- see
// checkTenantWritable's own doc comment for the escalation this closes.
func (s *RoleService) DefineRole(ctx context.Context, tenantID string, def rbac.RoleDefinition) (*rbac.Role, error) {
	svc, err := s.require()
	if err != nil {
		return nil, err
	}
	if err := s.checkTenantWritable(tenantID); err != nil {
		return nil, err
	}
	return svc.DefineRole(pkgcore.WithTenant(ctx, pkgcore.TenantID(tenantID)), def)
}

// AssignRole binds role to (tenantID, userID) at the org node named by
// nodeID (empty means tenant-wide), delegating to rbac.Service.AssignRole.
// tenantID is refused when empty (ErrTenantIDRequired) or when it names
// rbac.SystemDomain (ErrRolesSystemDomainForbidden), by the identical
// checkTenantWritable guard DefineRole runs: binding a role inside the
// system domain is the bind half of the same escalation defining one is.
func (s *RoleService) AssignRole(ctx context.Context, tenantID, userID, role, nodeID string) error {
	svc, err := s.require()
	if err != nil {
		return err
	}
	if err := s.checkTenantWritable(tenantID); err != nil {
		return err
	}
	tenant := pkgcore.TenantID(tenantID)
	sub := rbac.Subject{TenantID: tenant, UserID: userID}
	return svc.AssignRole(pkgcore.WithTenant(ctx, tenant), sub, role, rbac.Scope{NodeID: nodeID})
}

// RevokeRole undoes a binding AssignRole created, delegating to
// rbac.Service.RevokeRole. tenantID is validated by the same
// checkTenantWritable guard every other write here runs -- empty is
// ErrTenantIDRequired, rbac.SystemDomain is ErrRolesSystemDomainForbidden.
// admin declares no admin.role.revoked audit action of its own
// (module.go's audit-action block): RevokeRole already publishes its own
// domain event, and that event carries the Actor this call's ctx
// supplies, never a second, redundant admin-owned record of the same
// fact.
func (s *RoleService) RevokeRole(ctx context.Context, tenantID, userID, role, nodeID string) error {
	svc, err := s.require()
	if err != nil {
		return err
	}
	if err := s.checkTenantWritable(tenantID); err != nil {
		return err
	}
	tenant := pkgcore.TenantID(tenantID)
	sub := rbac.Subject{TenantID: tenant, UserID: userID}
	return svc.RevokeRole(pkgcore.WithTenant(ctx, tenant), sub, role, rbac.Scope{NodeID: nodeID})
}

// RestoreRole undoes the most recently revoked matching binding,
// delegating to rbac.Service.RestoreRole -- wrapped here because it fits
// the identical shape as AssignRole/RevokeRole and rbac ships it as a
// real, tested method. tenantID is validated by the same
// checkTenantWritable guard every other write here runs, so even a
// Service-level caller cannot use this to re-land a system-domain binding
// the surface never created. No HTTP route exposes it -- the
// role-management fragment carries only role CRUD and binding routes -- so
// it is reachable at the Service level only, exactly like org's own
// TreeService.Restore.
func (s *RoleService) RestoreRole(ctx context.Context, tenantID, userID, role, nodeID string) error {
	svc, err := s.require()
	if err != nil {
		return err
	}
	if err := s.checkTenantWritable(tenantID); err != nil {
		return err
	}
	tenant := pkgcore.TenantID(tenantID)
	sub := rbac.Subject{TenantID: tenant, UserID: userID}
	return svc.RestoreRole(pkgcore.WithTenant(ctx, tenant), sub, role, rbac.Scope{NodeID: nodeID})
}

// EnsureBuiltinRoles delegates to rbac.Service.EnsureBuiltinRoles inside
// tenantID -- the operation an operator runs to materialize the
// platform's built-in roles for a tenant before assigning any of them.
// tenantID is validated by the same checkTenantWritable guard every
// other write here runs: a host that needs built-in roles inside
// rbac.SystemDomain (BuiltinRoleOwner carries every admin:* permission,
// so its system-domain materialization is bootstrap business, exactly
// like the reference app's seedDemoPlatformStaff) calls rbac.Service
// directly under a system-tenant context, never this wrapper.
// No HTTP route exposes this either, for the identical reason
// RestoreRole's own doc comment gives.
func (s *RoleService) EnsureBuiltinRoles(ctx context.Context, tenantID string) error {
	svc, err := s.require()
	if err != nil {
		return err
	}
	if err := s.checkTenantWritable(tenantID); err != nil {
		return err
	}
	return svc.EnsureBuiltinRoles(pkgcore.WithTenant(ctx, pkgcore.TenantID(tenantID)))
}
