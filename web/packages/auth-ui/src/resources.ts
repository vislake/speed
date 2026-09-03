/**
 * The auth-ui translation bundle and its bare namespace.
 *
 * The host registers this namespace exactly once on its i18n instance,
 * right after createI18n (double registration throws, so it must never
 * happen inside a component or provider):
 *
 *   registerNamespace(i18n, AUTH_UI_NAMESPACE, authUiResources)
 *
 * Every user-facing string any auth-ui component renders by itself lives
 * here, in zh-CN and en-US with identical leaf key sets (registration
 * enforces that). The two language files are imported with the JSON import
 * attribute so the NodeNext build keeps working; tsc copies them into
 * dist/ and the published package ships them through the "." entry, never
 * as separate locale subpaths.
 */

import type { ResourceBundle } from '@speed/i18n'
import enUS from './locales/en-US.json' with { type: 'json' }
import zhCN from './locales/zh-CN.json' with { type: 'json' }

/** The bare namespace auth-ui translations register under: "auth-ui". */
export const AUTH_UI_NAMESPACE = 'auth-ui' as const

/** The auth-ui resource bundle: one bundle per supported language. */
export const authUiResources: Readonly<Record<string, ResourceBundle>> = {
  'zh-CN': zhCN as ResourceBundle,
  'en-US': enUS as ResourceBundle,
}
