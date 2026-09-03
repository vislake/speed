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
 */

import { describe, expect, it } from 'vitest'
import { createI18n, registerNamespace } from '@speed/i18n'
import { ACCOUNT_UI_NAMESPACE, accountUiResources } from '../resources.js'
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }
import { ERROR_TEXT_CODES } from './error-text.js'

type Bundle = typeof zhCN

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
})
