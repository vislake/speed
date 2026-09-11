/**
 * Error-code text resolution for the billing-ui namespace.
 *
 * Every reachable answer of the billing read surface maps, one bundle
 * key per code, under the errors section ('errors.billing.invoice_not_found'
 * and so on). The surface is two reads of the caller's tenant's
 * invoices (billing_listInvoices, billing_getInvoice), so its own
 * module answers are the read codes: billing.invoice_not_found is
 * billing_getInvoice's 404 (an id that names no invoice of the
 * caller's tenant -- never created, or another tenant's; the server
 * answers the same code for both, so the copy says nothing about
 * whether the id exists at all). The billing 400 pair
 * (billing.invalid_limit, billing.invalid_request) is unreachable from
 * this package by construction -- the list hook always sends the frozen
 * in-range limit -- and 500 envelopes (billing.internal_error) render
 * the unknown fallback like every internal-error answer. A read of a
 * signed-in surface is answered by the authn/tenancy chain when the
 * caller's session dies mid-flight, so the session-lifecycle family
 * every protected speed surface whitelists is whitelisted here too,
 * plus the transport-level client.network / client.timeout /
 * client.protocol codes of the @speed/api-client contract. Codes
 * outside the whitelist -- a future billing code, a client.http.<status>
 * answer, a non-ApiError throw -- resolve to 'errors.unknown', so the
 * bundle can never render a raw key and a missing translation never
 * leaks another language's text or an English fallback.
 *
 * Wording policy: every code whose answer text this package shares
 * with the sign-in family (the five session-lifecycle codes, the three
 * client.* transport codes and the errors.unknown fallback) is a
 * verbatim copy of the auth-ui bundle's text, so the same server
 * answer reads the same on every surface. Same-tier packages never
 * import one another's catalogs, so those leaves are deliberate
 * duplicates of the sign-in bundle's own -- a duplication the
 * error-text suite pins to its source, importing the auth-ui bundles
 * as test data, so the copies cannot drift.
 */

import {
  CLIENT_TRANSPORT_ERROR_CODES,
  SESSION_LIFECYCLE_ERROR_CODES,
  createErrorTextResolver,
} from '@speed/i18n'
import { useBillingUiTranslation } from './translation.js'

/**
 * Every code with a dedicated errors-section text. Exported so the suite
 * can pin the whitelist-to-bundle pairing in both languages: a code
 * added here without its two bundle keys (or vice versa) fails the
 * pairing test beside inline-error.tsx.
 */
export const ERROR_TEXT_CODES = [
  // billing: the invoice detail read's own 404. The id came from this
  // package's list or from the host; an id that names no invoice of
  // the caller's tenant answers it.
  'billing.invoice_not_found',
  // The shared families (see @speed/i18n): a read of a signed-in
  // surface is answered with the session-lifecycle codes when the
  // caller's session dies mid-flight (the api-client 401-refresh leg
  // surfaces them when its refresh cannot repair the request), and
  // every caller here talks through the api-client's transport.
  ...SESSION_LIFECYCLE_ERROR_CODES,
  ...CLIENT_TRANSPORT_ERROR_CODES,
] as const

const KNOWN_CODES = new Set<string>(ERROR_TEXT_CODES)

/**
 * The code-to-text resolver hook: call with an ApiError code and get the
 * current-language text ('errors.unknown' for anything unlisted).
 */
export function useBillingUiErrorText(): (code: string) => string {
  const { t } = useBillingUiTranslation()
  return createErrorTextResolver(t, KNOWN_CODES)
}
