package rbac

import (
	"context"
	"errors"
	"sort"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
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
	if s.beforeBindingDelete != nil {
		s.beforeBindingDelete()
	}
	if err := s.bindings.Delete(writeCtx, binding.ID); err != nil {
		if hasCode(err, dbkit.ErrRecordNotFound.Code) {
			// Find above and this Delete are not atomic either: two
			// concurrent RevokeRole calls for the same binding can both
			// pass Find, and the loser's Delete then finds zero rows
			// affected, which dbkit.Repository[T].Delete reports as
			// ErrRecordNotFound. That is the identical "nothing to revoke"
			// fact Find's own not-found path reports, so it is classified
			// the same way here rather than surfacing as an opaque storage
			// error -- a caller retrying a revoke under contention gets the
			// same ErrBindingNotFound either way.
			return ErrBindingNotFound.
				WithParam("role_id", def.ID).
				WithParam("node_id", scope.NodeID)
		}
		return ErrStorage.WithCause(err)
	}
	return s.publishBindingChanged(ctx, EventRoleBindingRevoked, sub, def, scope)
}

// RestoreRole undoes the mark-delete RevokeRole made to the binding that
// granted sub the role named by its key, at exactly scope -- the same three
// arguments RevokeRole itself takes, so a caller restores exactly what it
// just revoked without needing to have kept the binding's opaque id around.
//
// # Idempotent, like AssignRole, not strict, like RevokeRole
//
// A live binding already at this exact (tenant, user, role, node) tuple --
// most commonly because a fresh AssignRole landed at this scope some time
// after the revoke -- means the caller's desired end state, a live grant
// here, already holds. RestoreRole reports success without writing
// anything rather than attempting a write that would collide with
// uq_rbac_role_bindings_tenant_user_role_node (now partial,
// WHERE deleted_at IS NULL -- see
// migrations/{postgres,sqlite}/0002_add_soft_delete.sql). This mirrors
// AssignRole's own idempotent widening semantics, not RevokeRole's strict
// ones: RestoreRole's whole job is to make a grant exist, exactly like
// AssignRole's, so the asymmetry assign.go's own RevokeRole doc comment
// describes -- assignment that finds its work already done has achieved
// the caller's goal, while revocation that finds nothing usually has not --
// applies to restoration the identical way it applies to assignment.
//
// Otherwise it restores the most recently revoked binding at this tuple
// (RoleBindingRepository.findMostRecentlyRevoked -- more than one
// soft-deleted row can share a tuple once a revoke-then-reassign sequence
// has run more than once, and "restore exactly what I just revoked" means
// the newest one) and reports ErrBindingNotFound when there is no
// soft-deleted row to restore: the tuple was never granted at all, which is
// the same "nothing to restore" signal RevokeRole's own not-found path
// reports for "nothing to revoke".
//
// # Restoring onto a role or node that is now gone
//
// RestoreRole re-resolves the role by key through the same s.roles.ByKey
// call AssignRole and RevokeRole both make, so a role that somehow stopped
// existing by the time of the restore (rbac.Role has no delete path today,
// so this cannot happen through this module alone, but the check costs
// nothing and keeps the three methods' role resolution identical) reports
// ErrRoleNotFound rather than restoring a binding that names nothing.
//
// It does NOT check whether scope's node still exists in the organization
// tree, and that is a deliberate difference from go/org's
// TreeService.Restore, which refuses to restore a node onto a mark-deleted
// parent. The two situations are not the same hazard. org's tree is a
// STRUCTURE: a node whose parent is invisible corrupts every prefix-scan,
// ancestor walk and child-creation call that assumes Path agrees with
// ParentID (see go/org/AGENTS.md's "Soft deletion" section). A RoleBinding
// is a LEAF row that only feeds an authorization DECISION, and the decision
// path already tolerates a node id that resolves to nothing: scope.go's
// SubtreeResolver reports ok=false for a node it does not know, and
// Service.DataScope treats that as "this grant contributes nothing to
// scope" -- denying the narrowed row-level view rather than widening to the
// tenant -- exactly as it already does for a LIVE binding whose node was
// deleted out from under it after AssignRole ran (rbac has never verified
// node liveness at grant time; only DataScope resolution checks it, at
// decision time, every time). Restoring a binding whose node has since
// disappeared reintroduces no new failure mode: it is the identical
// dangling-node-reference situation an ordinary live binding can already be
// in, handled the identical fail-closed way. Refusing it here would need
// rbac to ask org whether a node is live, which the module boundary
// forbids outright (rbac never imports org; SubtreeResolver is the only
// fact it may learn, and only at decision time, never at grant or restore
// time). See go/rbac/AGENTS.md's "Soft deletion" section for the recorded
// decision and its justification in full.
func (s *Service) RestoreRole(ctx context.Context, sub Subject, role string, scope Scope) error {
	if !sub.Valid() {
		return ErrSubjectRequired
	}
	writeCtx := pkgcore.WithTenant(ctx, sub.TenantID)

	def, err := s.roles.ByKey(writeCtx, role)
	if err != nil {
		return err
	}

	switch _, findErr := s.bindings.Find(writeCtx, sub.UserID, def.ID, scope.NodeID); {
	case findErr == nil:
		// Already live at this exact scope: the desired end state already
		// holds, achieved by some other route since the revoke. See the
		// method doc comment for why this is a no-op rather than an error.
		return nil
	case !isBindingNotFound(findErr):
		return findErr
	}

	binding, err := s.bindings.findMostRecentlyRevoked(writeCtx, sub.UserID, def.ID, scope.NodeID)
	if err != nil {
		return err
	}

	if err := s.bindings.Restore(writeCtx, binding.ID); err != nil {
		if hasCode(err, dbkit.ErrRecordNotFound.Code) {
			// Lost a race: something else -- a concurrent RestoreRole for
			// the identical tuple, or a fresh AssignRole that landed
			// between the lookup above and this write -- changed the row's
			// state first. Classified the same way RevokeRole classifies
			// its own identical race, in assign.go's RevokeRole comment.
			return ErrBindingNotFound.
				WithParam("role_id", def.ID).
				WithParam("node_id", scope.NodeID)
		}
		return ErrStorage.WithCause(err)
	}
	return s.publishBindingChanged(ctx, EventRoleBindingRestored, sub, def, scope)
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
