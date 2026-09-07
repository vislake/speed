package authn

import (
	"net/http"

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

	// ErrSearchCriteriaRequired is returned by SearchUsers when a
	// UserSearchQuery names none of Email, Phone or DisplayNamePrefix. A
	// query that could only ever mean "every user in the platform" is
	// refused rather than silently answering it: SearchUsers is the
	// platform-operator search entry point (see search.go), and an
	// unbounded scan of every identity-domain user is never the intended
	// operation behind an empty query.
	ErrSearchCriteriaRequired = apperr.Invalid("authn.search_criteria_required")

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

	// ErrDisplayNameTooLong is returned by Register for a display name
	// longer than users.display_name's declared VARCHAR(128) column (the
	// module's displayNameWidth constant) -- see model.go's column-width
	// doc comment for why the migrations' widths are the authority. It
	// carries a "max_length" parameter. The display name is REFUSED rather
	// than truncated, unlike the device/user-agent diagnostic strings: it
	// is the user's own chosen identity text, where a silent shortening
	// would corrupt what the user typed instead of only trimming noise.
	ErrDisplayNameTooLong = apperr.Invalid("authn.display_name_too_long")

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
	//
	// It is deliberately never the password-login answer: password login
	// resolves membership only after the password verified, so answering
	// with this error would certify the password to an anonymous caller;
	// that path folds into ErrInvalidCredentials instead (see
	// Service.Login's doc comment). These membership errors answer paths
	// that already know who is calling -- tenant switching, refresh, the
	// SSO/social exchanges.
	ErrTenantMembershipRequired = apperr.Forbidden("authn.tenant_membership_required")

	// ErrTenantMembershipUnavailable is returned when membership cannot be
	// established at all -- no MembershipReader was wired in, or the one
	// that was failed. It never degrades into "allow": an unanswerable
	// membership question is a refusal, not a default. Like
	// ErrTenantMembershipRequired it is deliberately never the
	// password-login answer, which folds into ErrInvalidCredentials.
	ErrTenantMembershipUnavailable = apperr.Forbidden("authn.tenant_membership_unavailable")

	// ErrRevocationCheckFailed is returned when the immediate-revocation
	// list could not be consulted. Like the membership case it fails
	// closed: a revocation check that cannot run is not a passed
	// revocation check.
	ErrRevocationCheckFailed = apperr.Internal("authn.revocation_check_failed")

	// ErrTokenVerificationFailed is returned when an access token could not
	// be verified because the verification keys themselves could not be
	// loaded -- the signature question is unanswerable, and the token is
	// never judged on an unanswerable question. It is the same
	// cannot-answer classification ErrRevocationCheckFailed carries, for
	// the same reason: an infrastructure failure must not masquerade as
	// ErrTokenInvalid, because on the client side a 401 is the refresh
	// signal and its eventual failure signs the session out over what was
	// really a pki/store hiccup. An Internal error keeps the session where
	// it is until the keys answer again.
	ErrTokenVerificationFailed = apperr.Internal("authn.token_verification_failed")

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
	// module's automatic-link rules. The channel authorized somebody whose
	// email address already belongs to an account here, but the conditions
	// for automatically linking the two were not met -- the provider did
	// not assert the address was verified, or (for social channels) the
	// provider is not on the platform's trusted list, or (for enterprise
	// SSO) the account is not an active member of the tenant that
	// configured the identity provider.
	//
	// Auto-linking on a matching address alone is the classic social-login
	// account-takeover: an attacker registers at a third-party provider
	// using the victim's address and signs straight into the victim's
	// account here. The safe path, which this error asks the client to
	// follow, is "sign in the way you already can, then bind the new
	// identity from your settings page".
	//
	// Enterprise SSO refuses with the same error when the identity
	// provider did not assert the address was verified -- whether the
	// address is registered or not, since an unverified claim must neither
	// link an existing account nor provision a new one (the claim is not
	// usable evidence of anyone's control over the address, so nothing is
	// created from it). The safe path for SSO is not "sign in another way
	// and bind"; it is verification through the identity provider itself,
	// which is the tenant administrator's fix.
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

	// ErrSSOTenantIDTooLong is returned when a tenant id longer than the
	// enterprise channel can represent enters the SSO path -- a tenant
	// administrator saving their SSO configuration, or a member starting or
	// completing an SSO sign-in under such a tenant. The synthetic
	// "oidc:<tenant>" provider name identities are stored under sits in
	// user_identities.provider, VARCHAR(64) (migration 0005), so a tenant
	// id longer than ssoTenantIDMaxWidth runes cannot be represented at
	// all: truncating the name would collide under the (provider,
	// external_id) unique index and silently merge distinct tenants'
	// identities. The refusal carries a "max_length" parameter and fires
	// at configuration/entry time, never at a later login (see
	// validateSSOTenantID in oidc.go).
	ErrSSOTenantIDTooLong = apperr.Invalid("authn.sso_tenant_id_too_long")

	// ErrSSOIssuerTooLong is returned when a tenant administrator's issuer
	// URL would overflow tenant_sso_configs.issuer (VARCHAR(512), migration
	// 0006). Configuration values are REFUSED rather than truncated -- a
	// silently shortened issuer URL would point enterprise single sign-on
	// at the wrong endpoint -- and the error names the field, carrying a
	// "max_length" parameter.
	ErrSSOIssuerTooLong = apperr.Invalid("authn.sso_issuer_too_long")

	// ErrSSOClientIDTooLong is the client_id twin of ErrSSOIssuerTooLong,
	// for tenant_sso_configs.client_id (VARCHAR(255), migration 0006).
	ErrSSOClientIDTooLong = apperr.Invalid("authn.sso_client_id_too_long")

	// ErrSSOAllowedDomainsTooLong is the allowed-domains twin of
	// ErrSSOIssuerTooLong, for the stored whitespace-delimited list in
	// tenant_sso_configs.allowed_domains (VARCHAR(1024), migration 0006).
	ErrSSOAllowedDomainsTooLong = apperr.Invalid("authn.sso_allowed_domains_too_long")

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

	// ErrRateLimited is returned when a sliding-window rate-limit
	// dimension (ratelimit.go) is over limit. It carries a
	// "retry_after_seconds" parameter. apperr has no TooManyRequests
	// builder -- every other module's error catalog has so far needed
	// only the six HTTP statuses the package ships -- so this is built
	// directly from the exported Error fields, the same way apperr's own
	// unexported newError does internally.
	ErrRateLimited = &apperr.Error{Code: "authn.rate_limited", Status: http.StatusTooManyRequests}

	// ErrAccountLocked is returned when an account is inside its
	// progressive login-failure lockout window (ratelimit.go). It carries
	// a "retry_after_seconds" parameter. It is distinct from
	// ErrRateLimited because the two answer different questions to an
	// operator reading the login history: "too many requests, from
	// wherever" versus "this specific account is temporarily locked".
	ErrAccountLocked = &apperr.Error{Code: "authn.account_locked", Status: http.StatusTooManyRequests}

	// ErrChannelDisabled is returned when a sign-in channel whose feature
	// flag is turned off is nevertheless invoked -- a POST to the password
	// login endpoint of a deployment that disabled password sign-in, for
	// instance. The flag values are public (the pre-authentication features
	// endpoint serves them to the login page), so answering a disabled
	// channel with a distinct code discloses nothing an anonymous caller
	// could not already read. It carries a "channel" parameter naming the
	// flag key.
	ErrChannelDisabled = apperr.Forbidden("authn.channel_disabled")

	// ErrVerificationCodeInvalid is the single answer to every failed
	// phone-login code verification: no code was ever issued, it expired,
	// it is locked from too many wrong guesses, the code itself was
	// wrong, or a verification already consumed it -- a spent code,
	// whose single-use guard its own (or a concurrent winner's) request
	// spent, replays into this same error, never a distinct "used"
	// answer. Collapsing every failure into one error is the same
	// user-enumeration and information-minimization defence
	// ErrInvalidCredentials is for password sign-in -- and the collapse
	// of a spent code in particular is the pre-authentication half of
	// the asymmetry whose post-authentication half is the
	// spent-vs-never-valid split ErrMFAInvalidCode and ErrMFACodeUsed
	// draw on the step-up/MFA channel: step-up answers a caller who is
	// already authenticated, so hearing "used" rather than "wrong" for a
	// code they hold discloses nothing an outsider can use. A
	// phone-login code verifies a caller who is not yet anyone, and an
	// answer distinguishing "used" from "wrong" there would certify to
	// an anonymous caller that the code they submitted was the genuine
	// live credential -- an oracle confirming an intercepted or phished
	// code. The client that may honestly hear "used" is the one that
	// itself spent the code: @speed/auth-ui's SMSSignInForm keeps that
	// verdict in a local spent-code memory and refuses the string
	// client-side, never asking this answer to confirm it (see that
	// component's file-header comment, which carries the mirror
	// analysis).
	ErrVerificationCodeInvalid = apperr.Unauthorized("authn.verification_code_invalid")

	// ErrSMSDeliveryFailed names a verification code that could not be
	// handed to the SMSSender transport. It is deliberately never RETURNED
	// by RequestSMSCode: a delivery failure must answer indistinguishably
	// from a request for an unregistered number, or every SMS-gateway
	// outage would become a registration oracle on the response status
	// (RequestSMSCode's own doc comment). The failure is logged where it
	// happens and the request answers success; the sentinel is retained in
	// the catalog for hosts whose own SMS layer wants to render the same
	// vocabulary, not as a reachable answer from this module.
	ErrSMSDeliveryFailed = apperr.Internal("authn.sms_delivery_failed")

	// ErrMFANotEnrolled is returned when an operation needs an ACTIVE TOTP
	// factor -- confirming one that was never started, regenerating
	// recovery codes with none enrolled, or a step-up with no confirmed
	// factor to verify against.
	ErrMFANotEnrolled = apperr.NotFound("authn.mfa_not_enrolled")

	// ErrMFAAlreadyEnrolled is returned when ConfirmTOTP is called for a
	// factor that is already active.
	ErrMFAAlreadyEnrolled = apperr.Conflict("authn.mfa_already_enrolled")

	// ErrMFAInvalidCode is the answer to a second-factor code that was
	// never valid: a TOTP code the factor's secret does not produce, or a
	// recovery code no issued row matches (never issued to this user, or
	// invalidated by a batch regeneration). Collapsing those together is
	// the same information-minimization defence ErrVerificationCodeInvalid
	// is -- a guesser must not be able to tell a wrong guess from an
	// issued-code miss.
	//
	// The spent-code shape -- a code that IS valid but whose single-use
	// guard has already consumed it -- deliberately does NOT collapse into
	// this code (it answers ErrMFACodeUsed instead). That split discloses
	// nothing a brute-forcer can use: the two answers differ only for a
	// code that already passed the actual TOTP check or matched an issued
	// recovery row, which means the caller is already in possession of the
	// code -- the distinction exists so a holder of a consumed code is not
	// told "invalid, try again" for a code that can never verify again.
	ErrMFAInvalidCode = apperr.Unauthorized("authn.mfa_invalid_code")

	// ErrMFACodeUsed is the answer to a second-factor code that was valid
	// but is spent: a TOTP code whose step the factor's replay guard has
	// already advanced past (the code itself was verified, or a newer code
	// was, making this one unreachable forever), an already-used recovery
	// code, or a verification that lost its compare-and-swap race to a
	// concurrent use of the same code. The refusal is exactly the one
	// ErrMFAInvalidCode carries -- the code stays single-use, the guard is
	// unchanged -- but the answer names the truth a mislabeled "invalid"
	// would hide: the code cannot verify because it already did (or was
	// superseded), never because it was wrong, and the remedy is a fresh
	// code, not a retry of this one. The disclosure is bounded to callers
	// already holding a valid code (see ErrMFAInvalidCode's own comment),
	// so no guessing oracle opens.
	ErrMFACodeUsed = apperr.Unauthorized("authn.mfa_code_used")

	// ErrStepUpRequired is returned by RequireStepUp when the calling
	// Principal's access token carries no recent second-factor proof.
	ErrStepUpRequired = apperr.Forbidden("authn.step_up_required")

	// ErrSessionNotFound is returned by the session/device-management
	// endpoints (history.go) for a session id that either does not exist
	// or does not belong to the calling user. Those two cases are
	// deliberately the same answer, for the same no-existence-disclosure
	// reason ErrIdentityNotFound is: a caller must never be able to learn
	// that a session id belongs to somebody else by getting a different
	// error for "not found" than for "not yours".
	ErrSessionNotFound = apperr.NotFound("authn.session_not_found")

	// ErrInvalidRequestBody is returned by decodeJSON (handler.go) for a
	// request body that does not decode into the operation's expected
	// shape. Every one of this module's twenty HTTP operations that reads
	// a body can answer with it, which is exactly what made its previous
	// absence from this catalog (and from both locale files) a real gap: a
	// client had no text to render for the single most common decode
	// failure any endpoint can produce, only the raw code.
	ErrInvalidRequestBody = apperr.Invalid("authn.invalid_request_body")
)

// errorCodes lists every code this module can return, in catalog order. It
// exists so the locale files and the catalog cannot drift apart unnoticed:
// errors_test.go walks it against both embedded .toml files.
var errorCodes = []string{
	ErrInvalidCredentials.Code,
	ErrIdentifierRequired.Code,
	ErrInvalidEmail.Code,
	ErrInvalidPhone.Code,
	ErrSearchCriteriaRequired.Code,
	ErrEmailAlreadyRegistered.Code,
	ErrPhoneAlreadyRegistered.Code,
	ErrPasswordTooShort.Code,
	ErrPasswordTooLong.Code,
	ErrDisplayNameTooLong.Code,
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
	ErrTokenVerificationFailed.Code,
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
	ErrSSOTenantIDTooLong.Code,
	ErrSSOIssuerTooLong.Code,
	ErrSSOClientIDTooLong.Code,
	ErrSSOAllowedDomainsTooLong.Code,
	ErrSSODomainNotAllowed.Code,
	ErrSSOTokenInvalid.Code,
	ErrInternal.Code,
	ErrRateLimited.Code,
	ErrAccountLocked.Code,
	ErrChannelDisabled.Code,
	ErrVerificationCodeInvalid.Code,
	ErrSMSDeliveryFailed.Code,
	ErrMFANotEnrolled.Code,
	ErrMFAAlreadyEnrolled.Code,
	ErrMFAInvalidCode.Code,
	ErrMFACodeUsed.Code,
	ErrStepUpRequired.Code,
	ErrSessionNotFound.Code,
	ErrInvalidRequestBody.Code,
}
