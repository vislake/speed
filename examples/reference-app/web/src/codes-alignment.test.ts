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
 *     citation or a recorded WHITELISTED_BEYOND_THIS_APP exemption --
 *     a whitelist entry for a code no server answers here is dead copy
 *     and fails with its surface named. The exemption list exists
 *     because a package whitelist is consumer-facing: it serves every
 *     deployment that can be answered with the code, which can be a
 *     wider composition than THIS app wires. Exempting such a code from
 *     the no-dead-entries direction -- instead of adding a citation
 *     this app's routes cannot earn -- keeps the two hand lists honest:
 *     GO_PINNED stays the set of codes the app itself can answer with,
 *     and the exemption records the reasoning that keeps a code out of
 *     it.
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

import { readFileSync } from 'node:fs'
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
 * Go file, one carrying the identifier can. Since that audit, the
 * suite's own "every citation sits at its declared line" check below
 * has re-measured mechanically: a Go edit that moves a sentinel now
 * fails this suite with the code, the citation and the file named
 * (the P3-rnweb-3 re-pin of the 22 authn citations an intervening
 * round's sentinel insertions had shifted).
 */
const GO_PINNED: Readonly<Record<string, string>> = {
  // go/authn/errors.go -- the authn module's error sentinels, in current
  // file order (re-measured this round: a Go edit inserted a block above
  // ErrPasswordTooWeak since the last audit, shifting every sentinel
  // from there to ErrSessionNotFound by +10 to +17 lines; the
  // token-verification answers entered the enumeration with this
  // re-measurement, and composed-stack verification then kept
  // authn.token_invalid -- authn.authentication_required's citation
  // moved to WHITELISTED_BEYOND_THIS_APP below, see its entry).
  'authn.invalid_credentials': 'go/authn/errors.go:37 (ErrInvalidCredentials)',
  'authn.identifier_required': 'go/authn/errors.go:41 (ErrIdentifierRequired)',
  'authn.invalid_email': 'go/authn/errors.go:45 (ErrInvalidEmail)',
  'authn.invalid_phone': 'go/authn/errors.go:49 (ErrInvalidPhone)',
  'authn.email_already_registered': 'go/authn/errors.go:62 (ErrEmailAlreadyRegistered)',
  'authn.phone_already_registered': 'go/authn/errors.go:66 (ErrPhoneAlreadyRegistered)',
  'authn.password_too_short': 'go/authn/errors.go:69 (ErrPasswordTooShort)',
  'authn.password_too_long': 'go/authn/errors.go:72 (ErrPasswordTooLong)',
  'authn.display_name_too_long': 'go/authn/errors.go:82 (ErrDisplayNameTooLong)',
  'authn.password_too_weak': 'go/authn/errors.go:86 (ErrPasswordTooWeak)',
  // authn.token_invalid -- the composed authn.Middleware's answer for a
  // request that presented an access token the verifier refused (a
  // tampered or otherwise invalid token; an expired one is answered
  // authn.token_expired below). The middleware is optional
  // authentication (go/authn/middleware.go): a request with no
  // credential passes through anonymous -- which is why it never writes
  // authn.authentication_required -- but a token the verifier refuses
  // is 401'd immediately, on every protected route of this app, the
  // tenant-switch one included. The no-credential code
  // (authn.authentication_required) is written only by per-operation
  // and per-route guards -- RequireAuthenticated (go/authn/middleware.go)
  // and the handler's requirePrincipal (go/authn/handler.go) -- which
  // cannot answer the switch route in this app: the route is not in the
  // pre-auth allowlist, so tenancy.Middleware's fail-closed gate refuses
  // the anonymous request first with tenancy.tenant_unresolved, verified
  // on the composed stack; that code therefore lives in
  // WHITELISTED_BEYOND_THIS_APP below, not here.
  'authn.token_invalid': 'go/authn/errors.go:98 (ErrTokenInvalid)',
  'authn.token_expired': 'go/authn/errors.go:105 (ErrTokenExpired)',
  'authn.session_revoked': 'go/authn/errors.go:109 (ErrSessionRevoked)',
  'authn.refresh_token_invalid': 'go/authn/errors.go:113 (ErrRefreshTokenInvalid)',
  'authn.refresh_token_reused': 'go/authn/errors.go:119 (ErrRefreshTokenReused)',
  'authn.tenant_membership_required': 'go/authn/errors.go:125 (ErrTenantMembershipRequired)',
  'authn.tenant_membership_unavailable': 'go/authn/errors.go:131 (ErrTenantMembershipUnavailable)',
  'authn.oauth_state_invalid': 'go/authn/errors.go:145 (ErrOAuthStateInvalid)',
  'authn.redirect_uri_not_allowed': 'go/authn/errors.go:149 (ErrRedirectURINotAllowed)',
  'authn.provider_unknown': 'go/authn/errors.go:154 (ErrProviderUnknown)',
  'authn.social_exchange_failed': 'go/authn/errors.go:161 (ErrSocialExchangeFailed)',
  'authn.identity_requires_binding': 'go/authn/errors.go:194 (ErrIdentityRequiresBinding)',
  'authn.identity_already_bound': 'go/authn/errors.go:198 (ErrIdentityAlreadyBound)',
  'authn.identity_not_found': 'go/authn/errors.go:204 (ErrIdentityNotFound)',
  'authn.last_login_method': 'go/authn/errors.go:210 (ErrLastLoginMethod)',
  'authn.rate_limited': 'go/authn/errors.go:242 (ErrRateLimited)',
  'authn.account_locked': 'go/authn/errors.go:250 (ErrAccountLocked)',
  'authn.channel_disabled': 'go/authn/errors.go:260 (ErrChannelDisabled)',
  'authn.verification_code_invalid': 'go/authn/errors.go:268 (ErrVerificationCodeInvalid)',
  'authn.mfa_not_enrolled': 'go/authn/errors.go:285 (ErrMFANotEnrolled)',
  'authn.mfa_already_enrolled': 'go/authn/errors.go:289 (ErrMFAAlreadyEnrolled)',
  'authn.mfa_invalid_code': 'go/authn/errors.go:295 (ErrMFAInvalidCode)',
  'authn.step_up_required': 'go/authn/errors.go:299 (ErrStepUpRequired)',
  'authn.session_not_found': 'go/authn/errors.go:308 (ErrSessionNotFound)',
  // go/rbac/errors.go -- the permission-denied sentinel the notes route's
  // rbac gate answers with.
  'rbac.permission_denied': 'go/rbac/errors.go:56 (ErrPermissionDenied)',
  // examples/reference-app/internal/notes/handler.go -- the notes module
  // handler's own sentinels.
  'notes.text_required': 'examples/reference-app/internal/notes/handler.go:31 (ErrTextRequired)',
  // The two declarations below sit after the maxRequestBodyBytes constant
  // block the request-body-cap round added above ErrTextTooLong; the cited
  // lines are the current declaration sites, kept in step with the Go
  // source (the deep-check below fails any drift).
  'notes.text_too_long': 'examples/reference-app/internal/notes/handler.go:80 (ErrTextTooLong)',
  'notes.internal_error': 'examples/reference-app/internal/notes/handler.go:84 (errInternal)',
}

/**
 * Whitelisted codes this app's own surfaces cannot be answered with --
 * the deliberate, reasoning-carrying exceptions to the no-dead-entries
 * direction (see the file header). Each entry names the code and why it
 * has no in-app answer, so it must not gain a GO_PINNED citation either:
 * the two hand lists would otherwise start confirming each other for a
 * code no surface here can reach.
 */
const WHITELISTED_BEYOND_THIS_APP: Readonly<Record<string, string>> = {
  'authn.social_identity_incomplete':
    'auth-ui whitelists the social exchange answers for its SocialCallbackHandler ' +
    'surface, which any host with a wired, enabled social channel can be answered ' +
    'with (the provider authorized the person but reported no stable identifier). ' +
    'This app wires no social channel: cfg.SocialProviders is empty, so ' +
    'openConfiguredAuthnChannels opens no flag rows and every social feature flag ' +
    'stays OFF -- an exchange attempt here refuses at the channel gate (go/authn/' +
    'identity.go, SocialCallback\'s gate, which runs before any identity analysis) ' +
    'with authn.channel_disabled. The code therefore has no in-app answer.',
  // go/authn/errors.go:93 (ErrAuthenticationRequired) -- the sentinel
  // citation moved here with the code when composed-stack verification
  // overturned its in-app reachability (see the token_invalid citation
  // above for who writes each and why the switch route cannot draw the
  // no-credential answer).
  'authn.authentication_required':
    'tenancy-ui whitelists the token-verification answers of authn\'s per-operation ' +
    'and per-route guards, and any host whose guard mounts without a preceding ' +
    'tenant gate can be answered with this code: RequireAuthenticated (go/authn/' +
    'middleware.go) and the handler\'s requirePrincipal (go/authn/handler.go) both ' +
    'write it for a request that presented no credential. This app cannot be ' +
    'answered with it: authn.Middleware is optional authentication (a request with ' +
    'no credential passes through anonymous, never writing this code), and the ' +
    'switch route is not in the pre-auth allowlist, so tenancy.Middleware\'s ' +
    'fail-closed gate refuses the anonymous switch request first with 403 ' +
    'tenancy.tenant_unresolved -- verified on the composed stack (anonymous POST ' +
    '/api/v1/authn/tenant/switch answers tenancy.tenant_unresolved, not this ' +
    'code). The code therefore has no in-app answer.',
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

/** A citation's path, line and sentinel identifier. */
interface SentinelCitation {
  readonly path: string
  readonly line: number
  readonly identifier: string
}

/** Parses a citation of the shape 'path:line (Identifier)'. */
function parseCitation(code: string, citation: string): SentinelCitation {
  const match = /^([^:]+):(\d+) \(([^)]+)\)$/.exec(citation)
  const path = match?.[1]
  const lineText = match?.[2]
  const identifier = match?.[3]
  if (path === undefined || lineText === undefined || identifier === undefined) {
    throw new Error(`malformed citation for ${code}: ${citation}`)
  }
  return { path, line: Number(lineText), identifier }
}

/** The repository root as a filesystem path, derived from this test
 * file's own URL (the app's src sits four levels under the root:
 * examples/reference-app/web/src). The path is assembled as plain
 * string arithmetic rather than `new URL(..., import.meta.url)`,
 * because vite's transform rewrites that pattern as an asset
 * reference; the filesystem resolves the '..' segments at open time. */
function repoRootPath(): string {
  const url = import.meta.url
  const withoutScheme = url.startsWith('file://') ? url.slice(7) : url
  const srcDir = withoutScheme.slice(0, withoutScheme.lastIndexOf('/'))
  return `${srcDir}/../../../../`
}

describe('reachable-error whitelists vs the server code set', () => {
  it('keeps every GO_PINNED citation at the line that declares its sentinel', () => {
    // The machine half of the identifier-citation discipline (see the
    // GO_PINNED doc comment): each cited line must actually declare
    // the cited sentinel, so a Go edit that moves a sentinel -- the
    // drift reference-app-web.md P2-1 recorded -- fails this suite
    // instead of waiting for the next manual audit. The files are
    // read relative to this test file (the app lives at
    // examples/reference-app/web, four levels under the repository
    // root, and the citations are repository-root-relative paths).
    for (const [code, citation] of Object.entries(GO_PINNED)) {
      const { path, line, identifier } = parseCitation(code, citation)
      const source = readFileSync(`${repoRootPath()}${path}`, 'utf8')
      const cited = source.split('\n')[line - 1]
      expect(
        cited,
        `${code} is cited at ${path}:${line} (${identifier}), but that line does not declare the sentinel`,
      ).toMatch(new RegExp(`^\\s*(?:var\\s+)?${identifier}\\s*=`))
    }
  })

  it('keeps the hand-maintained enumeration at its audited size', () => {
    // 34 authn sentinels (the 33 of the previous audit plus this
    // round's authn.token_invalid -- the second middleware-era
    // addition, authn.authentication_required, has already left
    // GO_PINNED again: composed-stack verification overturned its
    // in-app reachability, and its citation now lives in
    // WHITELISTED_BEYOND_THIS_APP with the reasoning) +
    // rbac.permission_denied + the three notes sentinels. The size
    // guard makes a GO_PINNED edit (in either direction) fail loudly
    // here rather than silently through the subset assertions below.
    expect(Object.keys(GO_PINNED)).toHaveLength(38)
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
          GO_PINNED[code] !== undefined ||
            WHITELISTED_BEYOND_THIS_APP[code] !== undefined,
          `${surface} whitelists ${code}, which no server sentinel cited here defines and no recorded exemption explains`,
        ).toBe(true)
      }
    }
  })

  it('keeps the beyond-this-app exemption list honest in both directions', () => {
    const whitelisted = new Set(
      Object.values(SURFACE_WHITELISTS).flatMap(nonClientCodes),
    )
    for (const [code, reasoning] of Object.entries(WHITELISTED_BEYOND_THIS_APP)) {
      // An exemption for a code no surface whitelists would be dead
      // copy of its own kind: the exempted code must actually be
      // reachable text somewhere on these surfaces.
      expect(
        whitelisted.has(code),
        `${code} is exempted but whitelisted by no surface: ${reasoning}`,
      ).toBe(true)
      // An exemption for a code the enumeration already carries is
      // redundant: once the app answers a code, it belongs in GO_PINNED
      // with its citation, not in the exemption list.
      expect(
        GO_PINNED[code],
        `${code} is exempted but already carries a GO_PINNED citation: ${reasoning}`,
      ).toBeUndefined()
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

  it('keeps the tenancy-ui-only codes exactly the two token-verification answers', () => {
    // Every tenancy-ui code except the two token-verification answers
    // sits inside the auth-ui family whose shared texts it copies
    // verbatim. The two exceptions are real: authn.authentication_required
    // and authn.token_invalid are the answers of authn's per-operation
    // and per-route guards -- a pre-auth sign-in surface cannot be
    // answered with either, which is why auth-ui whitelists neither and
    // tenancy-ui authors their texts instead of copying. authn.token_invalid
    // carries a GO_PINNED citation above (the composed authn.Middleware
    // 401s a presented token that fails verification on the protected
    // switch route); authn.authentication_required's reachability is
    // host-dependent, so its coverage is the recorded
    // WHITELISTED_BEYOND_THIS_APP exemption rather than an in-app
    // citation -- the no-dead-entries direction holds for both either
    // way.
    const authUi = new Set(nonClientCodes(AUTH_UI_ERROR_TEXT_CODES))
    const tenancyOnly = nonClientCodes(TENANCY_UI_ERROR_TEXT_CODES).filter(
      (code) => !authUi.has(code),
    )
    expect([...tenancyOnly].sort()).toEqual([
      'authn.authentication_required',
      'authn.token_invalid',
    ])
    for (const code of tenancyOnly) {
      expect(
        GO_PINNED[code] !== undefined ||
          WHITELISTED_BEYOND_THIS_APP[code] !== undefined,
        `${code} is reachable on tenancy-ui but carries neither a GO_PINNED citation nor a recorded exemption`,
      ).toBe(true)
    }
  })
})
