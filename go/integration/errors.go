package integration

import (
	"net/http"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// The error index of the integration module. Every exported error is an
// *apperr.Error builder: match a decorated error with apperr.As(err) and
// compare its Code, the convention dbkit, tenancy, config, rbac and org all
// document. A call that returns an error decorated with WithParam or
// WithCause derives a NEW *apperr.Error, so never compare a once-returned
// error against a var here with == or errors.Is.
//
// Every code is <module>.<reason> per backend coding standard §6.2, and
// every one has a matching message id in locales/{zh-CN,en-US}.toml. The
// API never returns the localized prose: it returns the code plus
// parameters and the client resolves the text.
//
// Deliberately absent: any code naming a capability round 1 does not build
// (webhook delivery, HMAC signature verification, SSRF-checked outbound
// addresses, and so on). Declaring a code for a feature that does not exist
// yet is a lying vocabulary the same way an undeclared config schema would
// be; see go/integration/AGENTS.md's "Deferred to a later round" section
// for what those round(s) will need to add here instead.
var (
	// ErrKeyNotFound reports an API key id that does not exist in the
	// caller's tenant. Like dbkit.ErrRecordNotFound it never distinguishes
	// "no such key" from "that key belongs to another tenant": telling the
	// two apart would let a caller enumerate another tenant's key ids.
	ErrKeyNotFound = apperr.NotFound("integration.key_not_found")

	// ErrKeyAlreadyRevoked reports a Rotate or Revoke call against a key
	// whose RevokedAt is already set. Both operations are refused rather
	// than silently no-op'd: a caller revoking (or rotating) a key it
	// believes is still live has a stale view of the world, and telling it
	// so is more useful than a quiet success that changes nothing.
	ErrKeyAlreadyRevoked = apperr.Conflict("integration.key_already_revoked")

	// ErrCreatedByRequired reports a Create call with an empty creator user
	// id. The creator is mandatory: it is both the audit trail's
	// responsible party and the identity Create validates Scopes against,
	// so there is no meaningful "anonymous" key to issue.
	ErrCreatedByRequired = apperr.Invalid("integration.created_by_required")

	// ErrExpiryExceedsMaximum reports a Create call whose requested
	// ExpiresAt is further out than MaxAPIKeyLifetime from now. The design
	// doc (docs/internal/07-platform-services.md) requires a forced expiry
	// ceiling, defaulting to one year, specifically because a key that
	// never expires is the most common credential-leak surface, so this is
	// refused rather than silently clamped: a caller that asked for ten
	// years should learn its request was rejected, not discover a
	// year-long key it never agreed to.
	ErrExpiryExceedsMaximum = apperr.Invalid("integration.expiry_exceeds_maximum")

	// ErrExpiryInPast reports a Create call whose requested ExpiresAt is
	// not strictly after the current time -- a key that would be dead on
	// arrival.
	ErrExpiryInPast = apperr.Invalid("integration.expiry_in_past")

	// ErrScopeNotHeldByCreator reports a Create call requesting a scope the
	// creator does not currently hold as a permission, per the design
	// doc's rule that a key's scope is chosen from the creator's own
	// permissions at issuance time. WithParam("scope", ...) names the
	// offending scope.
	ErrScopeNotHeldByCreator = apperr.Forbidden("integration.scope_not_held_by_creator")

	// ErrPermissionListerUnavailable reports a Create call requesting one
	// or more scopes when no PermissionLister was wired (WithPermissionLister).
	// Scopes are a security boundary, not a display convenience -- unlike
	// MembershipChecker's absence, which only blanks a cosmetic flag, an
	// unwired PermissionLister means the "subset of the creator's
	// permissions" invariant cannot be checked at all, so Create refuses
	// rather than either skipping the check or inventing an answer. A
	// Create requesting zero scopes needs no lister and is not affected --
	// see Service.Create's own doc comment.
	ErrPermissionListerUnavailable = apperr.Internal("integration.permission_lister_unavailable")

	// ErrRateLimited reports that a LayeredLimiter.Allow call denied a
	// request at one of the three layers. Status is 429, not one of
	// apperr's five builder shapes, matching go/authn's identical
	// ErrRateLimited -- a struct literal rather than apperr.Forbidden or
	// apperr.Invalid, since neither status fits a rate-limit refusal.
	// WithRateLimitParams (httpguard.go) attaches "layer" and
	// "retry_after_seconds".
	ErrRateLimited = &apperr.Error{Code: "integration.rate_limited", Status: http.StatusTooManyRequests}

	// ErrInternal reports a failure this module cannot attribute to caller
	// input -- a repository error, a PermissionLister or MembershipChecker
	// call that itself failed, or a crypto/rand failure. Its cause is never
	// surfaced past this module: an *apperr.Error's cause chain does not
	// reach an HTTP response body (backend coding standard §6.2).
	ErrInternal = apperr.Internal("integration.internal_error")
)
