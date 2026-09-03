/**
 * The auth-ui resource bundle must satisfy the same discipline every
 * resource-carrying package does: canonical language keys, full coverage
 * of the instance's supported set, identical leaf key sets across
 * languages -- and that discipline lives in @speed/i18n's
 * registerNamespace, so this test proves the bundle by registering it on
 * a fresh instance. A key added to one language file and forgotten in the
 * other fails here before any component can ship it.
 *
 * Language text is asserted against the shipped JSON bundles (the
 * repo-authorized home of CJK), never inline: a rendering test compares
 * t() output to the bundle value, which keeps sources and tests
 * CJK-free while proving the registered bundle is what renders.
 */

import { describe, expect, it } from 'vitest'
import { createI18n, registerNamespace } from '@speed/i18n'
import { AUTH_UI_NAMESPACE, authUiResources } from './resources.js'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import enUS from './locales/en-US.json' with { type: 'json' }

function newInstance(): ReturnType<typeof createI18n> {
  return createI18n({
    storage: null,
    urlParameterName: null,
    searchParams: null,
    navigatorLanguages: [],
    defaultLanguage: 'zh-CN',
  })
}

describe('auth-ui resources', () => {
  it('register cleanly on a fresh instance (parity enforced by registerNamespace)', () => {
    const i18n = newInstance()
    expect(() =>
      registerNamespace(i18n, AUTH_UI_NAMESPACE, authUiResources),
    ).not.toThrow()
  })

  it('cover exactly the canonical supported languages, both files, no extras', () => {
    expect(Object.keys(authUiResources).sort()).toEqual(['en-US', 'zh-CN'])
  })

  it('document the shipped sections (one per component family with built-in text)', () => {
    const sections = Object.keys(authUiResources['zh-CN']!).sort()
    expect(sections).toEqual([
      'errors',
      'passwordSignIn',
      'register',
      'sessionEnded',
      'signOut',
      'smsSignIn',
      'social',
      'socialCallback',
    ])
  })

  it('render the zh-CN bundle verbatim through the registered namespace', () => {
    const i18n = newInstance()
    registerNamespace(i18n, AUTH_UI_NAMESPACE, authUiResources)
    expect(
      i18n.t('passwordSignIn.identifierLabel', { ns: AUTH_UI_NAMESPACE }),
    ).toBe(zhCN.passwordSignIn.identifierLabel)
    expect(
      i18n.t('social.provider.feishu', { ns: AUTH_UI_NAMESPACE }),
    ).toBe(zhCN.social.provider.feishu)
  })

  it('render the en-US bundle verbatim after a language switch', async () => {
    const i18n = newInstance()
    registerNamespace(i18n, AUTH_UI_NAMESPACE, authUiResources)
    await i18n.changeLanguage('en-US')
    expect(
      i18n.t('passwordSignIn.identifierLabel', { ns: AUTH_UI_NAMESPACE }),
    ).toBe(enUS.passwordSignIn.identifierLabel)
    expect(
      i18n.t('register.successMessage', { ns: AUTH_UI_NAMESPACE }),
    ).toBe(enUS.register.successMessage)
  })

  it('interpolate the SMS sent notice from each bundle template', async () => {
    const i18n = newInstance()
    registerNamespace(i18n, AUTH_UI_NAMESPACE, authUiResources)
    const phone = '+8613800138000'
    expect(
      i18n.t('smsSignIn.sentNotice', { ns: AUTH_UI_NAMESPACE, phone }),
    ).toBe(zhCN.smsSignIn.sentNotice.replace('{{phone}}', phone))
    await i18n.changeLanguage('en-US')
    expect(
      i18n.t('smsSignIn.sentNotice', { ns: AUTH_UI_NAMESPACE, phone }),
    ).toBe(enUS.smsSignIn.sentNotice.replace('{{phone}}', phone))
  })

  it('resolve nested errors-section keys to dedicated text, never the fallback', () => {
    const i18n = newInstance()
    registerNamespace(i18n, AUTH_UI_NAMESPACE, authUiResources)
    const fallback = i18n.t('errors.unknown', { ns: AUTH_UI_NAMESPACE })
    // The errors section is nested per source (errors.authn.*,
    // errors.client.*); each branch must resolve through the registered
    // namespace to its own text. Which codes the sign-in surface maps to
    // these keys -- and the unknown-code fallback rule -- is the
    // error-text resolver's own test, alongside its source file.
    for (const key of [
      'errors.authn.invalid_credentials',
      'errors.authn.email_already_registered',
      'errors.authn.identity_already_bound',
      'errors.authn.session_revoked',
      'errors.authn.token_expired',
      'errors.client.network',
    ]) {
      expect(i18n.t(key, { ns: AUTH_UI_NAMESPACE })).not.toBe(fallback)
    }
  })

  it('refuse double registration on the same instance (host registers once)', () => {
    const i18n = newInstance()
    registerNamespace(i18n, AUTH_UI_NAMESPACE, authUiResources)
    expect(() =>
      registerNamespace(i18n, AUTH_UI_NAMESPACE, authUiResources),
    ).toThrow(/already registered/)
  })
})
