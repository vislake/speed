/**
 * The ui-kit translation bundle and its bare namespace.
 *
 * The host registers this namespace exactly once on its i18n instance,
 * right after createI18n (double registration throws, so it must never
 * happen inside a component or provider):
 *
 *   registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)
 *
 * Every user-facing string any ui-kit component renders by itself lives
 * here, in zh-CN and en-US with identical leaf key sets (registration
 * enforces that). The two language files are imported with the JSON import
 * attribute so the NodeNext build keeps working; tsc copies them into
 * dist/ and the published package ships them through the "." entry, never
 * as separate locale subpaths.
 */

import type { ResourceBundle } from '@speed/i18n'
import enUS from './locales/en-US.json' with { type: 'json' }
import zhCN from './locales/zh-CN.json' with { type: 'json' }

/** The bare namespace ui-kit translations register under: "ui-kit". */
export const UI_KIT_NAMESPACE = 'ui-kit' as const

/** The ui-kit resource bundle: one bundle per supported language. */
export const uiKitResources: Readonly<Record<string, ResourceBundle>> = {
  'zh-CN': zhCN as ResourceBundle,
  'en-US': enUS as ResourceBundle,
}
