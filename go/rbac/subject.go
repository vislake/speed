package rbac

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"
)

// SystemDomain is the pseudo-tenant that carries platform-operations
// grants: the "system" domain docs/internal/05-identity-and-access.md
// reserves for staff who act across the platform rather than inside one
// customer's tenant.
//
// It is deliberately an ordinary tenant id, not a special case. A grant to
// a platform operator is a row with tenant_id = 'system', stored through
// the same repositories, filtered by the same isolation plugin, covered by
// the same row-level security policy, and evaluated by the same code path
// as any customer tenant's rows. Nothing in this module branches on it --
// which is exactly what makes it trustworthy: there is no widened path
// that could be reached by accident.
//
// It is NOT a wildcard. A subject in the system domain is granted whatever
// its system-domain bindings grant it and nothing else; it does not gain
// access to any customer tenant's data by holding it.
const SystemDomain pkgcore.TenantID = "system"

// Subject is who authorization is being decided for: a user, inside a
// tenant. It is the entire vocabulary this module has for identity.
//
// This type is the reason rbac never imports authn (root CLAUDE.md's
// module-boundary rule, restated first in this module's AGENTS.md).
// Whoever authenticates a request -- authn in production, a demo
// middleware in the reference app, a test in this package -- assembles a
// Subject from the access token's claims and hands it in. rbac never
// learns what a user record looks like, never reads a session, and has no
// opinion on how the caller proved the identity.
type Subject struct {
	// TenantID is the tenant the subject is acting inside. It always comes
	// from the access token's claims, never from a request parameter,
	// header or body (root CLAUDE.md's multi-tenant isolation rule).
	TenantID pkgcore.TenantID

	// UserID is the acting user's id in authn -- an id reference, carried
	// as a plain string, never a struct borrowed from another module.
	UserID string
}

// Valid reports whether s names both a tenant and a user. An incomplete
// Subject can never be granted anything: authorization fails closed rather
// than treating a missing half as a wildcard.
func (s Subject) Valid() bool {
	return s.TenantID != "" && s.UserID != ""
}

// Scope is where a grant applies inside a tenant's organization tree.
//
// The zero Scope means the tenant root -- the grant covers the whole
// tenant. A Scope naming a node covers that node and everything beneath
// it, resolved through the host's SubtreeResolver at evaluation time (see
// scope.go); it is a narrowing, never a widening, so a scope whose node
// cannot be resolved denies rather than falling back to tenant-wide.
type Scope struct {
	// NodeID is an organization node's id in org -- an id reference only,
	// no import and no foreign key. Empty means the tenant root.
	NodeID string
}

// IsTenantWide reports whether s covers the whole tenant rather than one
// subtree of it.
func (s Scope) IsTenantWide() bool { return s.NodeID == "" }

// subjectContextKey is the unexported key type Subject values are stored
// under in a context. An unexported struct type as the key is the standard
// way to make the entry unreachable from outside this package, so no other
// package can plant or overwrite a Subject.
type subjectContextKey struct{}

// WithSubject returns a copy of ctx carrying sub, for the authenticating
// side to install once per request. The middleware in this module reads it
// back with SubjectFromContext.
//
// It stores sub as given, including an incomplete one: rejecting it here
// would move a security decision into a context helper, where a caller
// that ignored the error would silently proceed unauthenticated.
// SubjectFromContext is the single place that decides what counts as a
// usable subject, and it fails closed.
func WithSubject(ctx context.Context, sub Subject) context.Context {
	return context.WithValue(ctx, subjectContextKey{}, sub)
}

// SubjectFromContext returns the Subject the authenticating side installed
// on ctx.
//
// ok is false when ctx carries no Subject at all AND when it carries one
// that is not Valid -- an incomplete subject is reported as no subject, so
// every caller's "no subject, deny" branch covers both cases and no caller
// has to remember to re-check Valid itself. That is the fail-closed
// direction: the alternative would hand out a half-empty Subject that
// compares equal to nothing and grants nothing, but looks present.
func SubjectFromContext(ctx context.Context) (Subject, bool) {
	sub, ok := ctx.Value(subjectContextKey{}).(Subject)
	if !ok || !sub.Valid() {
		return Subject{}, false
	}
	return sub, true
}
