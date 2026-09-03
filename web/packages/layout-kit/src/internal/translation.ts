/**
 * The layout-kit namespace translation hook, bound once so component
 * families do not repeat the namespace string.
 *
 * Components take their text from this namespace's bundles and never
 * hardcode user-facing strings; hosts override wording through the
 * namespace registration, not through component props.
 */

import { useTranslation } from '@speed/i18n'
import { LAYOUT_KIT_NAMESPACE } from '../resources.js'

/** The translation response (t plus re-render binding) for the layout-kit namespace. */
export function useLayoutKitTranslation() {
  return useTranslation(LAYOUT_KIT_NAMESPACE)
}
