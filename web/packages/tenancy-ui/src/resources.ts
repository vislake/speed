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
 * The errors section holds the code-to-text table of the switch
 * surface's reachable answers: the membership and account-status codes
 * the switch endpoint answers (authn.tenant_membership_required,
 * authn.tenant_membership_unavailable, authn.invalid_credentials), the
 * two token-verification codes of authn's per-operation and per-route
 * guards (authn.authentication_required, authn.token_invalid), the
 * five session-lifecycle codes, the three
 * transport-level client.* codes, and errors.unknown. Where the switch
 * answer and the sign-in answer share one meaning, the texts are
 * deliberate duplicates of @speed/auth-ui's own, kept verbatim; where
 * they diverge -- the token-verification codes the pre-auth sign-in
 * surface cannot draw, and invalid_credentials' account-status meaning
 * on the switch surface -- the texts are authored here (the error-text
 * suite's
 * SWITCH_AUTHORED_TEXTS records each). Same-tier packages cannot import
 * one another's catalogs; the error-text suite beside inline-error.tsx
 * imports the auth-ui bundles themselves as test data and pins every
 * copied leaf to its source, so a divergence between the two packages'
 * copies is a translation bug that fails there, to fix in both, never a
 * reason to introduce a dependency edge.
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
