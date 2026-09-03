/**
 * The account-ui translation bundle and its bare namespace.
 *
 * The host registers this namespace exactly once on its i18n instance,
 * right after createI18n (double registration throws, so it must never
 * happen inside a component or provider):
 *
 *   registerNamespace(i18n, ACCOUNT_UI_NAMESPACE, accountUiResources)
 *
 * Every user-facing string any account-ui component renders by itself
 * lives here, in zh-CN and en-US with identical leaf key sets
 * (registration enforces that). This package ships no surface keys yet --
 * the scaffold's bundles carry the errors section only, the code-level
 * text the error resolver and InlineError render; the session-list,
 * history, binding and two-factor surfaces' keys land with the component
 * families that render them. The two language files are imported with the
 * JSON import attribute so the NodeNext build keeps working; tsc copies
 * them into dist/ and the published package ships them through the "."
 * entry, never as separate locale subpaths.
 */

import type { ResourceBundle } from '@speed/i18n'
import enUS from './locales/en-US.json' with { type: 'json' }
import zhCN from './locales/zh-CN.json' with { type: 'json' }

/** The bare namespace account-ui translations register under: "account-ui". */
export const ACCOUNT_UI_NAMESPACE = 'account-ui' as const

/** The account-ui resource bundle: one bundle per supported language. */
export const accountUiResources: Readonly<Record<string, ResourceBundle>> = {
  'zh-CN': zhCN as ResourceBundle,
  'en-US': enUS as ResourceBundle,
}
