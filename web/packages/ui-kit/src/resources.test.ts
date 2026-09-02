/**
 * The ui-kit resource bundle must satisfy the same discipline every
 * resource-carrying package does: canonical language keys, full coverage
 * of the instance's supported set, identical leaf key sets across
 * languages -- and that discipline lives in @speed/i18n's
 * registerNamespace, so this test proves the bundle by registering it on
 * a fresh instance. A key added to one language file and forgotten in the
 * other fails here before any component can ship it.
 */

import { describe, expect, it } from 'vitest'
import { createI18n, registerNamespace } from '@speed/i18n'
import { UI_KIT_NAMESPACE, uiKitResources } from './resources.js'

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
    expect(sections).toEqual(['confirmDialog', 'dataTable', 'emptyState', 'form'])
  })

  it('render real zh-CN text through the registered namespace', () => {
    const i18n = newInstance()
    registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)
    expect(i18n.t('emptyState.empty.title', { ns: UI_KIT_NAMESPACE })).toBe('暂无数据')
    expect(i18n.t('confirmDialog.confirmLabel', { ns: UI_KIT_NAMESPACE })).toBe('确认')
  })

  it('render real en-US text through the registered namespace after a language switch', async () => {
    const i18n = newInstance()
    registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)
    await i18n.changeLanguage('en-US')
    expect(i18n.t('emptyState.empty.title', { ns: UI_KIT_NAMESPACE })).toBe('No data yet')
    expect(i18n.t('dataTable.loading', { ns: UI_KIT_NAMESPACE })).toBe('Loading')
  })

  it('interpolate the dataTable pagination summary in both languages', async () => {
    const i18n = newInstance()
    registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)
    const ns = UI_KIT_NAMESPACE
    expect(
      i18n.t('dataTable.displayedRows', { ns, from: 1, to: 5, count: 30 }),
    ).toBe('第 1–5 条，共 30 条')
    await i18n.changeLanguage('en-US')
    expect(i18n.t('dataTable.displayedRows', { ns, from: 1, to: 5, count: 30 })).toBe(
      '1–5 of 30',
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
