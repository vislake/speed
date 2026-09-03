/**
 * notes-view.tsx -- the notes surface slot of the app frame. This file
 * is the interim placeholder the frame composes while the surface's own
 * commit lands (the notes list over the generated API, the create flow,
 * the RouteGuard wiring): until then the nav target renders the ui-kit
 * EmptyState speaking the app namespace, so the frame's routes and the
 * placeholder never hardcode a word.
 */

import type { ReactElement } from 'react'
import { EmptyState } from '@speed/ui-kit'
import { useTranslation } from '@speed/i18n'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'

/** The interim notes surface: an EmptyState until the surface commit. */
export function NotesView(): ReactElement {
  const { t } = useTranslation(REFERENCE_APP_NAMESPACE)
  return (
    <EmptyState
      title={t('placeholder.notesTitle')}
      description={t('placeholder.notesDescription')}
    />
  )
}
