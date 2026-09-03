package rbac

import (
	"context"
	"strings"
)

// SubtreeResolver maps an organization-tree node id to its materialized
// path. It is the ONE fact rbac needs about the organization tree, and it
// is an interface declared here -- implemented by the host, which is the
// org module once that exists -- so that rbac never imports another
// business module's structs (root CLAUDE.md, "Do not import another
// business module's structs for database relations ... use ID references
// plus domain events").
//
// Together with the Subject the authenticating side assembles, this is the
// second of the module's two no-import seams: rbac learns who is asking
// and where a node sits, and nothing else about either neighbour.
//
// The resolver is consulted at evaluation time, on every decision that
// involves a node-scoped binding. It is deliberately not consulted at
// grant time and its answer is never stored on the binding row: a
// materialized path denormalized onto a binding goes stale the moment the
// node moves in the tree, and docs/internal/16-verification.md requires a
// member's permissions to follow such a move immediately.
type SubtreeResolver interface {
	// NodePath returns the materialized path of nodeID within the tenant
	// ctx carries, for example "/group1/region2/store7".
	//
	// ok reports whether the node exists in that tenant. A false ok makes
	// rbac DENY the binding that named the node -- never widen it to the
	// tenant -- because a narrowing that cannot be resolved must not fail
	// open into the broader grant it was meant to restrict. An error is
	// propagated to the caller rather than swallowed, so a resolver that
	// is merely unavailable is distinguishable from one that answered
	// "no such node".
	NodePath(ctx context.Context, nodeID string) (path string, ok bool, err error)
}

// DataScope answers "over which slice of the organization tree does this
// subject hold this permission", which is a different question from Can's
// "does this subject hold it at all". A row-level filter needs this one:
// Can is the coarse gate that decides whether a request may proceed,
// DataScope is what decides which rows it may see.
//
// The two can legitimately disagree. A subject whose only grant is a
// node-scoped binding the host cannot resolve (no SubtreeResolver wired,
// or the node is gone) gets Can == true and a Denied DataScope: the grant
// exists, but there is no known part of the tree it covers. Denied is
// therefore the fail-closed answer, never a widening to the tenant.
type DataScope struct {
	// Denied reports that the subject holds the permission nowhere the
	// caller may act on. It is true both when no binding grants the
	// permission at all and when every binding that grants it is
	// node-scoped and unresolvable.
	Denied bool

	// TenantWide reports a grant at the tenant root: every node in the
	// tenant is in scope, and SubtreePrefixes is empty. It is the widest
	// answer DataScope can give -- it never crosses into another tenant,
	// because a binding only ever grants within its own tenant_id.
	TenantWide bool

	// SubtreePrefixes holds the materialized-path prefixes in scope, sorted
	// and de-duplicated, when the grant is not tenant-wide. Prefixes may
	// nest (both "/g1" and "/g1/r2" can appear when two bindings grant the
	// same permission at two depths); Includes handles that without the
	// caller flattening anything.
	//
	// The paths are resolved at evaluation time through the host's
	// SubtreeResolver and are never persisted, so a member who moves in the
	// tree changes scope on the next decision rather than at the next
	// re-grant.
	SubtreePrefixes []string
}

// Includes reports whether nodePath is inside this scope. A Denied scope
// includes nothing, a TenantWide scope includes everything, and otherwise
// the path must lie within at least one of SubtreePrefixes under the
// segment-aware matching PathWithinSubtree defines.
func (s DataScope) Includes(nodePath string) bool {
	switch {
	case s.Denied:
		return false
	case s.TenantWide:
		return true
	}
	for _, prefix := range s.SubtreePrefixes {
		if PathWithinSubtree(nodePath, prefix) {
			return true
		}
	}
	return false
}

// PathWithinSubtree reports whether nodePath lies inside the subtree rooted
// at prefix, both given as materialized paths.
//
// Matching is SEGMENT-AWARE, not a plain string prefix test: "/g1/r2"
// contains "/g1/r2" and "/g1/r2/s7" but NOT "/g1/r20". A plain
// strings.HasPrefix would answer true for that last pair and hand one
// region's data to a peer region -- the classic materialized-path
// authorization bug, and the reason this predicate is exported and tested
// on its own rather than inlined into the evaluator.
//
// A trailing slash on either side is insignificant, so "/g1/r2/" and
// "/g1/r2" name the same subtree. An empty string on either side matches
// nothing: an unresolved node must never behave like a wildcard. A prefix
// of "/" is the tree root and contains every absolute path.
func PathWithinSubtree(nodePath, prefix string) bool {
	if nodePath == "" || prefix == "" {
		return false
	}

	root := strings.TrimRight(prefix, "/")
	if root == "" {
		// prefix was "/" (or only slashes): the tree root, which contains
		// every absolute path but still not a relative one, so that a
		// caller mixing path conventions gets a miss rather than a
		// silently universal grant.
		return strings.HasPrefix(nodePath, "/")
	}

	node := strings.TrimRight(nodePath, "/")
	if node == root {
		return true
	}
	return strings.HasPrefix(node, root+"/")
}
