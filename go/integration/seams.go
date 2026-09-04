package integration

import "context"

// PermissionLister lists every permission a subject currently holds, inside
// one tenant. Service.Create calls it exactly once per request, to validate
// that the Scopes a new key is being issued with are a genuine subset of
// its creator's own permissions right now
// (docs/internal/07-platform-services.md: "Key 的权限是创建者权限的子集").
//
// # Why this is a seam rather than an import of go/rbac
//
// go/rbac's Authorizer already exposes exactly this call --
// ListPermissions(ctx, sub Subject) ([]string, error), "every permission
// sub holds anywhere within its own tenant" -- which is the real,
// already-shipped API this interface mirrors structurally: same
// (ctx, tenant, user) -> ([]string, error) shape, with rbac.Subject's two
// string fields spread into plain parameters so this package needs no
// rbac.Subject value to call it. No new rbac method was invented for this
// round.
//
// This module still does not import go/rbac for it, for the same reason
// org.FeatureGate and rbac.SubtreeResolver are structurally-typed seams
// rather than direct imports of the module on the other side of the
// boundary: go/rbac carries dbkit, gorm and its own migrations along with
// it, and every one of those becomes a cost every consumer of this
// (otherwise storage-and-ratelimit-only) module would pay just to reference
// one interface's shape. A host that has rbac wires the real thing with a
// one-line closure (see WithPermissionLister); a host built around a
// different authorization system wires that instead, and this module never
// notices the difference. This mirrors the root CLAUDE.md's own module-
// boundary rule ("do not import another business module's structs") a
// half-step further: not just avoiding a struct, but avoiding the import
// edge entirely for a capability the caller can supply structurally.
//
// A host wires the real rbac.Service like this:
//
//	integration.WithPermissionLister(integration.PermissionListerFunc(
//	    func(ctx context.Context, tenantID, userID string) ([]string, error) {
//	        return rbacService.ListPermissions(ctx, rbac.Subject{
//	            TenantID: pkgcore.TenantID(tenantID),
//	            UserID:   userID,
//	        })
//	    },
//	))
//
// This seam is mandatory, not optional like MembershipChecker below: a key
// whose Scopes could not be validated against anything is a key issued with
// no security check at all, which Service.Create refuses rather than
// silently permits (ErrPermissionListerUnavailable) -- see Create's own doc
// comment for the exact rule, including the one case (an empty Scopes
// request) that needs no lister at all.
type PermissionLister interface {
	// ListPermissions returns every permission userID holds anywhere within
	// tenantID, sorted or not -- Service.Create only ever tests set
	// membership against the result, never its order.
	ListPermissions(ctx context.Context, tenantID, userID string) ([]string, error)
}

// PermissionListerFunc adapts a plain function to PermissionLister, the
// same func-to-interface adapter shape http.HandlerFunc popularized, so a
// host need not declare a named type just to satisfy this seam with a
// closure over its own rbac.Service.
type PermissionListerFunc func(ctx context.Context, tenantID, userID string) ([]string, error)

// ListPermissions implements PermissionLister.
func (f PermissionListerFunc) ListPermissions(ctx context.Context, tenantID, userID string) ([]string, error) {
	return f(ctx, tenantID, userID)
}

// MembershipChecker answers whether userID is still an active member of
// tenantID, the one fact Service.List needs to surface the design doc's
// "creator has left" flag ("Key 列表显示创建者已离职标记") on
// APIKeySummary.CreatorLeft.
//
// # Why this is a seam, and why it is optional where PermissionLister is not
//
// Membership is owned by whichever module actually tracks who belongs to a
// tenant -- go/org's roster in this codebase, but nothing requires that:
// docs/internal/07's own text only ever says "creator has left", never
// naming org or authn as the source of truth. go/integration must not
// import go/authn or go/org directly to find out (root CLAUDE.md's module-
// boundary rule -- a business module reaches a sibling by id and event,
// never by importing its structs), so, exactly like org.FeatureGate and
// rbac.SubtreeResolver, this is a structurally-typed interface a host
// implements with whatever module actually knows: a real deployment wires
// a closure over org's *org.MemberService (or, in a deployment with no org
// module at all, over authn's own membership table directly), and this
// module never learns which.
//
// Unlike PermissionLister, this seam is optional: it is a display
// convenience, not a security control -- nothing about whether a key
// authenticates, what it may do, or whether it keeps working depends on
// CreatorLeft's value (the design doc is explicit that a key must
// NOT stop working when its creator leaves). A host that wires none simply
// never sees the flag raised: List reports CreatorLeft as false for every
// row rather than refusing to list keys at all over a missing cosmetic
// seam. See WithMembershipChecker.
type MembershipChecker interface {
	// IsActiveMember reports whether userID currently holds an active
	// membership in tenantID.
	IsActiveMember(ctx context.Context, tenantID, userID string) (bool, error)
}

// MembershipCheckerFunc adapts a plain function to MembershipChecker, the
// same shape PermissionListerFunc gives PermissionLister.
type MembershipCheckerFunc func(ctx context.Context, tenantID, userID string) (bool, error)

// IsActiveMember implements MembershipChecker.
func (f MembershipCheckerFunc) IsActiveMember(ctx context.Context, tenantID, userID string) (bool, error) {
	return f(ctx, tenantID, userID)
}
