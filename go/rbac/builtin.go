package rbac

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"
)

// The built-in role keys every tenant gets. They are constants rather than
// free strings because org, authn and the admin console will all name them
// (a new member is bound to BuiltinRoleMember, a tenant's creator to
// BuiltinRoleOwner), and a typo in any of those would produce a role that
// exists but grants nothing.
const (
	// BuiltinRoleOwner is the tenant's full authority: it carries EVERY
	// permission any module declared. A tenant whose owner could not do
	// something would have no way to delegate it either, since delegation
	// itself is a permission.
	BuiltinRoleOwner = "owner"

	// BuiltinRoleAdmin carries every declared permission EXCEPT
	// PermissionManage. An administrator operates the tenant; deciding who
	// holds what authority stays with the owner. Without that one
	// exclusion the two roles would be identical, and an admin could grant
	// themselves anything an owner has, which makes the distinction
	// cosmetic rather than a boundary.
	BuiltinRoleAdmin = "admin"

	// BuiltinRoleMember carries NO permissions. It is the membership
	// marker -- "this person belongs to this tenant" -- and deliberately
	// not a bundle of guessed read access: which permissions an ordinary
	// member should hold is a product decision that differs per deployment,
	// and seeding a guess would hand out access nobody chose. Deny by
	// default applies to seeding too. A tenant that wants members to read
	// something defines a role for it.
	BuiltinRoleMember = "member"
)

// builtinRole is one built-in role's definition. Its permission set is a
// FUNCTION of the frozen catalog rather than a literal list, which is what
// lets rbac define an "owner" without naming a single other module's
// permissions -- naming billing:manage here would be exactly the
// cross-module coupling this module exists to avoid, and would go stale
// the moment a module was added or removed from the host's build.
type builtinRole struct {
	key            string
	descriptionKey string
	permissions    func(c catalog) []string
}

// builtinRoles is the ordered set EnsureBuiltinRoles seeds. Order is fixed
// so that a partially-completed seed resumes deterministically.
var builtinRoles = []builtinRole{
	{
		key:            BuiltinRoleOwner,
		descriptionKey: "rbac.role.owner",
		permissions:    func(c catalog) []string { return c.permissions() },
	},
	{
		key:            BuiltinRoleAdmin,
		descriptionKey: "rbac.role.admin",
		permissions: func(c catalog) []string {
			all := c.permissions()
			out := make([]string, 0, len(all))
			for _, perm := range all {
				if perm == PermissionManage {
					continue
				}
				out = append(out, perm)
			}
			return out
		},
	},
	{
		key:            BuiltinRoleMember,
		descriptionKey: "rbac.role.member",
		permissions:    func(catalog) []string { return nil },
	},
}

// EnsureBuiltinRoles seeds the built-in roles into the tenant ctx carries
// and reconciles the permission set of the ones already there.
//
// It is idempotent and meant to be run at every boot, and on tenant
// creation. Reconciling rather than only creating is what keeps a
// long-lived tenant correct: the owner role's permission set is defined as
// "everything declared", so adding a module to the host's build must widen
// it. Without reconciliation, every tenant created before that module
// existed would have an owner who could not use it, with nothing in the
// system reporting why.
//
// The seed runs entirely within one tenant: it reads the tenant's own
// roles and writes the tenant's own rows. There is deliberately no
// cross-tenant template table to copy from -- a template read would be the
// one place in this module where a query crossed a tenant boundary, and
// the definitions are cheap enough to recompute that buying that risk
// would be indefensible. The "system" pseudo-tenant is seeded by the same
// call under a system tenant context, with no special case anywhere.
//
// An event is published only for the roles that actually changed, so a
// no-op boot is silent on the bus.
func (s *Service) EnsureBuiltinRoles(ctx context.Context) error {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return err
	}

	for _, definition := range builtinRoles {
		want, err := s.validatePermissions(definition.permissions(s.catalog))
		if err != nil {
			// The catalog is where these came from, so this is
			// unreachable in practice; returning rather than ignoring
			// keeps it from becoming reachable silently if the
			// definitions above ever start naming a literal.
			return err
		}

		role, err := s.roles.ByKey(ctx, definition.key)
		switch {
		case isRoleNotFound(err):
			if _, defineErr := s.defineRole(ctx, RoleDefinition{
				Key:            definition.key,
				DescriptionKey: definition.descriptionKey,
				Permissions:    want,
			}, true); defineErr != nil {
				return defineErr
			}
			continue
		case err != nil:
			return err
		}

		if err := s.reconcileRolePermissions(ctx, tenant, role, want); err != nil {
			return err
		}
	}
	return nil
}

// reconcileRolePermissions rewrites role's permission rows to want when
// they differ, and does nothing at all when they already match.
//
// The comparison is what makes EnsureBuiltinRoles safe to run at every
// boot: without it, each boot would delete and re-insert every built-in
// role's permissions and publish a tenant-wide cache invalidation for each
// one, so a fleet restart would flush every replica's decision cache for
// no reason.
func (s *Service) reconcileRolePermissions(ctx context.Context, tenant pkgcore.TenantID, role *Role, want []string) error {
	current, err := s.rolePermissions.ByRole(ctx, role.ID)
	if err != nil {
		return err
	}
	have := make([]string, 0, len(current))
	for _, row := range current {
		have = append(have, row.Permission)
	}
	if sameStringSet(have, want) {
		return nil
	}

	rows := make([]RolePermission, 0, len(want))
	for _, perm := range want {
		rows = append(rows, RolePermission{
			ID:         newID(),
			RoleID:     role.ID,
			Permission: perm,
		})
	}
	if err := s.rolePermissions.ReplaceForRole(ctx, role.ID, rows); err != nil {
		return err
	}
	return s.publishRoleChanged(ctx, tenant, role, want)
}

// sameStringSet reports whether two permission lists grant the same thing.
// want arrives sorted and de-duplicated from validatePermissions; have
// comes from the database in no particular order, so membership is tested
// through a set rather than by comparing the slices element by element.
func sameStringSet(have, want []string) bool {
	if len(have) != len(want) {
		return false
	}
	set := make(map[string]struct{}, len(have))
	for _, item := range have {
		set[item] = struct{}{}
	}
	if len(set) != len(want) {
		return false
	}
	for _, item := range want {
		if _, ok := set[item]; !ok {
			return false
		}
	}
	return true
}
