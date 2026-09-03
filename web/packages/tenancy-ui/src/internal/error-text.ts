/**
 * Error-code text resolution for the tenancy-ui namespace.
 *
 * The switch operation's reachable answers -- authn.tenant_membership_required
 * (the one application-level code the tenant/switch endpoint answers:
 * the caller is not a member of the tenant it asked to switch into), the
 * session-lifecycle codes (a refused refresh or a dead session surfaces
 * as one of these, and TenantSwitcher must render the session's answer
 * rather than an invented one), plus the transport-level
 * client.network / client.timeout / client.protocol codes of the
 * @speed/api-client contract -- map, one bundle key per code, under the
 * errors section ('errors.authn.tenant_membership_required' and so on).
 * Codes outside the whitelist -- a future authn code, a
 * client.http.<status> answer, a non-ApiError throw -- resolve to
 * 'errors.unknown', so the bundle can never render a raw key and a
 * missing translation never leaks another language's text or an English
 * fallback.
 *
 * The texts themselves are the auth-ui error texts for the same codes,
 * copied verbatim: same-tier packages cannot import one another's
 * catalogs, so this errors section is a deliberate duplicate of the
 * reachable subset (see resources.ts).
 */

import { useTenancyUiTranslation } from './translation.js'

/**
 * Every code with a dedicated errors-section text. Exported so the suite
 * can pin the whitelist-to-bundle pairing in both languages: a code
 * added here without its two bundle keys (or vice versa) fails the
 * pairing test beside inline-error.tsx.
 */
export const ERROR_TEXT_CODES = [
  // authn: the one application-level code the switch endpoint answers
  // (the caller holds no membership in the tenant it asked to enter).
  'authn.tenant_membership_required',
  // authn: session lifecycle -- a switch whose session dies surfaces as
  // one of these (refused refresh, revoked session), and TenantSwitcher
  // renders the session's answer for the retryable failure.
  'authn.session_not_found',
  'authn.session_revoked',
  'authn.refresh_token_invalid',
  'authn.refresh_token_reused',
  'authn.token_expired',
  // Transport-level failures of the api-client contract.
  'client.network',
  'client.timeout',
  'client.protocol',
] as const

const KNOWN_CODES = new Set<string>(ERROR_TEXT_CODES)

/**
 * The code-to-text resolver hook: call with an ApiError code and get the
 * current-language text ('errors.unknown' for anything unlisted).
 */
export function useTenancyUiErrorText(): (code: string) => string {
  const { t } = useTenancyUiTranslation()
  return (code: string) =>
    t(KNOWN_CODES.has(code) ? `errors.${code}` : 'errors.unknown')
}
