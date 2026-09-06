/**
 * The error-text whitelist contract, pinned in both directions against
 * the shipped bundles: ERROR_TEXT_CODES (the reachable-subset whitelist
 * of the tenant-switch surface) and the errors-section leaves of each
 * language file must name exactly the same codes -- errors.unknown is
 * the one errors-section key with no code, the fallback itself. A code
 * added to the whitelist without its two bundle keys, or a bundle key
 * added without its whitelist code, fails the equality pin here; key
 * parity between the two language files is enforced earlier, by
 * registerNamespace, so a key missing from one language fails
 * registration before any component can ship it.
 *
 * The render walk adds the value half: every whitelisted code must
 * resolve through the registered namespace to its own dedicated text in
 * both languages -- never the errors.unknown fallback and never a raw
 * key. Where the switch answer and the sign-in answer share one meaning,
 * the texts are the auth-ui error texts for the same codes, copied
 * verbatim (same-tier packages cannot import one another's catalogs),
 * and the verbatim claim is pinned here too: the auth-ui bundles
 * themselves are imported as test data, and every shared-answer
 * whitelisted code's two tenancy-ui leaves must equal the auth-ui leaf
 * of the same code, so a divergence between the packages' copies fails
 * this file instead of reaching the product. Codes the switch surface
 * draws with a meaning of its own -- or that the pre-auth sign-in
 * surface cannot draw at all -- ship authored texts, enumerated in
 * SWITCH_AUTHORED_TEXTS below with the reason each is exempt from the
 * copy pin. Component-level render coverage (the role="alert" banner
 * for a given code) lives in TenantSwitcher.test.tsx; this file pins
 * the whole-list pairing that banner depends on.
 */

import { describe, expect, it } from 'vitest'
import { TENANCY_UI_NAMESPACE, tenancyUiResources } from '../resources.js'
import { createI18n, registerNamespace } from '@speed/i18n'
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }
// The same-tier rule keeps this package from importing auth-ui's catalog
// at runtime; the verbatim-copy pin below imports the source bundles
// here, as test data only.
import authUiZhCN from '../../../auth-ui/src/locales/zh-CN.json' with { type: 'json' }
import authUiEnUS from '../../../auth-ui/src/locales/en-US.json' with { type: 'json' }
import { ERROR_TEXT_CODES } from './error-text.js'

/**
 * Whitelisted codes whose tenancy-ui text is authored here rather than
 * copied from the auth-ui bundle, with the reason each is exempt from
 * the verbatim-copy pin: the switch surface is a protected operation,
 * so the authn middleware can answer it with the two token-verification
 * codes authn.authentication_required and authn.token_invalid -- a
 * PRE-AUTH sign-in surface cannot be answered with either, which is why
 * auth-ui ships no leaf for them and no copy exists to pin against --
 * and authn.invalid_credentials means something different on the switch
 * surface than on the sign-in one: go/authn Service.SwitchTenant
 * answers it when the ACCOUNT is not active (its user-status check),
 * never for a wrong password, so the sign-in surface's "email, phone
 * number or password is incorrect" would lie here. The values record
 * the divergence; the copy pin below skips these codes and the
 * dedicated-text walk still covers them.
 */
const SWITCH_AUTHORED_TEXTS: Readonly<Record<string, string>> = {
  'authn.authentication_required':
    'the middleware answers a credential-less protected request with it',
  'authn.token_invalid':
    'the middleware answers an unverifiable access token with it',
  'authn.invalid_credentials':
    'on the switch surface it means the account is not active, never a wrong password',
}

/** A language bundle's errors section, structural so both packages'
 * bundles satisfy the helpers below. */
type Bundle = { errors: unknown }

/** The code leaf keys of the errors section, e.g. 'authn.token_expired'. */
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
  registerNamespace(i18n, TENANCY_UI_NAMESPACE, tenancyUiResources)
  return i18n
}

describe('tenancy-ui error-text whitelist', () => {
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
      const zh = i18n.t(`errors.${code}`, { ns: TENANCY_UI_NAMESPACE })
      expect(zh, code).not.toBe(zhCN.errors.unknown)
      expect(zh, code).not.toBe(`errors.${code}`)
      expect(zh, code).toBe(bundleText(zhCN, code))
    }
    await i18n.changeLanguage('en-US')
    for (const code of ERROR_TEXT_CODES) {
      const en = i18n.t(`errors.${code}`, { ns: TENANCY_UI_NAMESPACE })
      expect(en, code).not.toBe(enUS.errors.unknown)
      expect(en, code).not.toBe(`errors.${code}`)
      expect(en, code).toBe(bundleText(enUS, code))
    }
  })

  it('keep every shared-answer text a verbatim copy of the auth-ui text for the same code', () => {
    for (const code of ERROR_TEXT_CODES) {
      const authZh = bundleText(authUiZhCN, code)
      const authEn = bundleText(authUiEnUS, code)
      if (SWITCH_AUTHORED_TEXTS[code] !== undefined) {
        // Authored text (see SWITCH_AUTHORED_TEXTS): the switch answer's
        // meaning diverges from the sign-in answer's, or the sign-in
        // surface cannot draw the code at all. Still dedicated in both
        // languages -- the resolve-every-whitelisted-code walk covers
        // it, and the pairing test above already tied the code to its
        // two bundle leaves.
        expect(bundleText(zhCN, code), code).not.toBe('')
        expect(bundleText(enUS, code), code).not.toBe('')
        continue
      }
      // The auth-ui leaf is the copy's source: a missing or empty one
      // fails here rather than passing a vacuous equality.
      expect(authZh, code).not.toBe('')
      expect(authEn, code).not.toBe('')
      expect(bundleText(zhCN, code), code).toBe(authZh)
      expect(bundleText(enUS, code), code).toBe(authEn)
    }
  })

  it('document every authored-text exemption with a whitelisted code behind it', () => {
    // The exemption list must not drift into dead copy of its own: every
    // authored code is actually whitelisted (the copy pin above would
    // otherwise skip a code it should be copying), and the reasons stay
    // attached to codes that exist.
    for (const code of Object.keys(SWITCH_AUTHORED_TEXTS)) {
      expect(
        (ERROR_TEXT_CODES as readonly string[]).includes(code),
        `${code} is listed as authored but not whitelisted`,
      ).toBe(true)
    }
  })

  it('cover the switch endpoint answers and the session-lifecycle codes', () => {
    expect(ERROR_TEXT_CODES).toEqual(
      expect.arrayContaining([
        // The switch endpoint's own answers: membership in the target
        // (required), membership that cannot be established at all
        // (unavailable -- an unwired MembershipReader fails closed), and
        // the account-status refusal (invalid_credentials) the
        // Service.SwitchTenant user-status check answers.
        'authn.tenant_membership_required',
        'authn.tenant_membership_unavailable',
        'authn.invalid_credentials',
        // The middleware token-verification answers a protected switch
        // request can draw: no credential presented, an unverifiable
        // access token.
        'authn.authentication_required',
        'authn.token_invalid',
        // Session lifecycle: a switch whose session dies surfaces as one
        // of these through the silent-refresh leg.
        'authn.session_not_found',
        'authn.session_revoked',
        'authn.refresh_token_invalid',
        'authn.refresh_token_reused',
        'authn.token_expired',
        // Transport-level failures of the api-client contract.
        'client.network',
        'client.timeout',
        'client.protocol',
      ]),
    )
  })
})
