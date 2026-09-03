package org

import (
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// The error index of the org module. Every exported error is an
// *apperr.Error builder whose Code follows the <module>.<reason> convention
// the backend coding standard requires: match a decorated error with
// apperr.As(err) and compare its Code, never with == or errors.Is against
// the var below. WithParam and WithCause derive a NEW *apperr.Error rather
// than mutating the receiver, so the pointer a call returns is never the
// pointer declared here -- the same convention dbkit, tenancy and config
// already document.
//
// Every code in this file has a matching description entry in
// locales/{zh-CN,en-US}.toml, under the identical id. The API returns the
// code and its parameters; the text is resolved by the consumer.
var (
	// ErrNodeNotFound reports that no node with the requested id exists in
	// the caller's tenant. It deliberately does not distinguish "no such
	// id anywhere" from "that id belongs to another tenant" -- the same
	// reasoning dbkit.ErrRecordNotFound documents, since telling the two
	// apart leaks the existence of another tenant's node.
	ErrNodeNotFound = apperr.NotFound("org.node_not_found")

	// ErrNodeNameRequired reports a create or rename with an empty (or
	// whitespace-only) name. A node's name is what a human navigates the
	// tree by, so a blank one is rejected rather than stored.
	ErrNodeNameRequired = apperr.Invalid("org.node_name_required")

	// ErrNodeNameTooLong reports a name longer than maxNameLen runes. The
	// bound is enforced in Go rather than left to the column width because
	// SQLite does not enforce VARCHAR(n) under type affinity, so the
	// standalone deployment mode would silently accept what the
	// distributed one rejects.
	ErrNodeNameTooLong = apperr.Invalid("org.node_name_too_long")

	// ErrParentNotFound reports a create or move whose target parent does
	// not exist in the caller's tenant.
	ErrParentNotFound = apperr.NotFound("org.parent_not_found")

	// ErrMaxDepthExceeded reports a create or move that would push a node
	// -- or, for a move, any node in the subtree being moved -- past
	// maxDepth. See path.go for why the tree is bounded at all.
	ErrMaxDepthExceeded = apperr.Invalid("org.max_depth_exceeded")

	// ErrCycleNotAllowed reports a move whose target parent is the node
	// itself or one of its own descendants. Such a move would detach the
	// subtree from the tree entirely, leaving rows reachable from nothing.
	ErrCycleNotAllowed = apperr.Invalid("org.cycle_not_allowed")

	// ErrNodeHasChildren reports a non-cascading delete of a node that
	// still has children. Deleting a subtree is an explicit request, never
	// an implicit consequence: org does not re-parent orphans to the
	// grandparent, because that silently widens every affected member's
	// data scope.
	ErrNodeHasChildren = apperr.Conflict("org.node_has_children")

	// ErrRootAlreadyExists reports a CreateRoot on a tenant that already
	// has a root node. Exactly one root per tenant is the invariant every
	// other operation reasons from -- notably Move, whose cycle check is
	// what stops the root itself from being moved.
	ErrRootAlreadyExists = apperr.Conflict("org.root_already_exists")

	// ErrRootNotDeletable reports a delete of the tenant root. The root is
	// structural: removing it would leave the tenant with no tree at all,
	// and every membership bound to a node inside it dangling.
	ErrRootNotDeletable = apperr.Conflict("org.root_not_deletable")

	// ErrDuplicateSiblingName reports a create, rename or move that would
	// give two children of the same parent the same name. The database
	// carries the matching UNIQUE(tenant_id, parent_id, name) constraint,
	// so this error is also what a lost race on that constraint reports.
	ErrDuplicateSiblingName = apperr.Conflict("org.duplicate_sibling_name")

	// ErrInvalidNodeID reports an id (or a stored path built from ids) that
	// falls outside the lowercase-hex-and-hyphen alphabet path.go pins.
	// That alphabet is not cosmetic: it is what makes a materialized-path
	// prefix scan return identical rows on SQLite, whose LIKE is
	// ASCII-case-insensitive by default, and on PostgreSQL, whose LIKE is
	// case-sensitive. See path.go's own doc comment.
	ErrInvalidNodeID = apperr.Invalid("org.invalid_node_id")

	// ErrInternal reports a failure org cannot classify -- a storage error,
	// or a stored row that violates an invariant this module maintains. It
	// wraps the underlying error as its cause so the trace carries it; the
	// cause never reaches an API response body.
	ErrInternal = apperr.Internal("org.internal_error")
)
