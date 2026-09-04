package sharing

import (
	"net/http"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// The error index of the sharing module. Every exported error is an
// *apperr.Error builder whose Code follows the <module>.<reason> convention
// the backend coding standard requires: match a decorated error with
// apperr.As(err) and compare its Code, never with == or errors.Is against
// the var below. WithParam and WithCause derive a NEW *apperr.Error rather
// than mutating the receiver, so the pointer a call returns is never the
// pointer declared here -- the same convention every other module in this
// codebase documents.
//
// Every code in this file has a matching description entry in
// locales/{zh-CN,en-US}.toml, under the identical id.
var (
	// ErrResourceRefRequired reports that Service.Create was called with an
	// empty ResourceRef -- there is nothing to share.
	ErrResourceRefRequired = apperr.Invalid("sharing.resource_ref_required")

	// ErrExpiryRequired reports that Service.Create's caller explicitly
	// asked for a share that never expires (CreateParams.Forever). Rule 2
	// (docs/internal/07-platform-services.md's "default expiry" rule) is
	// explicit that this must be refused outright, never silently allowed
	// -- see CreateParams.Forever's own doc comment.
	ErrExpiryRequired = apperr.Invalid("sharing.expiry_required")

	// ErrInvalidMaxViews reports that Service.Create was called with a
	// non-positive CreateParams.MaxViews. A caller that wants "unlimited
	// views" leaves MaxViews nil; zero or negative is not a meaningful
	// ceiling.
	ErrInvalidMaxViews = apperr.Invalid("sharing.invalid_max_views")

	// ErrNotAccessible is Service.Access's single outward answer for every
	// reason an access attempt may be refused: an unrecognized token, a
	// revoked, expired or view-exhausted share, or a missing or wrong
	// password. It deliberately carries no parameter distinguishing which
	// -- rule 5 (docs/internal/07-platform-services.md's "the share surface
	// must leak nothing about the tenant" rule) requires that probing a
	// token teach an outside caller nothing about which of those it
	// actually is, mirroring the existence-disclosure suppression go/authn
	// already applies to account enumeration.
	ErrNotAccessible = apperr.NotFound("sharing.not_accessible")

	// ErrShareNotFound reports that an owner-facing call (Service.Revoke,
	// Service.Get, Service.ListAccessLog) named a share id that does not
	// exist in the caller's tenant. Unlike ErrNotAccessible, this code is
	// safe to disclose: the caller here is the tenant that owns (or claims
	// to own) the share, addressing it by its own internal id, not an
	// external viewer holding a bearer token.
	ErrShareNotFound = apperr.NotFound("sharing.share_not_found")

	// ErrInvalidRequest reports that Handler's public access route
	// (handler.go) received a request the spec-generated parameter binder
	// itself rejected -- a missing or malformed token query parameter, a
	// duplicated X-Sharing-Password header, and so on
	// (api/sharing-server.gen.go's ServerInterfaceWrapper). NewHandler's
	// custom ErrorHandlerFunc wraps the binder's error with this code so a
	// binding failure still answers the module's own SharingError JSON
	// envelope -- and, more importantly, still carries
	// Cache-Control: no-store -- rather than falling through to
	// oapi-codegen's default http.Error handling, which does neither. See
	// AGENTS.md's "Revocation and caching" section for why every response
	// this route can produce must carry that header.
	ErrInvalidRequest = apperr.Invalid("sharing.invalid_request")

	// ErrInternal reports a failure this module cannot classify -- a
	// storage error, or a stored row that violates an invariant this
	// module maintains. It wraps the underlying error as its cause so the
	// trace carries it; the cause never reaches an API response body.
	ErrInternal = apperr.Internal("sharing.internal_error")

	// ErrResourceUnavailable reports that Handler's public access route
	// (handler.go) reached a share whose Access already granted -- a real
	// view was already recorded -- but the resource behind its ResourceRef
	// could not actually be read: no ResourceResolver was wired
	// (Handler.resolver nil) or the wired one returned an error opening it.
	// This is deliberately a distinct, undisguised failure from
	// ErrNotAccessible: rule 5's outward-identical-answer property covers
	// the question "is this token/password valid", which Access has
	// already answered yes to by the time this error can occur, so there is
	// nothing left to hide by collapsing this into the same 404 -- doing so
	// would instead hide a real operational fault (a broken resolver, a
	// resource genuinely gone from its own store) behind a code that means
	// "check your token", which is actively misleading to whoever operates
	// this deployment. Status 502: the share surface itself worked: the
	// resource behind it did not.
	ErrResourceUnavailable = &apperr.Error{Code: "sharing.resource_unavailable", Status: http.StatusBadGateway}
)

// hasCode reports whether err is, or wraps, an *apperr.Error with the given
// code. Codes are compared rather than pointers because WithParam and
// WithCause derive a new *apperr.Error every time -- the same helper
// go/storage's errors.go and go/org's own error-translation call sites use.
func hasCode(err error, code string) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == code
}
