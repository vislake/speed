/**
 * The error-text whitelist contract, pinned in both directions against
 * the shipped bundles: ERROR_TEXT_CODES (the reachable-subset whitelist
 * of the signed-in account surface) and the errors-section leaves of
 * each language file must name exactly the same codes -- errors.unknown
 * is the one errors-section key with no code, the fallback itself. A
 * code added to the whitelist without its two bundle keys, or a bundle
 * key added without its whitelist code, fails the equality pin here;
 * key parity between the two language files is enforced earlier, by
 * registerNamespace, so a key missing from one language fails
 * registration before any component can ship it.
 *
 * The render walk adds the value half: every whitelisted code must
 * resolve through the registered namespace to its own dedicated text in
 * both languages -- never the errors.unknown fallback and never a raw
 * key. Component-level render coverage (the role="alert" banner for a
 * given code) lives in the component suites beside each failure path;
 * this file pins the whole-list pairing that the banner depends on.
 *
 * The wording policy of error-text.ts adds a third pin: the eight codes
 * whose failure context is identical to the sign-in surface's (the
 * session-lifecycle family, authn.rate_limited, authn.identity_already_bound
 * and authn.identity_requires_binding) must stay verbatim copies of the
 * auth-ui texts for the same codes, in both languages, so the same server
 * answer reads the same on the account page and the sign-in surface. The
 * five session-lifecycle codes are incidentally covered by tenancy-ui's
 * own copy pin today -- tenancy-ui's whitelist shares them -- but that
 * coverage is a side effect of tenancy-ui's whitelist, never account-ui's
 * protection: tenancy-ui can narrow its whitelist for its own reasons and
 * silently drop it. The eight are pinned here, from this package's own
 * suite, against the auth-ui bundles imported below as test data.
 */

import { describe, expect, it } from 'vitest'
import { createI18n, registerNamespace } from '@speed/i18n'
import { ACCOUNT_UI_NAMESPACE, accountUiResources } from '../resources.js'
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }
// The same-tier rule keeps this package from importing auth-ui's catalog
// at runtime; the verbatim-copy pin below imports the source bundles
// here, as test data only -- JSON data is no code dependency, so this
// crosses no package boundary the rule draws.
import authUiZhCN from '../../../auth-ui/src/locales/zh-CN.json' with { type: 'json' }
import authUiEnUS from '../../../auth-ui/src/locales/en-US.json' with { type: 'json' }
import { ERROR_TEXT_CODES } from './error-text.js'

/** A language bundle's errors section, structural so both packages'
 * bundles (account-ui's own and the auth-ui bundles imported as test
 * data) satisfy the helpers below. */
type Bundle = { errors: unknown }

/** The code leaf keys of the errors section, e.g. 'authn.session_revoked'. */
function errorLeafKeys(bundle: Bundle): string[] {
  const leaves: string[] = []
  for (const [source, codes] of Object.entries(
    bundle.errors as unknown as Record<string, Record<string, string>>,
  )) {
    if (source === 'unknown') {
      continue
    }
    for (const code of Object.keys(codes)) {
      leaves.push(`${source}.${code}`)
    }
  }
  return leaves.sort()
}

/** The errors-section value a whitelisted code must resolve to. */
function bundleText(bundle: Bundle, code: string): string {
  const [source, leaf] = code.split('.')
  const section = (
    bundle.errors as unknown as Record<string, Record<string, string>>
  )[source!]
  return section?.[leaf!] ?? ''
}

function newInstance(): ReturnType<typeof createI18n> {
  const i18n = createI18n({
    storage: null,
    urlParameterName: null,
    searchParams: null,
    navigatorLanguages: [],
    defaultLanguage: 'zh-CN',
  })
  registerNamespace(i18n, ACCOUNT_UI_NAMESPACE, accountUiResources)
  return i18n
}

/**
 * The whitelisted codes whose account-ui text is a verbatim copy of the
 * auth-ui text for the same code -- the eight codes error-text.ts's
 * wording policy names (the session-lifecycle family, authn.rate_limited,
 * authn.identity_already_bound and authn.identity_requires_binding). Codes
 * outside this list are outside the verbatim claim even where a
 * same-named auth-ui leaf exists: five of them (authn.channel_disabled,
 * authn.oauth_state_invalid, authn.social_exchange_failed,
 * authn.provider_unknown, authn.redirect_uri_not_allowed) already carry
 * account-authored texts of their own, different from the sign-in
 * bundle's, so no copy claim exists for them to pin -- the pin asserts
 * exactly the eight codes the wording policy claims.
 */
const VERBATIM_CODES: readonly string[] = [
  // authn: session lifecycle -- the session list's revoke operation and
  // the login-history surface can answer with these, and a host renders
  // them for its own protected operations; a refused refresh surfaces as
  // one of them through the silent-refresh leg. A dead or refused
  // session reads the same on the account page and the sign-in surface.
  'authn.session_not_found',
  'authn.session_revoked',
  'authn.token_expired',
  'authn.refresh_token_invalid',
  'authn.refresh_token_reused',
  // authn: the shared rate limiter -- any operation behind it.
  'authn.rate_limited',
  // authn: social bindings -- the callback exchange answers these on the
  // same identity gates a sign-in authorize passes, so the sign-in
  // surface's wording is the account surface's.
  'authn.identity_already_bound',
  'authn.identity_requires_binding',
]

describe('error-text whitelist', () => {
  it('keep the whitelist and the bundle errors leaves identical, in both languages', () => {
    const expected = [...ERROR_TEXT_CODES].sort()
    expect(errorLeafKeys(zhCN)).toEqual(expected)
    expect(errorLeafKeys(enUS)).toEqual(expected)
  })

  it('resolve every whitelisted code to its own text in both languages', async () => {
    const i18n = newInstance()
    for (const code of ERROR_TEXT_CODES) {
      // Dedicated text, never the unknown fallback and never a raw key:
      // fallbackLng is false, so a missing key would render as the key
      // itself -- asserted against both failure shapes.
      const zh = i18n.t(`errors.${code}`, { ns: ACCOUNT_UI_NAMESPACE })
      expect(zh, code).not.toBe(zhCN.errors.unknown)
      expect(zh, code).not.toBe(`errors.${code}`)
      expect(zh, code).toBe(bundleText(zhCN, code))
    }
    await i18n.changeLanguage('en-US')
    for (const code of ERROR_TEXT_CODES) {
      const en = i18n.t(`errors.${code}`, { ns: ACCOUNT_UI_NAMESPACE })
      expect(en, code).not.toBe(enUS.errors.unknown)
      expect(en, code).not.toBe(`errors.${code}`)
      expect(en, code).toBe(bundleText(enUS, code))
    }
  })

  it('keep every policy-named shared-answer text a verbatim copy of the auth-ui text for the same code', () => {
    for (const code of VERBATIM_CODES) {
      // The auth-ui leaf is the copy's source: a missing or empty one
      // fails here rather than passing a vacuous equality, in either
      // language.
      const authZh = bundleText(authUiZhCN, code)
      const authEn = bundleText(authUiEnUS, code)
      expect(authZh, code).not.toBe('')
      expect(authEn, code).not.toBe('')
      expect(bundleText(zhCN, code), code).toBe(authZh)
      expect(bundleText(enUS, code), code).toBe(authEn)
    }
  })

  it('keep every verbatim-listed code whitelisted', () => {
    // The pin list must not drift into dead copy of its own: a code the
    // whitelist no longer draws is a code the pin should stop claiming,
    // not one it quietly stops enforcing -- the wording policy of
    // error-text.ts names the same eight, and the pairing test above
    // already tied each whitelisted code to its two bundle leaves.
    for (const code of VERBATIM_CODES) {
      expect(
        (ERROR_TEXT_CODES as readonly string[]).includes(code),
        `${code} is pinned as verbatim but not whitelisted`,
      ).toBe(true)
    }
  })
})
