/**
 * The ui-kit resource bundle must satisfy the same discipline every
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
import { UI_KIT_NAMESPACE, uiKitResources } from './resources.js'
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

describe('ui-kit resources', () => {
  it('register cleanly on a fresh instance (parity enforced by registerNamespace)', () => {
    const i18n = newInstance()
    expect(() => registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)).not.toThrow()
  })

  it('cover exactly the canonical supported languages, both files, no extras', () => {
    expect(Object.keys(uiKitResources).sort()).toEqual(['en-US', 'zh-CN'])
  })

  it('document the shipped sections (one per component family with built-in text)', () => {
    const sections = Object.keys(uiKitResources['zh-CN']!).sort()
    expect(sections).toEqual([
      'confirmDialog',
      'dataTable',
      'emptyState',
      'form',
      'pageHeader',
    ])
  })

  it('render the zh-CN bundle verbatim through the registered namespace', () => {
    const i18n = newInstance()
    registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)
    expect(i18n.t('emptyState.empty.title', { ns: UI_KIT_NAMESPACE })).toBe(
      zhCN.emptyState.empty.title,
    )
    expect(i18n.t('confirmDialog.confirmLabel', { ns: UI_KIT_NAMESPACE })).toBe(
      zhCN.confirmDialog.confirmLabel,
    )
  })

  it('render the en-US bundle verbatim after a language switch', async () => {
    const i18n = newInstance()
    registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)
    await i18n.changeLanguage('en-US')
    expect(i18n.t('emptyState.empty.title', { ns: UI_KIT_NAMESPACE })).toBe(
      enUS.emptyState.empty.title,
    )
    expect(i18n.t('dataTable.loading', { ns: UI_KIT_NAMESPACE })).toBe(
      enUS.dataTable.loading,
    )
  })

  it('interpolate the pagination summary from each bundle template', async () => {
    const i18n = newInstance()
    registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)
    const ns = UI_KIT_NAMESPACE
    const render = (template: string) =>
      template.replace('{{from}}', '1').replace('{{to}}', '5').replace('{{count}}', '30')
    expect(i18n.t('dataTable.displayedRows', { ns, from: 1, to: 5, count: 30 })).toBe(
      render(zhCN.dataTable.displayedRows),
    )
    await i18n.changeLanguage('en-US')
    expect(i18n.t('dataTable.displayedRows', { ns, from: 1, to: 5, count: 30 })).toBe(
      render(enUS.dataTable.displayedRows),
    )
  })

  it('refuse double registration on the same instance (host registers once)', () => {
    const i18n = newInstance()
    registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)
    expect(() => registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)).toThrow(
      /already registered/,
    )
  })
})
