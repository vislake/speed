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
 * surface's). The two token-verification codes --
 * authn.authentication_required (no credential presented) and
 * authn.token_invalid (the presented access token did not verify) --
 * are the answers of authn's per-operation and per-route guards, not of
 * the composed authn.Middleware: the middleware is optional
 * authentication, passing a credential-less request through anonymous
 * (it never writes authentication_required -- RequireAuthenticated in
 * go/authn/middleware.go and the handler's requirePrincipal do -- and
 * 401s a presented token that fails verification with
 * authn.token_invalid). A host whose guard mounts behind a tenant gate
 * refuses the credential-less request with tenancy.tenant_unresolved
 * before any authn guard runs (the reference app's switch route does);
 * the whitelist keeps both codes so a host whose guard does answer
 * renders text, never a raw key. The session-lifecycle codes (a refused
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

import {
  CLIENT_TRANSPORT_ERROR_CODES,
  SESSION_LIFECYCLE_ERROR_CODES,
  createErrorTextResolver,
} from '@speed/i18n'
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
  // authn: the token-verification answers of authn's per-operation and
  // per-route guards -- no credential presented (authentication_required,
  // written by RequireAuthenticated and the handler's requirePrincipal;
  // the composed optional authn.Middleware passes credential-less
  // requests through anonymous), an access token that did not verify
  // (token_invalid, which that middleware 401s for a presented-but-
  // invalid token). The pre-auth sign-in surface cannot be answered
  // with either, so these texts are authored, not copied.
  'authn.authentication_required',
  'authn.token_invalid',
  // The shared families (see @speed/i18n): a switch whose session dies
  // surfaces as a session-lifecycle code (refused refresh, revoked
  // session), and every caller here talks through the api-client's
  // transport.
  ...SESSION_LIFECYCLE_ERROR_CODES,
  ...CLIENT_TRANSPORT_ERROR_CODES,
] as const

const KNOWN_CODES = new Set<string>(ERROR_TEXT_CODES)

/**
 * The code-to-text resolver hook: call with an ApiError code and get the
 * current-language text ('errors.unknown' for anything unlisted).
 */
export function useTenancyUiErrorText(): (code: string) => string {
  const { t } = useTenancyUiTranslation()
  return createErrorTextResolver(t, KNOWN_CODES)
}
