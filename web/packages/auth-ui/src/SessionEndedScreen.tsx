/**
 * SessionEndedScreen: the "session ended" placeholder a host renders
 * where protected content used to be.
 *
 * The session lifecycle is observable, not imperative: there is no
 * recovery and no refresh cookie, so a page reload starts anonymous, and
 * a server-side session death converges through the api-client refresh
 * leg -- refresh() resolving false flips the snapshot to anonymous. The
 * host watches its own auth-core hooks and, at any view that just lost
 * its authenticated snapshot, mounts this screen; its action hands the
 * host back to its sign-in surface via onSignIn.
 *
 * Pure presentation: no session prop, no hooks, no network. The strings
 * all come from the auth-ui namespace, overridden onto ui-kit's
 * EmptyState -- the noPermission variant's lock icon, because the
 * content is gated again until the user signs in; nothing of ui-kit's
 * built-in texts can leak through (every text slot is overridden).
 */

import Button from '@mui/material/Button'
import { EmptyState } from '@speed/ui-kit'
import { useAuthUiTranslation } from './internal/translation.js'

export interface SessionEndedScreenProps {
  /** Fired when the viewer asks to sign in again; the host navigates. */
  readonly onSignIn: () => void
}

export function SessionEndedScreen({ onSignIn }: SessionEndedScreenProps) {
  const { t } = useAuthUiTranslation()
  return (
    <EmptyState
      variant="noPermission"
      title={t('sessionEnded.title')}
      description={t('sessionEnded.description')}
      action={
        <Button variant="contained" onClick={onSignIn}>
          {t('sessionEnded.signInAction')}
        </Button>
      }
    />
  )
}
