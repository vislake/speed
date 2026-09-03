package rbac

import "context"

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
