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
 */
const GO_PINNED: Readonly<Record<string, string>> = {
  // go/authn/errors.go -- the authn module's error sentinels.
  'authn.invalid_credentials': 'go/authn/errors.go:37',
  'authn.identifier_required': 'go/authn/errors.go:41',
  'authn.invalid_email': 'go/authn/errors.go:45',
  'authn.invalid_phone': 'go/authn/errors.go:49',
  'authn.email_already_registered': 'go/authn/errors.go:53',
  'authn.phone_already_registered': 'go/authn/errors.go:57',
  'authn.password_too_short': 'go/authn/errors.go:60',
  'authn.password_too_long': 'go/authn/errors.go:63',
  'authn.password_too_weak': 'go/authn/errors.go:67',
  'authn.token_expired': 'go/authn/errors.go:86',
  'authn.session_revoked': 'go/authn/errors.go:90',
  'authn.refresh_token_invalid': 'go/authn/errors.go:94',
  'authn.refresh_token_reused': 'go/authn/errors.go:100',
  'authn.tenant_membership_required': 'go/authn/errors.go:106',
  'authn.oauth_state_invalid': 'go/authn/errors.go:126',
  'authn.redirect_uri_not_allowed': 'go/authn/errors.go:130',
  'authn.provider_unknown': 'go/authn/errors.go:135',
  'authn.social_exchange_failed': 'go/authn/errors.go:142',
  'authn.identity_requires_binding': 'go/authn/errors.go:164',
  'authn.identity_already_bound': 'go/authn/errors.go:168',
  'authn.identity_not_found': 'go/authn/errors.go:174',
  'authn.last_login_method': 'go/authn/errors.go:180',
  'authn.rate_limited': 'go/authn/errors.go:212',
  'authn.account_locked': 'go/authn/errors.go:220',
  'authn.verification_code_invalid': 'go/authn/errors.go:228',
  'authn.mfa_not_enrolled': 'go/authn/errors.go:238',
  'authn.mfa_already_enrolled': 'go/authn/errors.go:242',
  'authn.mfa_invalid_code': 'go/authn/errors.go:248',
  'authn.step_up_required': 'go/authn/errors.go:252',
  'authn.session_not_found': 'go/authn/errors.go:261',
  // go/rbac/errors.go -- the permission-denied sentinel the notes route's
  // rbac gate answers with.
  'rbac.permission_denied': 'go/rbac/errors.go:56',
  // examples/reference-app/internal/notes/handler.go -- the notes module
  // handler's own sentinels.
  'notes.text_required': 'examples/reference-app/internal/notes/handler.go:31',
  'notes.text_too_long': 'examples/reference-app/internal/notes/handler.go:66',
  'notes.internal_error': 'examples/reference-app/internal/notes/handler.go:70',
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
