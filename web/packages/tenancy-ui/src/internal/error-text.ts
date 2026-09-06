/**
 * Error-code text resolution for the tenancy-ui namespace.
 *
 * The switch operation's reachable answers map, one bundle key per code,
 * under the errors section ('errors.authn.tenant_membership_required'
 * and so on). The switch endpoint itself answers the membership codes --
 * authn.tenant_membership_required (the caller holds no membership in
 * the tenant it asked to enter) and authn.tenant_membership_unavailable
 * (membership cannot be established at all: an unwired MembershipReader
 * fails closed) -- and the account-status code authn.invalid_credentials
 * (go/authn Service.SwitchTenant answers it for an account that is not
 * active; on the switch surface it never means a wrong password, which
 * is why its text here is authored rather than copied from the sign-in
 * surface's). The authn middleware answers the two token-verification
 * codes -- authn.authentication_required (no credential presented) and
 * authn.token_invalid (the presented access token did not verify) --
 * on the protected switch route. The session-lifecycle codes (a refused
 * refresh or a dead session surfaces as one of these through the
 * silent-refresh leg, and TenantSwitcher must render the session's
 * answer rather than an invented one), plus the transport-level
 * client.network / client.timeout / client.protocol codes of the
 * @speed/api-client contract, complete the list. Codes outside the
 * whitelist -- a future authn code, a client.http.<status> answer, a
 * non-ApiError throw -- resolve to 'errors.unknown', so the bundle can
 * never render a raw key and a missing translation never leaks another
 * language's text or an English fallback.
 *
 * Where the switch answer and the sign-in surface's answer share one
 * meaning, the texts are the auth-ui error texts for the same codes,
 * copied verbatim: same-tier packages cannot import one another's
 * catalogs, so this errors section is a deliberate duplicate of the
 * reachable subset (see resources.ts) -- a duplicate the error-text
 * suite pins to its source, importing the auth-ui bundles as test data
 * so the copies cannot drift. Codes the switch surface draws with a
 * meaning of its own -- authn.invalid_credentials (see above) -- or
 * that the pre-auth sign-in surface cannot draw at all --
 * authn.authentication_required and authn.token_invalid -- ship
 * authored texts, recorded in the suite's SWITCH_AUTHORED_TEXTS.
 */

import { useTenancyUiTranslation } from './translation.js'

/**
 * Every code with a dedicated errors-section text. Exported so the suite
 * can pin the whitelist-to-bundle pairing in both languages: a code
 * added here without its two bundle keys (or vice versa) fails the
 * pairing test beside inline-error.tsx.
 */
export const ERROR_TEXT_CODES = [
  // authn: the switch endpoint's own answers. Membership in the target
  // tenant, membership that cannot be established at all (an unwired
  // MembershipReader fails closed), and the account-status refusal
  // (Service.SwitchTenant answers authn.invalid_credentials for an
  // account that is not active -- never for a wrong password on this
  // surface).
  'authn.tenant_membership_required',
  'authn.tenant_membership_unavailable',
  'authn.invalid_credentials',
  // authn: the middleware's token-verification answers on the protected
  // switch route -- no credential presented, an access token that did
  // not verify. The pre-auth sign-in surface cannot be answered with
  // either, so these texts are authored, not copied.
  'authn.authentication_required',
  'authn.token_invalid',
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
