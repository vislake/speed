/**
 * The billing-ui namespace translation hook, bound once so component
 * families do not repeat the namespace string.
 *
 * Components take their text from this namespace's bundles and never
 * hardcode user-facing strings; hosts override wording through the
 * namespace registration, not through component props. The surface
 * composes ui-kit components whose built-in strings speak
 * ui-kit-namespace keys (the EmptyState texts), which ui-kit resolves
 * at render time -- a host rendering billing-ui registers both
 * namespaces.
 */

import { useTranslation } from '@speed/i18n'
import { BILLING_UI_NAMESPACE } from '../resources.js'

/** The translation response (t plus re-render binding) for the billing-ui namespace. */
export function useBillingUiTranslation() {
  return useTranslation(BILLING_UI_NAMESPACE)
}
