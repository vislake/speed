/**
 * The auth-ui namespace translation hook, bound once so component
 * families do not repeat the namespace string.
 *
 * Components take their text from this namespace's bundles and never
 * hardcode user-facing strings; hosts override wording through the
 * namespace registration, not through component props. The react-hook-form
 * field rules of the form family speak ui-kit-namespace keys
 * ('form.required'), which ui-kit's FormField resolves at render time --
 * a host rendering auth-ui forms registers both namespaces.
 */

import { useTranslation } from '@speed/i18n'
import { AUTH_UI_NAMESPACE } from '../resources.js'

/** The translation response (t plus re-render binding) for the auth-ui namespace. */
export function useAuthUiTranslation() {
  return useTranslation(AUTH_UI_NAMESPACE)
}
