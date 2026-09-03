package rbac

import (
	"context"
	"errors"
	"sort"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
)

// RoleDefinition describes a role to create: its tenant-unique key, the
// i18n id its display name resolves through, and the permissions it
// grants.
//
// It is a struct rather than a parameter list so that the admin console's
// round can add fields (a colour, an ordering hint, a parent) without
// breaking every caller of DefineRole -- which, under this repository's
// lockstep versioning, means every delivered project at once.
type RoleDefinition struct {
	// Key identifies the role within its tenant and is what AssignRole and
	// RevokeRole name. It is a stable identifier, not display text.
	Key string

	// DescriptionKey is the i18n message id the role's human-readable name
	// and description resolve through. It is an id rather than text
	// because a role row must not carry user-facing prose in one language
	// (root CLAUDE.md's internationalization rule).
	DescriptionKey string

	// Permissions is the "<resource>:<action>" set the role grants. Every
	// entry must have been declared by some module during Register;
	// anything else is rejected with ErrUnknownPermission rather than
	// stored as a row that could never match.
	Permissions []string
}

// DefineRole creates a custom role inside the tenant ctx carries and
// returns it.
//
// The permission set is validated against the frozen catalog FIRST, so a
// typo ("notes:wirte") is a rejected write rather than a role that appears
// to grant something and silently grants nothing. That is the one place
// this module is strict about a permission string; at decision time an
// unknown permission simply denies, because a check must never turn a
// request into an error just because the caller asked about a name nobody
// declared.
//
// A key that already exists in the tenant returns ErrDuplicateRole. Roles
// are not updatable through this API in this milestone -- see the module's
// AGENTS.md deferral list for where role editing belongs -- so DefineRole
// is create-only and built-in roles are reconciled by EnsureBuiltinRoles
// instead.
func (s *Service) DefineRole(ctx context.Context, def RoleDefinition) (*Role, error) {
	return s.defineRole(ctx, def, false)
}

// defineRole is DefineRole's implementation, with the builtin flag the
// public API deliberately does not expose: what counts as a built-in role
// is this module's decision (builtin.go), not a caller's, because the
// flag is what protects those roles from being edited into a shape the
// next EnsureBuiltinRoles would silently revert.
func (s *Service) defineRole(ctx context.Context, def RoleDefinition, builtin bool) (*Role, error) {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}
	permissions, err := s.validatePermissions(def.Permissions)
	if err != nil {
		return nil, err
	}

	switch _, err := s.roles.ByKey(ctx, def.Key); {
	case err == nil:
		return nil, ErrDuplicateRole.WithParam("key", def.Key)
	case !isRoleNotFound(err):
		return nil, err
	}

	role := &Role{
		ID:             newID(),
		Key:            def.Key,
		Builtin:        builtin,
		DescriptionKey: def.DescriptionKey,
	}
	rows := make([]RolePermission, 0, len(permissions))
	for _, perm := range permissions {
		rows = append(rows, RolePermission{
			ID:         newID(),
			RoleID:     role.ID,
			Permission: perm,
		})
	}
	if err := s.roles.CreateWithPermissions(ctx, role, rows); err != nil {
		return nil, err
	}

	if err := s.publishRoleChanged(ctx, tenant, role, permissions); err != nil {
		return nil, err
	}
	return role, nil
}

// AssignRole implements Authorizer.
//
// The grant is written under the SUBJECT's tenant, not whatever tenant ctx
// carries: sub.TenantID is the tenant the grant applies in, and letting an
// ambient context steer that would make it possible to grant a role in one
// tenant while believing you granted it in another.
//
// Assigning a grant that already exists at the same scope is a no-op that
// returns nil and publishes nothing. Assignment is a widening operation, so
// "it is already there" fully satisfies the caller's intent, and a retry
// after a timeout must not fail on the unique index.
func (s *Service) AssignRole(ctx context.Context, sub Subject, role string, scope Scope) error {
	if !sub.Valid() {
		return ErrSubjectRequired
	}
	writeCtx := pkgcore.WithTenant(ctx, sub.TenantID)

	def, err := s.roles.ByKey(writeCtx, role)
	if err != nil {
		return err
	}

	switch _, err := s.bindings.Find(writeCtx, sub.UserID, def.ID, scope.NodeID); {
	case err == nil:
		return nil
	case !isBindingNotFound(err):
		return err
	}

	if s.beforeBindingCreate != nil {
		s.beforeBindingCreate()
	}

	binding := &RoleBinding{
		ID:     newID(),
		UserID: sub.UserID,
		RoleID: def.ID,
		NodeID: scope.NodeID,
	}
	if err := s.bindings.Create(writeCtx, binding); err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			// The Find above and this Create are not atomic, so two
			// concurrent identical AssignRole calls can both pass Find
			// before either has written its row. The loser's Create hits
			// uq_rbac_role_bindings_tenant_user_role_node, which dbkit.Open
			// wires gorm's TranslateError to report as this driver-agnostic
			// sentinel rather than a dialect-specific error. That unique
			// index firing means exactly one thing here: the grant this
			// call wanted is already there, which is a no-op per this
			// method's own documented contract, not a storage failure.
			return nil
		}
		return ErrStorage.WithCause(err)
	}
	return s.publishBindingChanged(ctx, EventRoleBindingAssigned, sub, def, scope)
}

// RevokeRole implements Authorizer.
//
// Unlike AssignRole it is STRICT: revoking a grant that is not there
// returns ErrBindingNotFound instead of succeeding quietly. The asymmetry
// is deliberate and is the safer half of the pair. Assignment that finds
// its work already done has achieved the caller's goal. Revocation that
// finds nothing to revoke usually has not -- the overwhelmingly common
// cause is a scope mismatch, an administrator revoking tenant-wide while
// the grant is node-scoped (or the reverse), and reporting success there
// would tell them access was withdrawn while the user still holds it.
// That is precisely the failure an access-control system exists to
// prevent, so it is reported. A caller retrying a revoke can treat
// ErrBindingNotFound as "already gone".
func (s *Service) RevokeRole(ctx context.Context, sub Subject, role string, scope Scope) error {
	if !sub.Valid() {
		return ErrSubjectRequired
	}
	writeCtx := pkgcore.WithTenant(ctx, sub.TenantID)

	def, err := s.roles.ByKey(writeCtx, role)
	if err != nil {
		return err
	}
	binding, err := s.bindings.Find(writeCtx, sub.UserID, def.ID, scope.NodeID)
	if err != nil {
		return err
	}
	if err := s.bindings.Delete(writeCtx, binding.ID); err != nil {
		return ErrStorage.WithCause(err)
	}
	return s.publishBindingChanged(ctx, EventRoleBindingRevoked, sub, def, scope)
}

// validatePermissions rejects anything the frozen catalog does not know,
// and returns the set sorted and de-duplicated so that two definitions
// listing the same permissions in different orders produce identical rows.
func (s *Service) validatePermissions(permissions []string) ([]string, error) {
	seen := make(map[string]struct{}, len(permissions))
	out := make([]string, 0, len(permissions))
	for _, perm := range permissions {
		if !s.catalog.Has(perm) {
			return nil, ErrUnknownPermission.WithParam("permission", perm)
		}
		if _, dup := seen[perm]; dup {
			continue
		}
		seen[perm] = struct{}{}
		out = append(out, perm)
	}
	sort.Strings(out)
	return out, nil
}

// publishBindingChanged invalidates this process's cache and announces the
// change on the bus.
//
// The local invalidation happens FIRST and unconditionally. The subscriber
// will invalidate again when the event comes back around (harmlessly --
// invalidation is idempotent), but the bus is allowed to fail, and this
// process must not keep answering from a grant set it just made obsolete
// even when the announcement to the other replicas does not get out.
//
// A publish failure is reported as ErrStorage wrapping the bus error
// rather than swallowed: the row is committed, but the other replicas have
// not been told, and the caller is the only one in a position to decide
// whether to retry or to accept up to one cache TTL of divergence.
func (s *Service) publishBindingChanged(ctx context.Context, eventType string, sub Subject, role *Role, scope Scope) error {
	s.cache.invalidate(grantKey{tenant: sub.TenantID, user: sub.UserID})

	evt := pkgcore.Event{
		Type:     eventType,
		TenantID: sub.TenantID,
		Payload: RoleBindingChangedEvent{
			TenantID:    string(sub.TenantID),
			UserID:      sub.UserID,
			RoleID:      role.ID,
			RoleKey:     role.Key,
			NodeID:      scope.NodeID,
			ActorUserID: actorFrom(ctx),
			ChangedAt:   s.now().UTC(),
		},
	}
	if err := s.bus.Publish(ctx, evt); err != nil {
		return ErrStorage.WithCause(err)
	}
	return nil
}

// publishRoleChanged is publishBindingChanged's counterpart for a role's
// own permission set; it invalidates the whole tenant for the reason
// grantCache.invalidateTenant documents.
func (s *Service) publishRoleChanged(ctx context.Context, tenant pkgcore.TenantID, role *Role, permissions []string) error {
	s.cache.invalidateTenant(tenant)

	evt := pkgcore.Event{
		Type:     EventRoleChanged,
		TenantID: tenant,
		Payload: RoleChangedEvent{
			TenantID:    string(tenant),
			RoleID:      role.ID,
			RoleKey:     role.Key,
			Permissions: permissions,
			ActorUserID: actorFrom(ctx),
			ChangedAt:   s.now().UTC(),
		},
	}
	if err := s.bus.Publish(ctx, evt); err != nil {
		return ErrStorage.WithCause(err)
	}
	return nil
}

// isRoleNotFound and isBindingNotFound classify the two "absent" answers
// the repositories give, by error CODE rather than by pointer identity:
// ByKey and Find decorate their sentinels with parameters, which produces
// a new error value, so errors.Is against the bare sentinel would not
// match.
func isRoleNotFound(err error) bool { return hasCode(err, ErrRoleNotFound.Code) }

func isBindingNotFound(err error) bool { return hasCode(err, ErrBindingNotFound.Code) }
