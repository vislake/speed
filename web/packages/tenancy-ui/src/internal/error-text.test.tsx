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
 * key. The texts are the auth-ui error texts for the same codes, copied
 * verbatim (same-tier packages cannot import one another's catalogs),
 * and the verbatim claim is pinned here too: the auth-ui bundles
 * themselves are imported as test data, and every whitelisted code's
 * two tenancy-ui leaves must equal the auth-ui leaf of the same code,
 * so a divergence between the packages' copies fails this file instead
 * of reaching the product. Component-level render coverage (the
 * role="alert" banner for a given code) lives in TenantSwitcher.test.tsx;
 * this file pins the whole-list pairing that banner depends on.
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

  it('keep every whitelisted text a verbatim copy of the auth-ui text for the same code', () => {
    for (const code of ERROR_TEXT_CODES) {
      const authZh = bundleText(authUiZhCN, code)
      const authEn = bundleText(authUiEnUS, code)
      // The auth-ui leaf is the copy's source: a missing or empty one
      // fails here rather than passing a vacuous equality.
      expect(authZh, code).not.toBe('')
      expect(authEn, code).not.toBe('')
      expect(bundleText(zhCN, code), code).toBe(authZh)
      expect(bundleText(enUS, code), code).toBe(authEn)
    }
  })

  it('cover the switch endpoint answer and the session-lifecycle codes', () => {
    expect(ERROR_TEXT_CODES).toEqual(
      expect.arrayContaining([
        'authn.tenant_membership_required',
        'authn.session_not_found',
        'authn.session_revoked',
        'authn.refresh_token_invalid',
        'authn.refresh_token_reused',
        'authn.token_expired',
        'client.network',
        'client.timeout',
        'client.protocol',
      ]),
    )
  })
})
