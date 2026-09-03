/**
 * The tenancy-ui namespace translation hook, bound once so component
 * families do not repeat the namespace string.
 *
 * TenantSwitcher takes its text from this namespace's bundles and never
 * hardcodes user-facing strings; hosts override wording through the
 * namespace registration, not through component props.
 */

import { useTranslation } from '@speed/i18n'
import { TENANCY_UI_NAMESPACE } from '../resources.js'

/** The translation response (t plus re-render binding) for the tenancy-ui namespace. */
export function useTenancyUiTranslation() {
  return useTranslation(TENANCY_UI_NAMESPACE)
}
