package org

import (
	"context"

	obs "github.com/vislake/speed/go/observability"
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
// The result is that org never imports rbac (docs/internal/01-architecture
// .md's graph has no such edge) and rbac never imports org (it must not learn
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

// ScopeService implements Scope over a tenant's tree and roster. Obtain one
// from Module.Scope().
//
// It is read-only by construction: it holds the tree service and the
// membership repository, and calls nothing on either that writes.
type ScopeService struct {
	tree    *TreeService
	members *MembershipRepository
}

// NewScopeService returns a ScopeService over the given tree and roster.
func NewScopeService(tree *TreeService, members *MembershipRepository) *ScopeService {
	return &ScopeService{tree: tree, members: members}
}

// Path implements Scope.
func (s *ScopeService) Path(ctx context.Context, nodeID string) (string, error) {
	node, err := s.tree.Get(ctx, nodeID)
	if err != nil {
		return "", err
	}
	if err := validatePath(node.Path); err != nil {
		return "", ErrInternal.WithCause(err)
	}
	return node.Path, nil
}

// DescendantIDs implements Scope.
func (s *ScopeService) DescendantIDs(ctx context.Context, nodeID string) ([]string, error) {
	subtree, err := s.tree.Subtree(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	return nodeIDs(subtree), nil
}

// MemberNodeIDs implements Scope.
//
// A membership whose node no longer exists yields an empty set and a Warn
// line rather than an error: the safe reading of a dangling row is "sees
// nothing", never "sees everything", and a visibility question must not fail
// the caller's request.
//
// TreeService.Delete refuses to delete a node with LIVE members bound inside
// it, but that guard alone no longer makes a dangling row impossible in
// band: MemberService.Restore (membership.go) can reintroduce exactly this
// state without touching a table directly. Its own doc comment records why
// -- it deliberately does not re-validate the restored row against Add's
// rules, "the tenant's node still existing" among them -- so the ordinary
// remove-then-delete-then-restore sequence (remove a membership at node A,
// which assertNoMembers no longer sees; delete node A; restore the earlier
// membership) leaves an active row whose NodeID names a node that is now
// mark-deleted, with no table written behind org's back anywhere in the
// sequence. This method's fail-closed reading is what keeps that reachable
// state safe rather than a hole -- see membership.go's Restore doc comment
// for the full trace and go/org/AGENTS.md's "Soft deletion" section for the
// recorded limitation.
func (s *ScopeService) MemberNodeIDs(ctx context.Context, userID string) ([]string, error) {
	membership, err := s.members.byUser(ctx, userID)
	switch {
	case hasCode(err, ErrMembershipNotFound.Code):
		return []string{}, nil
	case err != nil:
		return nil, err
	}
	if !membership.IsActive() {
		return []string{}, nil
	}

	ids, err := s.DescendantIDs(ctx, membership.NodeID)
	if hasCode(err, ErrNodeNotFound.Code) {
		obs.FromContext(ctx).Warn("org membership points at a node that no longer exists",
			"user_id", userID, "node_id", membership.NodeID)
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// nodeIDs projects a node slice onto its ids, preserving order. It always
// returns a non-nil slice so a caller can distinguish "no nodes" from a
// failure without a nil check.
func nodeIDs(nodes []OrgNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

// compile-time check that *ScopeService satisfies Scope.
var _ Scope = (*ScopeService)(nil)
