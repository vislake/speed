package rbac

import (
	"context"
	"sort"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// Service is rbac's runtime: the authorization decision engine, plus the
// grant administration that feeds it. It is produced by Module.Attach --
// never constructed directly by a consumer -- because it only becomes
// correct once every module has declared its permissions, which is what
// Attach waits for.
//
// The decision path is deliberately short and has no branch on deployment
// mode anywhere in it (root CLAUDE.md's rule; the differences live in the
// EventBus and the *gorm.DB the host injected):
//
//	Subject -> bindings of that (tenant, user)
//	        -> permissions of those roles
//	        -> a flattened grant set, cached per subject
//	        -> exact match on "<resource>:<action>"
//
// and, for row filtering only, one further step through the host's
// SubtreeResolver to turn granted node ids into materialized paths.
//
// Every method is safe for concurrent use.
type Service struct {
	// catalog is the frozen snapshot of every permission every module
	// declared, taken in Attach. It is what makes a grant of a permission
	// nobody declared a rejected write rather than a silently dead row.
	catalog catalog

	// The three repositories are this module's only data access; see
	// repository.go for why each embeds dbkit.Repository[T].
	roles           *RoleRepository
	rolePermissions *RolePermissionRepository
	bindings        *RoleBindingRepository

	// subtree is the host's organization-tree seam, nil when none was
	// wired. Nil is a supported configuration: tenant-wide grants need no
	// resolver, and node-scoped ones then contribute nothing to a
	// DataScope rather than widening to the tenant.
	subtree SubtreeResolver

	// bus carries the invalidation and audit events. Writes publish on it;
	// Attach subscribes this Service's own handlers to it, so a replica
	// converges on another replica's revoke through the same code path a
	// local revoke takes.
	bus pkgcore.EventBus

	// cacheTTL is the decision cache's anti-loss expiry, kept here because
	// it is part of the Service's documented contract even though only the
	// cache reads it.
	cacheTTL time.Duration

	// cache holds flattened grant sets per subject. See cache.go for why
	// invalidation, not lookup speed, is what its design is about.
	cache *grantCache

	// now is the clock. It is a field so a test can pin the TTL boundary
	// without sleeping; production always gets time.Now.
	now func() time.Time

	// afterLoadGrants, when non-nil, runs synchronously inside grantsFor
	// right after loadGrants returns and before the result is written to
	// the cache. It exists solely so a test can inject a concurrent
	// invalidation into that exact window deterministically, rather than
	// relying on goroutine scheduling luck to reproduce the race the
	// generation fence in cache.go exists to close; production code never
	// sets it, and grantsFor is a no-op wrapper around it when it is nil.
	afterLoadGrants func()

	// beforeBindingCreate is afterLoadGrants's counterpart for AssignRole:
	// it runs synchronously right after AssignRole's existence check
	// (Find) reports the binding absent and right before its Create, so a
	// test can deterministically inject a second, concurrent identical
	// AssignRole into that exact window instead of relying on goroutine
	// scheduling to reproduce the race. Nil, and therefore a no-op, in
	// production.
	beforeBindingCreate func()

	// beforeBindingDelete is beforeBindingCreate's counterpart for
	// RevokeRole: it runs synchronously right after RevokeRole's Find
	// succeeds and right before its Delete, for the identical reason.
	beforeBindingDelete func()
}

// Close releases the Service's background resources: it stops the decision
// cache's janitor and waits for it to exit. It is idempotent.
//
// The Service stays usable afterwards, and stays CORRECT: without a
// janitor, expired entries are still refused by the cache's own expiry
// check, they are simply no longer reclaimed. A host that shuts rbac down
// while request traffic drains therefore does not start serving stale
// decisions at Close.
func (s *Service) Close() error {
	s.cache.close()
	return nil
}

// Can implements Authorizer.
func (s *Service) Can(ctx context.Context, sub Subject, action, resource string) (bool, error) {
	grants, err := s.grantsFor(ctx, sub)
	if err != nil {
		return false, err
	}
	// Deny by default, in the most literal sense available: the answer is
	// a map lookup on the permission, and every path that does not find
	// one returns false. An unknown permission is not special-cased --
	// nobody can hold what nobody declared, so it simply misses.
	_, ok := grants[Permission(resource, action)]
	return ok, nil
}

// ListPermissions implements Authorizer.
func (s *Service) ListPermissions(ctx context.Context, sub Subject) ([]string, error) {
	grants, err := s.grantsFor(ctx, sub)
	if err != nil {
		return nil, err
	}

	// A fresh slice every call: the cached grant map must stay immutable
	// (see grantEntry), and a caller that received the cache's own keys
	// could not be prevented from holding them past an invalidation.
	out := make([]string, 0, len(grants))
	for perm := range grants {
		out = append(out, perm)
	}
	sort.Strings(out)
	return out, nil
}

// DataScope implements Authorizer.
func (s *Service) DataScope(ctx context.Context, sub Subject, action, resource string) (DataScope, error) {
	grants, err := s.grantsFor(ctx, sub)
	if err != nil {
		return DataScope{Denied: true}, err
	}

	grant, ok := grants[Permission(resource, action)]
	if !ok {
		return DataScope{Denied: true}, nil
	}
	// A tenant-wide grant subsumes every node-scoped one, so the resolver
	// is not consulted at all: there is nothing a subtree could add to
	// "the whole tenant", and consulting it would put an avoidable
	// organization-tree read on the hot path of the common case.
	if grant.tenantWide {
		return DataScope{TenantWide: true}, nil
	}

	prefixes, err := s.resolvePrefixes(ctx, sub, grant.nodeIDs)
	if err != nil {
		return DataScope{Denied: true}, err
	}
	if len(prefixes) == 0 {
		// Every grant was node-scoped and none resolved: no resolver was
		// wired, or the nodes are gone. Deny -- a narrowing that cannot be
		// evaluated must never fail open into the tenant-wide grant it was
		// meant to restrict.
		return DataScope{Denied: true}, nil
	}
	return DataScope{SubtreePrefixes: prefixes}, nil
}

// resolvePrefixes turns granted node ids into materialized-path prefixes
// through the host's SubtreeResolver, dropping what cannot be resolved and
// propagating what failed.
//
// The three outcomes are distinct on purpose. A nil resolver means the host
// runs without an organization module, so a node-scoped grant is simply
// unevaluable and contributes nothing -- the host still works, it just
// cannot use node scope. A resolver that reports the node absent is the
// same case per node. A resolver that ERRORS is neither: the tree is
// momentarily unreachable, the correct answer is unknown, and returning a
// partial scope would silently narrow (or, if the failing node was the
// broadest, silently widen relative to the truth) so the error travels to
// the caller instead.
func (s *Service) resolvePrefixes(ctx context.Context, sub Subject, nodeIDs []string) ([]string, error) {
	if s.subtree == nil || len(nodeIDs) == 0 {
		return nil, nil
	}

	// The resolver is asked within the subject's own tenant, not whatever
	// tenant ctx happened to carry, matching the tenant every binding was
	// read under.
	resolveCtx := pkgcore.WithTenant(ctx, sub.TenantID)

	seen := make(map[string]struct{}, len(nodeIDs))
	prefixes := make([]string, 0, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		path, ok, err := s.subtree.NodePath(resolveCtx, nodeID)
		if err != nil {
			return nil, ErrSubtreeUnresolved.
				WithParam("node_id", nodeID).
				WithCause(err)
		}
		if !ok || path == "" {
			continue
		}
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}
		prefixes = append(prefixes, path)
	}
	sort.Strings(prefixes)
	return prefixes, nil
}

// grantsFor returns the subject's flattened grant set, from the cache when
// it holds a live entry and from the database otherwise.
//
// The returned map must be treated as read-only by every caller: it is the
// very map the cache stores, shared across goroutines without a lock held
// during evaluation (see grantEntry).
func (s *Service) grantsFor(ctx context.Context, sub Subject) (map[string]permissionGrant, error) {
	if !sub.Valid() {
		// An incomplete Subject is a caller bug, not a denial: something
		// upstream failed to establish identity. Reporting it as "no
		// permission" would hide that behind a plausible-looking 403 in
		// every log, so it is an error the caller must treat as a denial
		// (which the middleware does).
		return nil, ErrSubjectRequired
	}

	key := grantKey{tenant: sub.TenantID, user: sub.UserID}
	now := s.now()
	if grants, ok := s.cache.get(key, now); ok {
		return grants, nil
	}

	// The generation is captured BEFORE the database read starts, not
	// after. That ordering is what makes the fence below correct: if a
	// revoke's invalidate() (or a role change's invalidateTenant())
	// commits anywhere between this line and the eventual putIfCurrent,
	// the generation will have moved on and the stale load below is
	// discarded instead of cached -- see grantCache.putIfCurrent's doc
	// comment for the full race this closes.
	gen := s.cache.generation()
	grants, err := s.loadGrants(ctx, sub)
	if err != nil {
		return nil, err
	}
	if s.afterLoadGrants != nil {
		s.afterLoadGrants()
	}
	s.cache.putIfCurrent(key, grants, now, gen)
	return grants, nil
}

// loadGrants reads the subject's bindings and the permissions of the roles
// they name, and flattens them into one grant set.
//
// Isolation is structural rather than asserted here: every read runs under
// the SUBJECT's tenant, and both repositories are tenant-filtered, so a
// binding belonging to another tenant is not merely ignored, it is never
// returned. The subject's tenant is used in preference to whatever tenant
// ctx carried, because the Subject is the authorization identity the
// authenticating side vouched for -- an ambient tenant that disagreed with
// it must not be able to steer which tenant's grants are read.
func (s *Service) loadGrants(ctx context.Context, sub Subject) (map[string]permissionGrant, error) {
	readCtx := pkgcore.WithTenant(ctx, sub.TenantID)

	bindings, err := s.bindings.ByUser(readCtx, sub.UserID)
	if err != nil {
		return nil, err
	}
	if len(bindings) == 0 {
		// An empty (not nil) map, so a subject with no grants is cached as
		// "nothing" rather than re-read on every request.
		return map[string]permissionGrant{}, nil
	}

	// Two bindings can name the same role at different scopes, so the
	// role ids are de-duplicated for the permission read while the
	// scopes are kept per binding below.
	roleIDs := make([]string, 0, len(bindings))
	seenRole := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		if _, dup := seenRole[binding.RoleID]; dup {
			continue
		}
		seenRole[binding.RoleID] = struct{}{}
		roleIDs = append(roleIDs, binding.RoleID)
	}

	permissions, err := s.rolePermissions.ByRoles(readCtx, roleIDs)
	if err != nil {
		return nil, err
	}
	byRole := make(map[string][]string, len(roleIDs))
	for _, row := range permissions {
		byRole[row.RoleID] = append(byRole[row.RoleID], row.Permission)
	}

	grants := make(map[string]permissionGrant, len(permissions))
	for _, binding := range bindings {
		for _, perm := range byRole[binding.RoleID] {
			grant := grants[perm]
			if binding.IsTenantWide() {
				grant.tenantWide = true
			} else if !containsString(grant.nodeIDs, binding.NodeID) {
				grant.nodeIDs = append(grant.nodeIDs, binding.NodeID)
			}
			grants[perm] = grant
		}
	}
	return grants, nil
}

// containsString is the small membership test the node-id de-duplication
// above needs. A map would be asymptotically better and materially worse
// here: a subject holds a handful of bindings, and building a set per
// permission would allocate far more than it saves.
func containsString(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

// onRoleBindingChanged is the subscriber Attach installs for
// EventRoleBindingAssigned and EventRoleBindingRevoked. It drops the one
// subject's cached grants, wherever in the deployment the change happened.
//
// It never returns an error. On the in-memory bus this handler runs
// synchronously inside the publishing AssignRole/RevokeRole call, so a
// returned error would make a successful, already-committed write report
// failure -- the row is written before the event is published, and a
// caller that retried on that error would be retrying a change that
// already took effect. A payload of an unrecognized shape is dropped for
// the same reason; the TTL is what covers that case.
func (s *Service) onRoleBindingChanged(_ context.Context, evt pkgcore.Event) error {
	payload, ok := roleBindingChangedFromWire(evt.Payload)
	if !ok {
		return nil
	}
	s.cache.invalidate(grantKey{
		tenant: pkgcore.TenantID(payload.TenantID),
		user:   payload.UserID,
	})
	return nil
}

// onRoleChanged is the subscriber Attach installs for EventRoleChanged. A
// role's permission set changed, and the cache stores grants already
// flattened through their roles, so every subject in that tenant may be
// affected: the whole tenant's entries go. See
// grantCache.invalidateTenant.
func (s *Service) onRoleChanged(_ context.Context, evt pkgcore.Event) error {
	payload, ok := roleChangedFromWire(evt.Payload)
	if !ok {
		return nil
	}
	s.cache.invalidateTenant(pkgcore.TenantID(payload.TenantID))
	return nil
}

// actorFrom reports the acting subject's user id when the host installed
// one on ctx with WithSubject, and "" otherwise. See
// RoleBindingChangedEvent.ActorUserID for why this is best-effort.
func actorFrom(ctx context.Context) string {
	actor, ok := SubjectFromContext(ctx)
	if !ok {
		return ""
	}
	return actor.UserID
}
