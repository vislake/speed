package admin

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
)

// RoleService is D8's runtime: a thin wrapper over rbac.Service's
// already-real role-management writes (DefineRole/AssignRole/RevokeRole/
// RestoreRole/EnsureBuiltinRoles) plus the new DeclaredPermissions read,
// giving go/admin an HTTP surface for role management -- rbac itself
// mounts no HTTP routes by design (go/rbac/AGENTS.md: role management is
// the operations console's own surface, not rbac's).
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
// edit -- rather than from a caller's own token, exactly like D5's
// impersonation TargetTenantID: admin's own routes deliberately do not
// sit downstream of tenancy.Middleware (AGENTS.md's HTTP surface
// section), since these are platform operations ABOUT a tenant, not
// scoped to the caller's own one.
func (s *RoleService) DefineRole(ctx context.Context, tenantID string, def rbac.RoleDefinition) (*rbac.Role, error) {
	svc, err := s.require()
	if err != nil {
		return nil, err
	}
	if tenantID == "" {
		return nil, ErrTenantIDRequired
	}
	return svc.DefineRole(pkgcore.WithTenant(ctx, pkgcore.TenantID(tenantID)), def)
}

// AssignRole binds role to (tenantID, userID) at the org node named by
// nodeID (empty means tenant-wide), delegating to rbac.Service.AssignRole.
func (s *RoleService) AssignRole(ctx context.Context, tenantID, userID, role, nodeID string) error {
	svc, err := s.require()
	if err != nil {
		return err
	}
	if tenantID == "" {
		return ErrTenantIDRequired
	}
	tenant := pkgcore.TenantID(tenantID)
	sub := rbac.Subject{TenantID: tenant, UserID: userID}
	return svc.AssignRole(pkgcore.WithTenant(ctx, tenant), sub, role, rbac.Scope{NodeID: nodeID})
}

// RevokeRole undoes a binding AssignRole created, delegating to
// rbac.Service.RevokeRole. admin declares no admin.role.revoked audit
// action of its own -- RevokeRole already publishes its own domain event
// (go/admin/AGENTS.md's round-1 note, carried forward unchanged this
// round), so this call site is exactly where that division of
// responsibility is honored: rbac's own event carries the Actor this
// call's ctx supplies, never a second, redundant admin-owned record of
// the same fact.
func (s *RoleService) RevokeRole(ctx context.Context, tenantID, userID, role, nodeID string) error {
	svc, err := s.require()
	if err != nil {
		return err
	}
	if tenantID == "" {
		return ErrTenantIDRequired
	}
	tenant := pkgcore.TenantID(tenantID)
	sub := rbac.Subject{TenantID: tenant, UserID: userID}
	return svc.RevokeRole(pkgcore.WithTenant(ctx, tenant), sub, role, rbac.Scope{NodeID: nodeID})
}

// RestoreRole undoes the most recently revoked matching binding,
// delegating to rbac.Service.RestoreRole -- wrapped here because it fits
// the identical shape as AssignRole/RevokeRole and rbac's own soft-delete
// round already ships it as a real, tested method. admin exposes no HTTP
// route for it this round (docs/internal/23-admin.md's D8 HTTP table
// names only three routes), so it is reachable at the Service level
// only, exactly like org's own TreeService.Restore.
func (s *RoleService) RestoreRole(ctx context.Context, tenantID, userID, role, nodeID string) error {
	svc, err := s.require()
	if err != nil {
		return err
	}
	if tenantID == "" {
		return ErrTenantIDRequired
	}
	tenant := pkgcore.TenantID(tenantID)
	sub := rbac.Subject{TenantID: tenant, UserID: userID}
	return svc.RestoreRole(pkgcore.WithTenant(ctx, tenant), sub, role, rbac.Scope{NodeID: nodeID})
}

// EnsureBuiltinRoles delegates to rbac.Service.EnsureBuiltinRoles inside
// tenantID -- the operation an operator runs to materialize the
// platform's built-in roles for a tenant before assigning any of them.
// No HTTP route exposes this either, for the identical reason
// RestoreRole's own doc comment gives.
func (s *RoleService) EnsureBuiltinRoles(ctx context.Context, tenantID string) error {
	svc, err := s.require()
	if err != nil {
		return err
	}
	if tenantID == "" {
		return ErrTenantIDRequired
	}
	return svc.EnsureBuiltinRoles(pkgcore.WithTenant(ctx, pkgcore.TenantID(tenantID)))
}
