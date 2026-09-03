/**
 * Error-code text resolution for the account-ui namespace.
 *
 * Every reachable answer of the signed-in account surface -- the authn
 * error codes the session-revocation, social-binding and step-up MFA
 * operations answer with (the session and login-history list being a
 * read of the caller's own sessions answers session-lifecycle codes when
 * one of them dies mid-flight; the binding endpoints answer the
 * identity-domain and OAuth-flow codes; the MFA endpoints answer the
 * step-up and enrollment codes; and the rate-limiter can answer
 * authn.rate_limited on any of them), plus the transport-level
 * client.network / client.timeout / client.protocol codes of the
 * @speed/api-client contract -- maps, one bundle key per code, under the
 * errors section ('errors.authn.session_revoked' and so on). Codes
 * outside the whitelist -- a future authn code, a client.http.<status>
 * answer, a non-ApiError throw -- resolve to 'errors.unknown', so the
 * bundle can never render a raw key and a missing translation never
 * leaks another language's text or an English fallback.
 *
 * Wording policy: the seven codes whose failure context is identical to
 * the sign-in surface's (the session-lifecycle family, authn.rate_limited
 * and authn.identity_already_bound) reuse the auth-ui bundle's text
 * verbatim, so the same server answer reads the same on both surfaces.
 */

import { useAccountUiTranslation } from './translation.js'

/**
 * Every code with a dedicated errors-section text. Exported so the suite
 * can pin the whitelist-to-bundle pairing in both languages: a code
 * added here without its two bundle keys (or vice versa) fails the
 * pairing test beside inline-error.tsx.
 */
export const ERROR_TEXT_CODES = [
  // authn: session lifecycle -- the session list's revoke operation and
  // the login-history surface can answer with these, and a host renders
  // them for its own protected operations.
  'authn.session_not_found',
  'authn.session_revoked',
  'authn.token_expired',
  'authn.refresh_token_invalid',
  'authn.refresh_token_reused',
  // authn: shared rate limiter -- any operation behind it.
  'authn.rate_limited',
  // authn: social bindings -- the bind/unbind endpoints answer these.
  'authn.identity_already_bound',
  'authn.identity_not_found',
  'authn.oauth_state_invalid',
  'authn.social_exchange_failed',
  'authn.provider_unknown',
  'authn.redirect_uri_not_allowed',
  // The unbind guard: an account cannot shed its last login method.
  'authn.last_login_method',
  // authn: step-up-gated two-factor setup.
  'authn.step_up_required',
  'authn.mfa_not_enrolled',
  'authn.mfa_already_enrolled',
  'authn.mfa_invalid_code',
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
export function useAccountUiErrorText(): (code: string) => string {
  const { t } = useAccountUiTranslation()
  return (code: string) =>
    t(KNOWN_CODES.has(code) ? `errors.${code}` : 'errors.unknown')
}
