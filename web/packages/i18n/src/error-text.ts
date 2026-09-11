/**
 * The error-code text convention every error surface resolves through.
 *
 * Failures arrive as codes -- the API envelope's code, the api-client
 * transport codes, a classifier's collapse target -- and text resolves
 * from the code, never from an API's English fallback message (which
 * exists for log triage only). The convention: a listed code resolves
 * to its own leaf under the bundle's `errors` section
 * ('errors.<code>' -- the section the generated platform bundle and the
 * package catalogs share), and anything unlisted to `errors.unknown`.
 * A resolver built here can therefore never render a raw key, and a
 * missing translation never leaks another language's text.
 *
 * Two code families are part of the convention because every protected
 * surface can be answered with them: the session-lifecycle codes (a
 * session that dies mid-flight surfaces as one of these through the
 * refresh leg) and the transport-level codes of the @speed/api-client
 * contract. Each surface's whitelist composes these with the codes its
 * own operations reach and passes the union to the resolver factory.
 */

/**
 * The session-lifecycle answers any signed-in surface can receive: a
 * refused refresh or a dead session surfaces as one of these, and a
 * host renders them for its own protected operations.
 */
export const SESSION_LIFECYCLE_ERROR_CODES = [
  'authn.session_not_found',
  'authn.session_revoked',
  'authn.token_expired',
  'authn.refresh_token_invalid',
  'authn.refresh_token_reused',
] as const

/** The transport-level failures of the @speed/api-client contract. */
export const CLIENT_TRANSPORT_ERROR_CODES = [
  'client.network',
  'client.timeout',
  'client.protocol',
] as const

/** The bundle section error codes resolve under, one dotted code per leaf. */
const ERROR_TEXT_SECTION = 'errors'

/** The leaf every unlisted code resolves to -- the fallback itself. */
const UNKNOWN_ERROR_TEXT_KEY = `${ERROR_TEXT_SECTION}.unknown`

/**
 * Builds a code-to-text resolver over one surface's whitelist: a listed
 * code resolves to its own leaf, anything else to the shared unknown
 * fallback. `t` is the surface namespace's translation function;
 * `knownCodes` is the whitelist membership.
 */
export function createErrorTextResolver(
  t: (key: string) => string,
  knownCodes: ReadonlySet<string>,
): (code: string) => string {
  return (code) =>
    t(knownCodes.has(code) ? `${ERROR_TEXT_SECTION}.${code}` : UNKNOWN_ERROR_TEXT_KEY)
}
