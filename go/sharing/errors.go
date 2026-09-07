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

	// ErrExpiryOutOfRange reports that Service.Create's caller supplied an
	// ExpiresAt outside the bounds rule 2 allows an explicit expiry to
	// occupy: not strictly in the future (a share born already expired is
	// refused rather than created dead), or further out than the tenant's
	// operative explicit-expiry ceiling -- MaxExplicitShareLifetime (the
	// module's 30-day ceiling on explicit requests), raised to the tenant's
	// configured default when that default is longer (see
	// MaxExplicitShareLifetime's own doc comment). The second half is the
	// "never-expiring in disguise" arm of rule 2: an expiresAt of
	// 9999-12-31 must not be accepted just because it is technically not
	// Forever.
	ErrExpiryOutOfRange = apperr.Invalid("sharing.expiry_out_of_range")

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

	// ErrInvalidRequest reports that a request this module's HTTP layer
	// could not even parse into a valid call failed before reaching
	// Service at all. Two distinct sources feed this same code: the public
	// access route's spec-generated parameter binder rejecting a request
	// (a missing or malformed token query parameter, a duplicated
	// X-Sharing-Password header -- api/sharing-server.gen.go's
	// ServerInterfaceWrapper), via NewHandler's custom ErrorHandlerFunc
	// (bindingErrorHandler, which also guarantees Cache-Control: no-store
	// on the response -- see AGENTS.md's "Revocation and caching" section
	// for why every response the access route can produce must carry that
	// header); and, as of the owner-facing round, a malformed JSON body on
	// sharing_createShare (handler.go's decodeJSON), which never reaches
	// the spec-generated binder at all since a request body is not a
	// bound parameter. Both sources report the same code because both mean
	// the identical thing to a caller: "the request could not be
	// understood", never a validation Service itself performed.
	ErrInvalidRequest = apperr.Invalid("sharing.invalid_request")

	// ErrInternal reports a failure this module cannot classify -- a
	// storage error, or a stored row that violates an invariant this
	// module maintains. It wraps the underlying error as its cause so the
	// trace carries it; the cause never reaches an API response body.
	ErrInternal = apperr.Internal("sharing.internal_error")

	// ErrResourceUnavailable reports that Handler's public access route
	// (handler.go) authorized a share but the resource behind its
	// ResourceRef could not actually be read: no ResourceResolver was wired
	// (Handler.resolver nil) or the wired one returned an error opening it.
	// The attempt is settled as denied -- one denied access-log row, no view
	// consumed (SharingAccessShare's own doc comment) -- so a MaxViews
	// budget is never spent by a serve whose content nobody saw.
	// This is deliberately a distinct, undisguised failure from
	// ErrNotAccessible: rule 5's outward-identical-answer property covers
	// the question "is this token/password valid", which the authorization
	// has already answered yes to by the time this error can occur, so there
	// is nothing left to hide by collapsing this into the same 404 -- doing
	// so would instead hide a real operational fault (a broken resolver, a
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
