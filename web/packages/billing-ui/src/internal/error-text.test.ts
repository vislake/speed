/**
 * The error-text whitelist contract, pinned in both directions against
 * the shipped bundles: ERROR_TEXT_CODES (the reachable-subset whitelist
 * of the billing read surface) and the errors-section leaves of each
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
 * key. Component-level render coverage (the role="alert" banner for a
 * given code) lives in the component suites beside each failure path;
 * this file pins the whole-list pairing that the banner depends on.
 *
 * The wording policy of error-text.ts adds a third pin: the codes whose
 * answer text this package shares with the sign-in family (the five
 * session-lifecycle codes, the three client.* transport codes and the
 * errors.unknown fallback) must stay verbatim copies of the auth-ui
 * texts for the same codes, in both languages, so the same server
 * answer reads the same on the billing surface and the sign-in
 * surface. They are pinned here, from this package's own suite,
 * against the auth-ui bundles imported below as test data.
 */

import { describe, expect, it } from 'vitest'
import { createElement } from 'react'
import { renderHook } from '@testing-library/react'
import {
  createI18n,
  I18nextProvider,
  registerNamespace,
} from '@speed/i18n'
import {
  BILLING_UI_NAMESPACE,
  billingUiResources,
} from '../resources.js'
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }
// The same-tier rule keeps this package from importing auth-ui's
// catalog at runtime; the verbatim-copy pin below imports the source
// bundles here, as test data only -- JSON data is no code dependency,
// so this crosses no package boundary the rule draws.
import authUiZhCN from '../../../auth-ui/src/locales/zh-CN.json' with { type: 'json' }
import authUiEnUS from '../../../auth-ui/src/locales/en-US.json' with { type: 'json' }
import { ERROR_TEXT_CODES, useBillingUiErrorText } from './error-text.js'

/** A language bundle's errors section, structural so both packages'
 * bundles (billing-ui's own and the auth-ui bundles imported as test
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

/** The errors.unknown leaf of one language file. */
function unknownText(language: 'zh-CN' | 'en-US'): string {
  return (language === 'zh-CN' ? zhCN : enUS).errors.unknown
}

function newInstance(
  language: 'zh-CN' | 'en-US' = 'zh-CN',
): ReturnType<typeof createI18n> {
  const i18n = createI18n({
    supportedLanguages: ['zh-CN', 'en-US'],
    defaultLanguage: language,
    storage: null,
    urlParameterName: null,
    navigatorLanguages: [],
  })
  registerNamespace(i18n, BILLING_UI_NAMESPACE, billingUiResources)
  return i18n
}

/** The whitelist codes whose text this package copies verbatim from the
 * auth-ui bundle (error-text.ts's wording policy): the five
 * session-lifecycle codes, the three transport codes. errors.unknown,
 * the fallback key itself, is pinned separately. */
const VERBATIM_SHARED_CODES = ERROR_TEXT_CODES.filter((code) =>
  code.startsWith('authn.') || code.startsWith('client.'),
)

describe('the whitelist-to-bundle pairing', () => {
  it('name exactly the same codes in both directions, in both languages', () => {
    const whitelist = [...ERROR_TEXT_CODES].sort()
    for (const bundle of [zhCN, enUS]) {
      expect(errorLeafKeys(bundle)).toEqual(whitelist)
    }
  })

  it('resolve every whitelisted code to its own text in both languages', () => {
    for (const bundle of [zhCN, enUS]) {
      for (const code of ERROR_TEXT_CODES) {
        const text = bundleText(bundle, code)
        expect(text, `${code} in ${bundle === zhCN ? 'zh-CN' : 'en-US'}`).not.toBe('')
      }
    }
    // The render walk: a code resolves through the registered namespace
    // to that dedicated text, never the unknown fallback and never a
    // raw key -- fallbackLng is false, so a missing key would render as
    // the key itself.
    for (const language of ['zh-CN', 'en-US'] as const) {
      const bundle = language === 'zh-CN' ? zhCN : enUS
      const i18n = newInstance(language)
      for (const code of ERROR_TEXT_CODES) {
        const text = i18n.t(`errors.${code}`, { ns: BILLING_UI_NAMESPACE })
        expect(text, `${code} in ${language}`).toBe(bundleText(bundle, code))
        expect(text, code).not.toBe(unknownText(language))
        expect(text, code).not.toBe(`errors.${code}`)
      }
      expect(
        i18n.t('errors.unknown', { ns: BILLING_UI_NAMESPACE }),
      ).toBe(unknownText(language))
    }
  })

  it('resolve codes outside the whitelist through the resolver to the unknown fallback', () => {
    // The raw key lookup renders a missing key as the key itself (the
    // missing-key discipline) -- the resolver is what never asks for a
    // non-whitelisted key in the first place, so the fallback gate is
    // pinned through the actual resolver hook here.
    for (const language of ['zh-CN', 'en-US'] as const) {
      const bundle = language === 'zh-CN' ? zhCN : enUS
      const { result } = renderHook(() => useBillingUiErrorText(), {
        // This suite is a .ts file (no JSX): the provider wrapper is
        // composed through createElement.
        wrapper: ({ children }) =>
          createElement(
            I18nextProvider,
            { i18n: newInstance(language) },
            children,
          ),
      })
      for (const code of [
        'billing.internal_error',
        'billing.invalid_limit',
        'client.http.429',
        'client.unknown',
      ]) {
        expect(result.current(code), code).toBe(bundle.errors.unknown)
      }
      // The whitelisted document-read code keeps its own text through
      // the same resolver.
      expect(result.current('billing.invoice_not_found')).toBe(
        bundleText(bundle, 'billing.invoice_not_found'),
      )
    }
  })
})

describe('the verbatim-copy pin to the auth-ui bundle', () => {
  it('keep the shared codes word-for-word identical to the sign-in family, in both languages', () => {
    for (const code of VERBATIM_SHARED_CODES) {
      expect(bundleText(zhCN, code), `${code} zh-CN`).toBe(
        bundleText(authUiZhCN, code),
      )
      expect(bundleText(enUS, code), `${code} en-US`).toBe(
        bundleText(authUiEnUS, code),
      )
    }
  })

  it('keep the errors.unknown fallback word-for-word identical to the sign-in family', () => {
    expect(zhCN.errors.unknown).toBe(authUiZhCN.errors.unknown)
    expect(enUS.errors.unknown).toBe(authUiEnUS.errors.unknown)
  })
})
