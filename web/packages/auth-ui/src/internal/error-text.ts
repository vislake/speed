/**
 * Error-code text resolution for the auth-ui namespace.
 *
 * Every reachable answer of the sign-in surface -- the authn error codes
 * the spec enumerates across register, the password/SMS logins, the SMS
 * request and the social endpoints, plus the transport-level
 * client.network / client.timeout / client.protocol codes of the
 * @speed/api-client contract -- maps, one bundle key per code, under the
 * errors section ('errors.authn.invalid_credentials' and so on). Codes
 * outside the whitelist -- a future authn code, a client.http.<status>
 * answer, a non-ApiError throw -- resolve to 'errors.unknown', so the
 * bundle can never render a raw key and a missing translation never
 * leaks another language's text or an English fallback.
 */

import { useAuthUiTranslation } from './translation.js'

/** Every code with a dedicated errors-section text. */
const ERROR_TEXT_CODES = [
  // authn: register, password/SMS logins, SMS request, social endpoints.
  'authn.invalid_credentials',
  'authn.tenant_membership_required',
  'authn.account_locked',
  'authn.rate_limited',
  'authn.verification_code_invalid',
  'authn.email_already_registered',
  'authn.phone_already_registered',
  'authn.identifier_required',
  'authn.password_too_short',
  'authn.password_too_long',
  'authn.password_too_weak',
  'authn.provider_unknown',
  'authn.redirect_uri_not_allowed',
  'authn.oauth_state_invalid',
  'authn.social_exchange_failed',
  'authn.identity_requires_binding',
  'authn.identity_already_bound',
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
export function useAuthUiErrorText(): (code: string) => string {
  const { t } = useAuthUiTranslation()
  return (code: string) =>
    t(KNOWN_CODES.has(code) ? `errors.${code}` : 'errors.unknown')
}
