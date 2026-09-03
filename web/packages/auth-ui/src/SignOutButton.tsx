/**
 * SignOutButton: the sign-out action over the session's logout operation.
 *
 * A click drives session.logout(); while the request is in flight the
 * button is disabled and shows the signOut.busy label; a failed logout
 * renders the answer's code text (session-lifecycle codes like
 * authn.session_revoked included) in one InlineError and leaves the
 * button ready to retry. A successful logout is deliberately quiet: it
 * clears the session store and flips the snapshot to anonymous, and the
 * host's own auth-core hooks observe that flip -- the component never
 * navigates and fires no completion callback.
 *
 * The auth-core contract behind the retry story: a failed logout leaves
 * the local session exactly as it was (a raw ApiError rejection, zero
 * state change), so a transport failure needs a manual retry while a
 * server-side session death converges through the refresh leg --
 * refresh() resolving false signs the session out locally. Neither is a
 * gap this component can close.
 *
 * Nothing here reads the session state: the button renders whether the
 * snapshot is authenticated or not, and the host mounts it where a
 * sign-out action belongs (typically app chrome).
 */

import { useState } from 'react'
import Button from '@mui/material/Button'
import type { AuthSession } from '@speed/auth-core'
import { useAuthUiTranslation } from './internal/translation.js'
import { InlineError, errorCodeOf } from './internal/inline-error.js'

export interface SignOutButtonProps {
  /** The session the sign-out drives. */
  readonly session: AuthSession
}

export function SignOutButton({ session }: SignOutButtonProps) {
  const { t } = useAuthUiTranslation()
  const [pending, setPending] = useState(false)
  const [errorCode, setErrorCode] = useState<string | null>(null)

  const handleClick = async (): Promise<void> => {
    setErrorCode(null)
    setPending(true)
    try {
      await session.logout()
      // Success is the host's to observe: the snapshot flipped anonymous
      // and the host unmounts this button; if it does not, the finally
      // below re-enables it for the next click (logout is idempotent
      // server-side -- a second call answers 204 too).
    } catch (error) {
      setErrorCode(errorCodeOf(error))
    } finally {
      setPending(false)
    }
  }

  return (
    <>
      <Button type="button" disabled={pending} onClick={() => void handleClick()}>
        {t(pending ? 'signOut.busy' : 'signOut.label')}
      </Button>
      <InlineError code={errorCode} />
    </>
  )
}
