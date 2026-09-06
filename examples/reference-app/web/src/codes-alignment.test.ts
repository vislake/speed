/**
 * codes-alignment.test.ts -- the reference-app shell's reachable-error
 * alignment suite: every server-emittable error code the shell's four
 * surfaces can be answered with (the auth-ui sign-in/session family, the
 * account-ui signed-in family, the tenancy-ui switch family and the
 * notes create surface) is rendered through a reachable-error whitelist,
 * and this suite pins the whitelists to the server codes themselves.
 *
 * The server side of the comparison is GO_PINNED below: a hand-maintained
 * enumeration of the codes the Go side of this app can answer with on
 * these four surfaces, each entry carrying the source citation of the
 * sentinel that defines it (go/authn/errors.go for the authn codes, the
 * go/rbac and notes-module sentinels for the others). The shell side is
 * derived, never copied: the auth-ui / account-ui / tenancy-ui
 * whitelists are deep-imported from the packages' own error-text modules
 * (never from package entry points -- each package's whitelist is its
 * own, and this app-level suite reads them where they live), and the
 * notes whitelist from this app's own notes-view. The two directions are
 * asserted separately so a drift names its side:
 *
 *   - every GO_PINNED code is whitelisted by at least one surface -- a
 *     server code with no surface text would render a raw key (a code
 *     added server-side and forgotten here fails with the code in hand);
 *   - every non-client code a surface whitelists has a GO_PINNED
 *     citation -- a whitelist entry for a code no server answers here is
 *     dead copy and fails with its surface named.
 *
 * The client.network / client.timeout / client.protocol codes are the
 * @speed/api-client transport contract, not server answers, so they sit
 * outside the comparison: the suite asserts each surface whitelists
 * exactly that trio (and never a client.http.<status> family, which the
 * api-client contract leaves dynamic and every surface resolves to its
 * unknown fallback by design).
 *
 * The GO_PINNED enumeration is deliberately hand-maintained -- a
 * machine-extracted server-side code census (walking the Go sources and
 * the generated spec for every answerable code of every route this app
 * mounts) is DEFERRED: no such extractor ships in this round, the app's
 * four surfaces are small and their reachable codes are enumerated here
 * with citations for audit, and the suite's two directions keep the
 * manual list honest -- a code the server gains and the whitelists do
 * not cover, or a whitelist entry with no citation, both fail here.
 */

import { describe, expect, it } from 'vitest'
import { ERROR_TEXT_CODES as AUTH_UI_ERROR_TEXT_CODES } from '../../../../web/packages/auth-ui/src/internal/error-text.js'
import { ERROR_TEXT_CODES as ACCOUNT_UI_ERROR_TEXT_CODES } from '../../../../web/packages/account-ui/src/internal/error-text.js'
import { ERROR_TEXT_CODES as TENANCY_UI_ERROR_TEXT_CODES } from '../../../../web/packages/tenancy-ui/src/internal/error-text.js'
import { NOTE_ERROR_TEXT_KEYS } from './views/notes-view.js'

/**
 * The server-emittable codes of this app's four surfaces, each cited to
 * the source of the sentinel that defines it. The authn sentinels all
 * live in go/authn/errors.go; rbac.permission_denied is go/rbac's
 * ErrPermissionDenied, and the notes codes are the notes module
 * handler's own sentinels.
 *
 * Each citation names the file, the sentinel's current line AND the
 * sentinel identifier that defines the code -- the identifier is the
 * stable half an audit greps for when a Go edit moves the line, which
 * is how the reference-app-web.md P2-1 drift (a sentinel inserted above
 * the authn block shifted 25 of 30 line citations and stayed silent
 * until an audit re-measured them) became visible at all: a citation
 * carrying only a line number cannot be checked without re-opening the
 * Go file, one carrying the identifier can.
 */
const GO_PINNED: Readonly<Record<string, string>> = {
  // go/authn/errors.go -- the authn module's error sentinels.
  'authn.invalid_credentials': 'go/authn/errors.go:37 (ErrInvalidCredentials)',
  'authn.identifier_required': 'go/authn/errors.go:41 (ErrIdentifierRequired)',
  'authn.invalid_email': 'go/authn/errors.go:45 (ErrInvalidEmail)',
  'authn.invalid_phone': 'go/authn/errors.go:49 (ErrInvalidPhone)',
  'authn.email_already_registered': 'go/authn/errors.go:62 (ErrEmailAlreadyRegistered)',
  'authn.phone_already_registered': 'go/authn/errors.go:66 (ErrPhoneAlreadyRegistered)',
  'authn.password_too_short': 'go/authn/errors.go:69 (ErrPasswordTooShort)',
  'authn.password_too_long': 'go/authn/errors.go:72 (ErrPasswordTooLong)',
  'authn.password_too_weak': 'go/authn/errors.go:76 (ErrPasswordTooWeak)',
  'authn.token_expired': 'go/authn/errors.go:95 (ErrTokenExpired)',
  'authn.session_revoked': 'go/authn/errors.go:99 (ErrSessionRevoked)',
  'authn.refresh_token_invalid': 'go/authn/errors.go:103 (ErrRefreshTokenInvalid)',
  'authn.refresh_token_reused': 'go/authn/errors.go:109 (ErrRefreshTokenReused)',
  'authn.tenant_membership_required': 'go/authn/errors.go:115 (ErrTenantMembershipRequired)',
  'authn.oauth_state_invalid': 'go/authn/errors.go:135 (ErrOAuthStateInvalid)',
  'authn.redirect_uri_not_allowed': 'go/authn/errors.go:139 (ErrRedirectURINotAllowed)',
  'authn.provider_unknown': 'go/authn/errors.go:144 (ErrProviderUnknown)',
  'authn.social_exchange_failed': 'go/authn/errors.go:151 (ErrSocialExchangeFailed)',
  'authn.identity_requires_binding': 'go/authn/errors.go:184 (ErrIdentityRequiresBinding)',
  'authn.identity_already_bound': 'go/authn/errors.go:188 (ErrIdentityAlreadyBound)',
  'authn.identity_not_found': 'go/authn/errors.go:194 (ErrIdentityNotFound)',
  'authn.last_login_method': 'go/authn/errors.go:200 (ErrLastLoginMethod)',
  'authn.rate_limited': 'go/authn/errors.go:232 (ErrRateLimited)',
  'authn.account_locked': 'go/authn/errors.go:240 (ErrAccountLocked)',
  'authn.verification_code_invalid': 'go/authn/errors.go:258 (ErrVerificationCodeInvalid)',
  'authn.mfa_not_enrolled': 'go/authn/errors.go:268 (ErrMFANotEnrolled)',
  'authn.mfa_already_enrolled': 'go/authn/errors.go:272 (ErrMFAAlreadyEnrolled)',
  'authn.mfa_invalid_code': 'go/authn/errors.go:278 (ErrMFAInvalidCode)',
  'authn.step_up_required': 'go/authn/errors.go:282 (ErrStepUpRequired)',
  'authn.session_not_found': 'go/authn/errors.go:291 (ErrSessionNotFound)',
  // go/rbac/errors.go -- the permission-denied sentinel the notes route's
  // rbac gate answers with.
  'rbac.permission_denied': 'go/rbac/errors.go:56 (ErrPermissionDenied)',
  // examples/reference-app/internal/notes/handler.go -- the notes module
  // handler's own sentinels.
  'notes.text_required': 'examples/reference-app/internal/notes/handler.go:31 (ErrTextRequired)',
  'notes.text_too_long': 'examples/reference-app/internal/notes/handler.go:66 (ErrTextTooLong)',
  'notes.internal_error': 'examples/reference-app/internal/notes/handler.go:70 (errInternal)',
}

/** The transport codes of the @speed/api-client contract: reserved to
 * the client layer, never a server answer, and the one reserved family
 * every surface whitelists exactly (client.http.<status> stays dynamic
 * and resolves to each surface's unknown fallback). */
const CLIENT_RESERVED_CODES: readonly string[] = [
  'client.network',
  'client.timeout',
  'client.protocol',
]

function nonClientCodes(codes: readonly string[]): string[] {
  return codes.filter((code) => !code.startsWith('client.'))
}

/** The four whitelists by surface, for failure messages that name the
 * list a drift was found in. */
const SURFACE_WHITELISTS: Readonly<Record<string, readonly string[]>> = {
  'auth-ui sign-in/session': AUTH_UI_ERROR_TEXT_CODES,
  'account-ui signed-in family': ACCOUNT_UI_ERROR_TEXT_CODES,
  'tenancy-ui tenant switch': TENANCY_UI_ERROR_TEXT_CODES,
  'notes create surface': Object.keys(NOTE_ERROR_TEXT_KEYS),
}

describe('reachable-error whitelists vs the server code set', () => {
  it('keeps the hand-maintained enumeration at its audited size', () => {
    // 30 authn sentinels + rbac.permission_denied + the three notes
    // sentinels. The size guard makes a GO_PINNED edit (in either
    // direction) fail loudly here rather than silently through the
    // subset assertions below.
    expect(Object.keys(GO_PINNED)).toHaveLength(34)
  })

  it('whitelists every code the server can answer with (GO_PINNED is covered)', () => {
    const covered = new Set(
      Object.values(SURFACE_WHITELISTS).flatMap(nonClientCodes),
    )
    for (const code of Object.keys(GO_PINNED)) {
      expect(
        covered.has(code),
        `${code} (cited ${GO_PINNED[code]}) is whitelisted by no surface`,
      ).toBe(true)
    }
  })

  it('whitelists no code the server never emits on these surfaces (no dead entries)', () => {
    for (const [surface, codes] of Object.entries(SURFACE_WHITELISTS)) {
      for (const code of nonClientCodes(codes)) {
        expect(
          GO_PINNED[code] !== undefined,
          `${surface} whitelists ${code}, which no server sentinel cited here defines`,
        ).toBe(true)
      }
    }
  })

  it('treats client.* exactly as the transport contract: the reserved trio, nothing more', () => {
    for (const [surface, codes] of Object.entries(SURFACE_WHITELISTS)) {
      const clientCodes = codes.filter((code) => code.startsWith('client.'))
      expect(
        [...clientCodes].sort(),
        `${surface} whitelists the reserved trio and no other client.* family`,
      ).toEqual([...CLIENT_RESERVED_CODES].sort())
    }
  })

  it('keeps the tenancy-ui reachable set a subset of the auth-ui one', () => {
    // The switch surface's session-lifecycle answers reuse the sign-in
    // surface's texts verbatim (tenancy-ui's own error-text module
    // records the copy), so its code set must never grow beyond the
    // auth-ui family it shares texts with.
    const authUi = new Set(nonClientCodes(AUTH_UI_ERROR_TEXT_CODES))
    for (const code of nonClientCodes(TENANCY_UI_ERROR_TEXT_CODES)) {
      expect(authUi.has(code), `${code} is reachable on tenancy-ui but not whitelisted by auth-ui`).toBe(
        true,
      )
    }
  })
})
