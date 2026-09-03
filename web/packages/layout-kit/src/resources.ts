/**
 * The layout-kit translation bundle and its bare namespace.
 *
 * The host registers this namespace exactly once on its i18n instance,
 * right after createI18n (double registration throws, so it must never
 * happen inside a component or provider):
 *
 *   registerNamespace(i18n, LAYOUT_KIT_NAMESPACE, layoutKitResources)
 *
 * Every user-facing string any layout-kit component renders by itself
 * lives here, in zh-CN and en-US with identical leaf key sets
 * (registration enforces that). The two language files are imported with
 * the JSON import attribute so the NodeNext build keeps working; tsc
 * copies them into dist/ and the published package ships them through
 * the "." entry, never as separate locale subpaths.
 */

import type { ResourceBundle } from '@speed/i18n'
import enUS from './locales/en-US.json' with { type: 'json' }
import zhCN from './locales/zh-CN.json' with { type: 'json' }

/** The bare namespace layout-kit translations register under: "layout-kit". */
export const LAYOUT_KIT_NAMESPACE = 'layout-kit' as const

/** The layout-kit resource bundle: one bundle per supported language. */
export const layoutKitResources: Readonly<Record<string, ResourceBundle>> = {
  'zh-CN': zhCN as ResourceBundle,
  'en-US': enUS as ResourceBundle,
}
