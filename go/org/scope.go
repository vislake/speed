package org

import (
	"context"
)

// Scope is the query-side, read-only view of a tenant's organization tree
// that authorization and data-visibility consumers need.
//
// # Why every signature is built from stdlib types only
//
// Not one method mentions an org type. That is the whole mechanism: a
// consumer -- rbac above all, whose path-prefix policies are written against
// a node's materialized path and whose subject resolution needs the set of
// nodes a person may see -- declares this exact interface in its OWN package
// and accepts org's implementation structurally. Go's structural interface
// satisfaction does the rest.
//
// The result is that org never imports rbac and rbac never imports org (it must not learn
// what an OrgNode is, nor inherit org's dependencies). The host wires the two
// together at bootstrap, and that host is the only place both names appear.
// A method that returned []OrgNode -- however convenient -- would destroy the
// property and force the import back.
//
// Every method reads the tenant from ctx and nothing else; there is no
// parameter through which a caller could name a tenant.
type Scope interface {
	// Path returns nodeID's materialized path within ctx's tenant, the value
	// a path-prefix authorization policy is written against. It reports an
	// error whose code is org.node_not_found when nodeID is not a node of
	// that tenant -- which is also what a node of another tenant reports.
	Path(ctx context.Context, nodeID string) (string, error)

	// DescendantIDs returns nodeID and every node beneath it, in stable
	// (depth, id) order, within ctx's tenant. The node itself is included:
	// the set answers "which nodes does standing here cover", and standing
	// somewhere covers that place.
	DescendantIDs(ctx context.Context, nodeID string) ([]string, error)

	// MemberNodeIDs returns every node id whose data userID may see in ctx's
	// tenant: the subtree rooted at the node their active membership binds
	// them to.
	//
	// It returns an empty slice and a nil error -- never an error -- when the
	// user has no membership in this tenant, or has one that is not active.
	// "This person sees nothing here" is an ordinary answer to a visibility
	// question, not a failure, and a consumer filtering a listing with
	// "node_id IN (...)" gets an empty result set from it, which is the
	// fail-closed outcome.
	MemberNodeIDs(ctx context.Context, userID string) ([]string, error)
}
