/**
 * The account-ui namespace translation hook, bound once so component
 * families do not repeat the namespace string.
 *
 * Components take their text from this namespace's bundles and never
 * hardcode user-facing strings; hosts override wording through the
 * namespace registration, not through component props. The surfaces
 * compose ui-kit components whose built-in strings speak ui-kit-namespace
 * keys, which ui-kit resolves at render time -- a host rendering
 * account-ui registers both namespaces.
 */

import { useTranslation } from '@speed/i18n'
import { ACCOUNT_UI_NAMESPACE } from '../resources.js'

/** The translation response (t plus re-render binding) for the account-ui namespace. */
export function useAccountUiTranslation() {
  return useTranslation(ACCOUNT_UI_NAMESPACE)
}
