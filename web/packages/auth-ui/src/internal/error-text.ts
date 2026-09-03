/**
 * Error-code text resolution for the auth-ui namespace.
 *
 * Every reachable answer of the sign-in and session surface -- the authn
 * error codes the register, password/SMS login, SMS request and social
 * endpoints answer with (those the spec enumerates, plus the
 * identifier-format answers their implementations return:
 * authn.invalid_phone on the SMS request and on the register phone
 * slot, authn.invalid_email on the register email slot's
 * canonical-form gate), the session-lifecycle codes a sign-out call or a
 * host-side protected operation can answer with, plus the
 * transport-level client.network / client.timeout / client.protocol
 * codes of the @speed/api-client contract -- maps, one bundle key per
 * code, under the errors section ('errors.authn.invalid_credentials' and
 * so on). Codes outside the whitelist -- a future authn code, a
 * client.http.<status> answer, a non-ApiError throw -- resolve to
 * 'errors.unknown', so the bundle can never render a raw key and a
 * missing translation never leaks another language's text or an English
 * fallback.
 */

import { useAuthUiTranslation } from './translation.js'

/**
 * Every code with a dedicated errors-section text. Exported so the suite
 * can pin the whitelist-to-bundle pairing in both languages: a code
 * added here without its two bundle keys (or vice versa) fails the
 * pairing test beside inline-error.tsx.
 */
export const ERROR_TEXT_CODES = [
  // authn: register, password/SMS logins, SMS request, social endpoints.
  'authn.invalid_credentials',
  'authn.tenant_membership_required',
  'authn.account_locked',
  'authn.rate_limited',
  'authn.verification_code_invalid',
  'authn.email_already_registered',
  'authn.phone_already_registered',
  'authn.identifier_required',
  // Identifier canonical-form answers: the register path answers these
  // for an identifier its slot cannot canonicalize (the SMS request
  // answers authn.invalid_phone the same way).
  'authn.invalid_email',
  'authn.invalid_phone',
  'authn.password_too_short',
  'authn.password_too_long',
  'authn.password_too_weak',
  'authn.provider_unknown',
  'authn.redirect_uri_not_allowed',
  'authn.oauth_state_invalid',
  'authn.social_exchange_failed',
  'authn.identity_requires_binding',
  'authn.identity_already_bound',
  // authn: session lifecycle -- a sign-out call can answer with these,
  // and a host renders them for its own protected operations.
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
export function useAuthUiErrorText(): (code: string) => string {
  const { t } = useAuthUiTranslation()
  return (code: string) =>
    t(KNOWN_CODES.has(code) ? `errors.${code}` : 'errors.unknown')
}
