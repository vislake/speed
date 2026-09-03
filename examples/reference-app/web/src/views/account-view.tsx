/**
 * account-view.tsx -- the account surface slot of the app frame. This
 * file is the interim placeholder the frame composes while the
 * surface's own commit lands (the account-ui sections over the
 * generated API and the social-binding callback subroute): until then
 * the nav target renders the ui-kit EmptyState speaking the app
 * namespace, so the frame's routes and the placeholder never hardcode
 * a word.
 */

import type { ReactElement } from 'react'
import { EmptyState } from '@speed/ui-kit'
import { useTranslation } from '@speed/i18n'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'

/** The interim account surface: an EmptyState until the surface commit. */
export function AccountView(): ReactElement {
  const { t } = useTranslation(REFERENCE_APP_NAMESPACE)
  return (
    <EmptyState
      title={t('placeholder.accountTitle')}
      description={t('placeholder.accountDescription')}
    />
  )
}
