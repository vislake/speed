/**
 * The billing-ui translation bundle and its bare namespace.
 *
 * The host registers this namespace exactly once on its i18n instance,
 * right after createI18n (double registration throws, so it must never
 * happen inside a component or provider):
 *
 *   registerNamespace(i18n, BILLING_UI_NAMESPACE, billingUiResources)
 *
 * Every user-facing string any billing-ui component renders by itself
 * lives here, in zh-CN and en-US with identical leaf key sets
 * (registration enforces that): the errors section, the code-level text
 * the error resolver and InlineError render, and the invoices surface
 * keys of the billing-documents component family. The two language
 * files are imported with the JSON import attribute so the NodeNext
 * build keeps working; tsc copies them into dist/ and the published
 * package ships them through the "." entry, never as separate locale
 * subpaths.
 */

import type { ResourceBundle } from '@speed/i18n'
import enUS from './locales/en-US.json' with { type: 'json' }
import zhCN from './locales/zh-CN.json' with { type: 'json' }

/** The bare namespace billing-ui translations register under: "billing-ui". */
export const BILLING_UI_NAMESPACE = 'billing-ui' as const

/** The billing-ui resource bundle: one bundle per supported language. */
export const billingUiResources: Readonly<Record<string, ResourceBundle>> = {
  'zh-CN': zhCN as ResourceBundle,
  'en-US': enUS as ResourceBundle,
}
