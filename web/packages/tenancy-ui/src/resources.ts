/**
 * The tenancy-ui translation bundle and its bare namespace.
 *
 * The host registers this namespace exactly once on its i18n instance,
 * right after createI18n (double registration throws, so it must never
 * happen inside a component or provider):
 *
 *   registerNamespace(i18n, TENANCY_UI_NAMESPACE, tenancyUiResources)
 *
 * Every user-facing string TenantSwitcher renders by itself lives here, in
 * zh-CN and en-US with identical leaf key sets (registration enforces
 * that). The two language files are imported with the JSON import
 * attribute so the NodeNext build keeps working; tsc copies them into
 * dist/ and the published package ships them through the "." entry, never
 * as separate locale subpaths.
 *
 * The errors section repeats the subset of @speed/auth-ui's error texts
 * the switch operation can answer with (authn.tenant_membership_required,
 * the five session-lifecycle codes, the three transport-level client.*
 * codes, errors.unknown). Same-tier packages cannot import one another's
 * catalogs, so the texts are deliberate duplicates, kept verbatim -- the
 * pairing is pinned by the error-text suite beside inline-error.tsx, and
 * a divergence between the two packages' copies is a translation bug to
 * fix in both, not a reason to introduce a dependency edge.
 */

import type { ResourceBundle } from '@speed/i18n'
import enUS from './locales/en-US.json' with { type: 'json' }
import zhCN from './locales/zh-CN.json' with { type: 'json' }

/** The bare namespace tenancy-ui translations register under: "tenancy-ui". */
export const TENANCY_UI_NAMESPACE = 'tenancy-ui' as const

/** The tenancy-ui resource bundle: one bundle per supported language. */
export const tenancyUiResources: Readonly<Record<string, ResourceBundle>> = {
  'zh-CN': zhCN as ResourceBundle,
  'en-US': enUS as ResourceBundle,
}
