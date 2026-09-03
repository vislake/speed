package org

import (
	"net/http"

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

	// ErrNodeHasMembers reports a delete of a node that still has members
	// bound to it or to something beneath it. org refuses rather than
	// deleting their memberships along with the node: a membership is who a
	// person is inside a tenant, and a structural edit must not silently
	// change that. Move the members elsewhere in the tree first.
	ErrNodeHasMembers = apperr.Conflict("org.node_has_members")

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

// The membership and invitation half of the error index. Same convention as
// the tree errors above: a package-level *apperr.Error sentinel per code,
// matched with apperr.As and a Code comparison, described in both locale
// files under the identical id.
var (
	// ErrMembershipNotFound reports that the user has no membership in the
	// caller's tenant. Like ErrNodeNotFound it does not distinguish "no such
	// user anywhere" from "that user is a member of a different tenant":
	// telling the two apart would let a caller probe another tenant's roster,
	// and org does not know whether a user id exists at all -- the users table
	// is authn's, and org never reads it.
	ErrMembershipNotFound = apperr.NotFound("org.membership_not_found")

	// ErrMembershipExists reports an attempt to give a user a second
	// membership in one tenant. A user sits at exactly one place in a tenant's
	// tree; the database carries the matching UNIQUE(tenant_id, user_id)
	// index, so this is also what a lost race on that constraint reports.
	ErrMembershipExists = apperr.Conflict("org.membership_exists")

	// ErrMemberNotRemovable reports a removal that would leave the tenant with
	// no active member at all. Somebody has to be able to invite the next
	// person, and org has no privileged path back in: the invitation endpoint
	// requires an authenticated member, so an empty tenant is unrecoverable
	// from inside the product.
	ErrMemberNotRemovable = apperr.Conflict("org.member_not_removable")

	// ErrInvitationNotFound reports a token that matches no pending
	// invitation of the caller's tenant. A token minted for another tenant
	// reports exactly this, because the lookup is tenant-scoped and the token
	// itself is never trusted to name a tenant -- see InviteService.Accept.
	ErrInvitationNotFound = apperr.NotFound("org.invitation_not_found")

	// ErrInvitationExpired reports an invitation whose ExpiresAt has passed.
	// Expiry is evaluated at acceptance time against the service's clock, not
	// by a background sweep, so a stale row can never be accepted even if
	// nothing has cleaned it up yet.
	ErrInvitationExpired = apperr.Conflict("org.invitation_expired")

	// ErrInvitationAlreadyAccepted reports a second acceptance of one
	// invitation. Acceptance is idempotent in its effect -- the membership is
	// ensured, never duplicated -- but it is not silent: a replayed link is
	// reported rather than pretended away, because a token arriving twice is
	// worth surfacing.
	ErrInvitationAlreadyAccepted = apperr.Conflict("org.invitation_already_accepted")

	// ErrInvitationRevoked reports acceptance of an invitation the tenant
	// withdrew.
	ErrInvitationRevoked = apperr.Conflict("org.invitation_revoked")

	// ErrInvitationRateLimited reports an invite refused by one of the two
	// rate-limit dimensions InviteService applies: per tenant, and per
	// recipient address.
	//
	// apperr has no 429 constructor, so the status is set on this
	// package-owned instance directly -- which apperr.Error's own doc comment
	// sanctions ("Assigning to the exported fields directly still modifies the
	// value in place, so only do that on an instance the caller owns"). Every
	// derived error keeps the status, because clone copies it.
	ErrInvitationRateLimited = rateLimited("org.invitation_rate_limited")

	// ErrInvalidEmail reports an address with no canonical form under
	// dbkit.NormalizeEmail -- the same normalizer that computes the blind
	// index -- so an address org could not index is refused before anything
	// is stored or sent. The address itself is never echoed as a parameter:
	// it is PII, and an error payload is rendered, logged and traced.
	ErrInvalidEmail = apperr.Invalid("org.invalid_email")

	// ErrInvitationsDisabled reports an invite attempted while the
	// org.invitations feature flag is off for the caller's tenant.
	ErrInvitationsDisabled = apperr.Forbidden("org.invitations_disabled")

	// ErrEmailIndexerRequired is returned by Module.Register when no blind
	// indexer was injected with WithEmailIndexer.
	//
	// It mirrors config.Attach's ErrCipherRequired exactly: a module whose
	// declared surface includes an encrypted-and-queryable column refuses to
	// boot without the key that makes the column queryable, rather than
	// starting and failing on the first write. An invitation's email is
	// encrypted at rest and looked up by HMAC blind index; without the index
	// key org could store an invitation it could never find again.
	ErrEmailIndexerRequired = apperr.Internal("org.email_indexer_required")

	// ErrInvitationMailRequired is returned by Module.Register when the
	// invitation email is enabled -- which it is by default -- but the host
	// wired neither a sender address (WithMailFrom) nor a link builder
	// (WithInvitationLinkBuilder). Both are needed to render a message a
	// recipient can act on, and pkgcore.Mailer rejects an empty From outright,
	// so the gap is reported at boot instead of on the first invite.
	//
	// A host that lets something else deliver the invitation -- the M2
	// notification module subscribing to org.member.invited, say -- calls
	// WithInvitationEmailDisabled and needs neither option.
	ErrInvitationMailRequired = apperr.Internal("org.invitation_mail_required")
)

// rateLimited returns an *apperr.Error carrying HTTP 429, the status apperr
// has no constructor for.
func rateLimited(code string) *apperr.Error {
	err := apperr.Invalid(code)
	err.Status = http.StatusTooManyRequests
	return err
}
