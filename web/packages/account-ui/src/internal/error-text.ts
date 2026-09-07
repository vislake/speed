/**
 * Error-code text resolution for the account-ui namespace.
 *
 * Every reachable answer of the signed-in account surface -- the authn
 * error codes the session-revocation, social-binding and step-up MFA
 * operations answer with (the session and login-history list being a
 * read of the caller's own sessions answers session-lifecycle codes when
 * one of them dies mid-flight; the binding endpoints answer the
 * identity-domain and OAuth-flow codes, and the add area's authorize
 * request answers authn.channel_disabled when a deployment turned the
 * offered channel off; the MFA endpoints answer the
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
 * Wording policy: the eight codes whose failure context is identical to
 * the sign-in surface's (the session-lifecycle family, authn.rate_limited,
 * authn.identity_already_bound and authn.identity_requires_binding) reuse
 * the auth-ui bundle's text verbatim, so the same server answer reads the
 * same on both surfaces. Same-tier packages never import one another's
 * catalogs, so those eight leaves are deliberate duplicates of the sign-in
 * bundle's own -- a duplication the error-text suite pins to its source,
 * importing the auth-ui bundles as test data, so the copies cannot drift.
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
  // authn: social bindings -- the add area's authorize request passes
  // the same channel gate a sign-in authorize does: a known channel a
  // deployment turned off answers authn.channel_disabled before any
  // state value is minted, and the flag values are public (the pre-auth
  // features endpoint serves them), so the refusal discloses nothing a
  // caller could not already read. The bind/unbind endpoints answer the
  // identity-domain and OAuth-flow codes below, and the callback
  // exchange answers identity_requires_binding when the external
  // identity's verified email already belongs to another account whose
  // auto-link conditions were not met.
  'authn.channel_disabled',
  'authn.identity_already_bound',
  'authn.identity_requires_binding',
  'authn.identity_not_found',
  'authn.oauth_state_invalid',
  'authn.social_exchange_failed',
  'authn.provider_unknown',
  'authn.redirect_uri_not_allowed',
  // The unbind guard: an account cannot shed its last login method.
  'authn.last_login_method',
  // authn: step-up-gated two-factor setup. The step-up challenge answers
  // split spent codes from wrong ones server-side: mfa_invalid_code stays
  // the retryable wrong/never-valid answer, mfa_code_used names a code
  // that was valid and is gone -- the surface renders it (never as
  // "invalid", never as retryable-with-the-same-code) so a user who
  // re-submits a consumed code is told it is consumed.
  'authn.step_up_required',
  'authn.mfa_not_enrolled',
  'authn.mfa_already_enrolled',
  'authn.mfa_invalid_code',
  'authn.mfa_code_used',
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
