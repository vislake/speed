/**
 * The tenancy-ui resource bundle must satisfy the same discipline every
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
import { TENANCY_UI_NAMESPACE, tenancyUiResources } from './resources.js'
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

describe('tenancy-ui resources', () => {
  it('register cleanly on a fresh instance (parity enforced by registerNamespace)', () => {
    const i18n = newInstance()
    expect(() =>
      registerNamespace(i18n, TENANCY_UI_NAMESPACE, tenancyUiResources),
    ).not.toThrow()
  })

  it('cover exactly the canonical supported languages, both files, no extras', () => {
    expect(Object.keys(tenancyUiResources).sort()).toEqual(['en-US', 'zh-CN'])
  })

  it('document the shipped sections (one per component family with built-in text)', () => {
    const sections = Object.keys(tenancyUiResources['zh-CN']!).sort()
    expect(sections).toEqual(['errors', 'tenantSwitcher'])
  })

  it('render the zh-CN bundle verbatim through the registered namespace', () => {
    const i18n = newInstance()
    registerNamespace(i18n, TENANCY_UI_NAMESPACE, tenancyUiResources)
    expect(
      i18n.t('tenantSwitcher.noCurrentTenant', { ns: TENANCY_UI_NAMESPACE }),
    ).toBe(zhCN.tenantSwitcher.noCurrentTenant)
    expect(
      i18n.t('tenantSwitcher.switching', { ns: TENANCY_UI_NAMESPACE }),
    ).toBe(zhCN.tenantSwitcher.switching)
  })

  it('render the en-US bundle verbatim after a language switch', async () => {
    const i18n = newInstance()
    registerNamespace(i18n, TENANCY_UI_NAMESPACE, tenancyUiResources)
    await i18n.changeLanguage('en-US')
    expect(
      i18n.t('tenantSwitcher.noCurrentTenant', { ns: TENANCY_UI_NAMESPACE }),
    ).toBe(enUS.tenantSwitcher.noCurrentTenant)
    expect(
      i18n.t('tenantSwitcher.switching', { ns: TENANCY_UI_NAMESPACE }),
    ).toBe(enUS.tenantSwitcher.switching)
  })

  it('resolve nested errors-section keys to dedicated text, never the fallback', () => {
    const i18n = newInstance()
    registerNamespace(i18n, TENANCY_UI_NAMESPACE, tenancyUiResources)
    const fallback = i18n.t('errors.unknown', { ns: TENANCY_UI_NAMESPACE })
    // The errors section is nested per source (errors.authn.*,
    // errors.client.*); each branch must resolve through the registered
    // namespace to its own text. Which codes the switch surface maps to
    // these keys -- and the unknown-code fallback rule -- is the
    // error-text resolver's own test, alongside its source file.
    for (const key of [
      'errors.authn.tenant_membership_required',
      'errors.authn.session_not_found',
      'errors.authn.session_revoked',
      'errors.authn.refresh_token_invalid',
      'errors.authn.refresh_token_reused',
      'errors.authn.token_expired',
      'errors.client.network',
      'errors.client.timeout',
      'errors.client.protocol',
    ]) {
      expect(i18n.t(key, { ns: TENANCY_UI_NAMESPACE })).not.toBe(fallback)
    }
  })

  it('refuse double registration on the same instance (host registers once)', () => {
    const i18n = newInstance()
    registerNamespace(i18n, TENANCY_UI_NAMESPACE, tenancyUiResources)
    expect(() =>
      registerNamespace(i18n, TENANCY_UI_NAMESPACE, tenancyUiResources),
    ).toThrow(/already registered/)
  })
})
