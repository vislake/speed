/**
 * The product-shell translation bundle and its bare namespace.
 *
 * The shell renders one kind of text of its own: the polite transition
 * announcement its view machine emits when it lands the viewer in the
 * session-ended view (an unsolicited whole-page switch -- every other
 * flip is user-initiated and carries no announcement). Everything else
 * on screen belongs to the sibling namespaces the host registers for the
 * views it renders anyway. The host registers this namespace exactly
 * once on its i18n instance, right after createI18n, exactly like every
 * sibling's (double registration throws, so it must never happen inside
 * a component or provider):
 *
 *   registerNamespace(i18n, PRODUCT_SHELL_NAMESPACE, productShellResources)
 *
 * The two language files carry identical leaf key sets (registration
 * enforces that). They are imported with the JSON import attribute so
 * the NodeNext build keeps working; tsc copies them into dist/ and the
 * published package ships them through the "." entry, never as separate
 * locale subpaths.
 */

import type { ResourceBundle } from '@speed/i18n'
import enUS from './locales/en-US.json' with { type: 'json' }
import zhCN from './locales/zh-CN.json' with { type: 'json' }

/** The bare namespace product-shell translations register under: "product-shell". */
export const PRODUCT_SHELL_NAMESPACE = 'product-shell' as const

/** The product-shell resource bundle: one bundle per supported language. */
export const productShellResources: Readonly<Record<string, ResourceBundle>> = {
  'zh-CN': zhCN as ResourceBundle,
  'en-US': enUS as ResourceBundle,
}
