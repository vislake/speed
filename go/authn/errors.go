package authn

import (
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// This file is authn's error catalog. Every value here is an *apperr.Error
// whose Code follows "<module>.<reason>", and every Code has a matching entry
// in BOTH locales/zh-CN.toml and locales/en-US.toml -- the parity the i18n
// builder enforces at bootstrap and tools/check_i18n_keys.py enforces over
// the raw files.
//
// The API returns the code and its parameters, never rendered text. That is
// not a preference about response shape: backend-generated content has to
// render in the RECIPIENT's locale, and the only participant that knows what
// that is for an interactive request is the client.
//
// Holding these as package-level sentinels is safe because *apperr.Error's
// builders derive a new value instead of mutating the receiver, so decorating
// one with WithParam per request cannot race. Match on Code through
// apperr.As, never by pointer identity.
var (
	// ErrInvalidCredentials is the single answer to every failed
	// password sign-in: no such account, wrong password, an account with
	// no password set, and a suspended account all produce this exact
	// error with no distinguishing parameter.
	//
	// The uniformity is the security property. An endpoint that says
	// "no such user" for one input and "wrong password" for another is a
	// free account-enumeration oracle: an attacker learns which addresses
	// are registered without ever guessing a password, which is the first
	// step of both credential stuffing and targeted phishing. The reason
	// is recorded on the LoginAttempt row for the operator and for the
	// account owner's own security page; it is never returned.
	ErrInvalidCredentials = apperr.Unauthorized("authn.invalid_credentials")

	// ErrIdentifierRequired is returned when neither an email nor a phone
	// number was supplied where one was needed.
	ErrIdentifierRequired = apperr.Invalid("authn.identifier_required")

	// ErrInvalidEmail is returned when an email address has no canonical
	// form, so it can neither be normalized nor blind-indexed.
	ErrInvalidEmail = apperr.Invalid("authn.invalid_email")

	// ErrInvalidPhone is returned when a phone number has no E.164
	// canonical form.
	ErrInvalidPhone = apperr.Invalid("authn.invalid_phone")

	// ErrEmailAlreadyRegistered is returned when a registration would
	// collide with an existing account's email blind index.
	ErrEmailAlreadyRegistered = apperr.Conflict("authn.email_already_registered")

	// ErrPhoneAlreadyRegistered is returned when a registration would
	// collide with an existing account's phone blind index.
	ErrPhoneAlreadyRegistered = apperr.Conflict("authn.phone_already_registered")

	// ErrPasswordTooShort carries a "min_length" parameter.
	ErrPasswordTooShort = apperr.Invalid("authn.password_too_short")

	// ErrPasswordTooLong carries a "max_length" parameter.
	ErrPasswordTooLong = apperr.Invalid("authn.password_too_long")

	// ErrPasswordTooWeak is returned for a password on the policy
	// denylist, whatever its length.
	ErrPasswordTooWeak = apperr.Invalid("authn.password_too_weak")

	// ErrAuthenticationRequired is returned by RequireAuthenticated when a
	// request carried no credential at all. It is distinct from
	// ErrTokenInvalid on purpose: "you did not say who you are" and "what
	// you presented is not valid" are different situations for a client,
	// and neither reveals anything about another party.
	ErrAuthenticationRequired = apperr.Unauthorized("authn.authentication_required")

	// ErrTokenInvalid is returned for an access token that fails to parse,
	// is signed with an unexpected algorithm or an unknown key, or whose
	// claims do not satisfy the verifier.
	ErrTokenInvalid = apperr.Unauthorized("authn.token_invalid")

	// ErrTokenExpired is returned for a well-formed, correctly signed
	// access token that is past its expiry. It is separated from
	// ErrTokenInvalid so a client knows to refresh rather than to sign the
	// user out; it discloses nothing, since the client already holds the
	// token and can read its own expiry.
	ErrTokenExpired = apperr.Unauthorized("authn.token_expired")

	// ErrSessionRevoked is returned when a token's session was signed out
	// before the token's natural expiry.
	ErrSessionRevoked = apperr.Unauthorized("authn.session_revoked")

	// ErrRefreshTokenInvalid is returned for a refresh token that is
	// unknown, expired, or bound to a session that is no longer active.
	ErrRefreshTokenInvalid = apperr.Unauthorized("authn.refresh_token_invalid")

	// ErrRefreshTokenReused is returned when an ALREADY CONSUMED refresh
	// token is presented. By then the whole token family and its session
	// have been revoked, because a consumed token turning up again means a
	// copy of it exists somewhere it should not.
	ErrRefreshTokenReused = apperr.Unauthorized("authn.refresh_token_reused")

	// ErrTenantMembershipRequired is returned when a caller asks for a
	// tenant they are not an active member of. It is the fail-closed
	// answer to the single most exploited horizontal-privilege-escalation
	// entry point in a multi-tenant product.
	ErrTenantMembershipRequired = apperr.Forbidden("authn.tenant_membership_required")

	// ErrTenantMembershipUnavailable is returned when membership cannot be
	// established at all -- no MembershipReader was wired in, or the one
	// that was failed. It never degrades into "allow": an unanswerable
	// membership question is a refusal, not a default.
	ErrTenantMembershipUnavailable = apperr.Forbidden("authn.tenant_membership_unavailable")

	// ErrRevocationCheckFailed is returned when the immediate-revocation
	// list could not be consulted. Like the membership case it fails
	// closed: a revocation check that cannot run is not a passed
	// revocation check.
	ErrRevocationCheckFailed = apperr.Internal("authn.revocation_check_failed")

	// ErrOAuthStateInvalid is returned when an authorization callback's
	// "state" is unknown, expired, already used, bound to a different
	// channel, or bound to a different browser. All five collapse into one
	// error on purpose: each of them means the callback cannot be shown to
	// have come from the flow this server started, and telling an attacker
	// WHICH of the five failed tells them how to iterate.
	ErrOAuthStateInvalid = apperr.Unauthorized("authn.oauth_state_invalid")

	// ErrRedirectURINotAllowed is returned when an authorization flow asks
	// to return to a URI the deployment has not registered.
	ErrRedirectURINotAllowed = apperr.Invalid("authn.redirect_uri_not_allowed")

	// ErrProviderUnknown is returned when no social channel is registered
	// under the requested name. It carries a "provider" parameter, which
	// discloses nothing: the caller supplied the name.
	ErrProviderUnknown = apperr.Invalid("authn.provider_unknown")

	// ErrSocialExchangeFailed is returned when a channel refused the
	// authorization code, or could not be reached, or answered with
	// something this module could not read. The provider's own message is
	// deliberately not propagated: those bodies routinely echo back the
	// client secret they were sent.
	ErrSocialExchangeFailed = apperr.Unauthorized("authn.social_exchange_failed")

	// ErrSocialIdentityIncomplete is returned when a channel authorized
	// the person but did not report a stable identifier this module can
	// key on -- WeChat answering with an openid and no unionid is the case
	// that actually happens, and treating the openid as the identifier
	// would split one person into a different account per application.
	ErrSocialIdentityIncomplete = apperr.Invalid("authn.social_identity_incomplete")

	// ErrIdentityRequiresBinding is the refusal at the heart of this
	// module's social-login rules. The channel authorized somebody whose
	// email address already belongs to an account here, but the conditions
	// for automatically linking the two were not met -- the provider did
	// not assert the address was verified, or the provider is not on the
	// platform's trusted list.
	//
	// Auto-linking on a matching address alone is the classic social-login
	// account-takeover: an attacker registers at a third-party provider
	// using the victim's address and signs straight into the victim's
	// account here. The safe path, which this error asks the client to
	// follow, is "sign in the way you already can, then bind the new
	// identity from your settings page".
	ErrIdentityRequiresBinding = apperr.Conflict("authn.identity_requires_binding")

	// ErrIdentityAlreadyBound is returned when the external identity is
	// already bound to a DIFFERENT user. It never says which one.
	ErrIdentityAlreadyBound = apperr.Conflict("authn.identity_already_bound")

	// ErrIdentityNotFound is returned when the identity a caller asked to
	// unbind is not one of their own. It is deliberately the same answer
	// as "no such identity at all", so the endpoint does not confirm the
	// existence of another user's binding.
	ErrIdentityNotFound = apperr.NotFound("authn.identity_not_found")

	// ErrLastLoginMethod is returned when unbinding an identity would
	// leave the account with no way to sign in at all. Locking a user out
	// of their own account is a support incident with no self-service
	// recovery, so the operation is refused rather than confirmed.
	ErrLastLoginMethod = apperr.Conflict("authn.last_login_method")

	// ErrSSONotConfigured is returned when a tenant has no enterprise
	// single sign-on configuration, or has one that is disabled.
	ErrSSONotConfigured = apperr.NotFound("authn.sso_not_configured")

	// ErrSSOIssuerNotAllowed is returned when an issuer URL is not a valid
	// https URL, or resolves to an address inside the deployment's own
	// network. A tenant administrator types this value, which makes it a
	// server-side request forgery vector.
	ErrSSOIssuerNotAllowed = apperr.Invalid("authn.sso_issuer_not_allowed")

	// ErrSSODomainNotAllowed is returned when the identity provider
	// asserted an email address outside the domains the tenant registered.
	ErrSSODomainNotAllowed = apperr.Forbidden("authn.sso_domain_not_allowed")

	// ErrSSOTokenInvalid is returned when the identity provider's ID token
	// could not be verified, or its claims did not satisfy this module --
	// a missing subject, a mismatched nonce, an unusable email claim.
	ErrSSOTokenInvalid = apperr.Unauthorized("authn.sso_token_invalid")

	// ErrInternal is the catch-all for a server-side failure whose detail
	// must not reach the response body.
	ErrInternal = apperr.Internal("authn.internal_error")
)

// errorCodes lists every code this module can return, in catalog order. It
// exists so the locale files and the catalog cannot drift apart unnoticed:
// errors_test.go walks it against both embedded .toml files.
var errorCodes = []string{
	ErrInvalidCredentials.Code,
	ErrIdentifierRequired.Code,
	ErrInvalidEmail.Code,
	ErrInvalidPhone.Code,
	ErrEmailAlreadyRegistered.Code,
	ErrPhoneAlreadyRegistered.Code,
	ErrPasswordTooShort.Code,
	ErrPasswordTooLong.Code,
	ErrPasswordTooWeak.Code,
	ErrAuthenticationRequired.Code,
	ErrTokenInvalid.Code,
	ErrTokenExpired.Code,
	ErrSessionRevoked.Code,
	ErrRefreshTokenInvalid.Code,
	ErrRefreshTokenReused.Code,
	ErrTenantMembershipRequired.Code,
	ErrTenantMembershipUnavailable.Code,
	ErrRevocationCheckFailed.Code,
	ErrOAuthStateInvalid.Code,
	ErrRedirectURINotAllowed.Code,
	ErrProviderUnknown.Code,
	ErrSocialExchangeFailed.Code,
	ErrSocialIdentityIncomplete.Code,
	ErrIdentityRequiresBinding.Code,
	ErrIdentityAlreadyBound.Code,
	ErrIdentityNotFound.Code,
	ErrLastLoginMethod.Code,
	ErrSSONotConfigured.Code,
	ErrSSOIssuerNotAllowed.Code,
	ErrSSODomainNotAllowed.Code,
	ErrSSOTokenInvalid.Code,
	ErrInternal.Code,
}
