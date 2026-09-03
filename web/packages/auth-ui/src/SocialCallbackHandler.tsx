/**
 * SocialCallbackHandler: completes a social sign-in at the host's
 * callback route.
 *
 * The host renders this handler with the provider and the code/state the
 * provider redirected back; an effect -- ref-keyed on the (code, state)
 * pair, so the double effect invocation of StrictMode development starts
 * exactly one exchange -- completes the login through the session and
 * fires onSignedIn once. A mount that finds the session already
 * authenticated (a re-entry to the callback URL after a completed
 * exchange) starts no exchange -- the single-use code is consumed -- and
 * fires onSignedIn again so the host navigates the viewer onward. While
 * the exchange is in flight the handler shows the pending notice; a
 * failed exchange (authn.oauth_state_invalid,
 * authn.social_exchange_failed, authn.identity_requires_binding) renders
 * its code text in the InlineError banner under a retry button that
 * re-runs the exchange for the same pair. Nothing here navigates: the
 * props come from the host's own route and the success callback is the
 * host's.
 */

import { useEffect, useRef, useState } from 'react'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import CircularProgress from '@mui/material/CircularProgress'
import Typography from '@mui/material/Typography'
import type { AuthSession } from '@speed/auth-core'
import type { SocialProvider } from './SocialSignInSection.js'
import { useAuthUiTranslation } from './internal/translation.js'
import { InlineError, errorCodeOf } from './internal/inline-error.js'

export interface SocialCallbackHandlerProps {
  /** The session that completes the exchange. */
  readonly session: AuthSession
  /** The channel the redirect came back on, a path segment of the
   * callback endpoint, threaded through to the exchange verbatim. */
  readonly provider: SocialProvider
  /** The authorization response the provider redirected back. */
  readonly code: string
  /** The state the authorize URL carried; the server validates it. */
  readonly state: string
  /** Fired once after the exchange commits -- and again on a later
   * mount that finds the session already authenticated, the re-entry
   * case where no exchange is started; the host navigates. */
  readonly onSignedIn?: () => void
}

type Status = 'pending' | 'failed'

export function SocialCallbackHandler({
  session,
  provider,
  code,
  state,
  onSignedIn,
}: SocialCallbackHandlerProps) {
  const { t } = useAuthUiTranslation()
  // One exchange per (code, state) pair: the ref survives the
  // mount/unmount/mount cycle of StrictMode's double effect invocation,
  // so the second run sees the pair already handled and starts nothing.
  const handledPairRef = useRef<string | null>(null)
  const [status, setStatus] = useState<Status>('pending')
  const [attempt, setAttempt] = useState(0)
  const [errorCode, setErrorCode] = useState<string | null>(null)

  // The NUL joins the two values so a (code, state) split cannot
  // collide with another pair's concatenation.
  const pair = `${code}\u0000${state}`

  // A running exchange is not cancelled on cleanup: StrictMode's
  // mount/effect/cleanup/effect cycle would cancel the only exchange
  // (the ref guard skips the second start), so the first exchange must
  // be allowed to finish. Instead each start takes a run token and a
  // resolving exchange only acts when its token is still current -- a
  // stale exchange from a superseded pair cannot paint over the active
  // one. The attempt counter re-arms the effect for a retry after the
  // ref guard is lifted.
  const runRef = useRef(0)
  useEffect(() => {
    if (handledPairRef.current === pair) {
      return
    }
    handledPairRef.current = pair
    const run = ++runRef.current
    // A mount that finds the session already authenticated is a re-entry
    // to the callback route after a completed exchange: the code is
    // single-use server-side, so a second exchange could only fail and
    // paint a failure over a session that is actually live. Start no
    // exchange; fire onSignedIn again and keep the pending notice up
    // until the host reacts (navigating onward, or gating on its own
    // authenticated snapshot).
    if (session.getSnapshot().state === 'authenticated') {
      onSignedIn?.()
      return
    }
    setStatus('pending')
    setErrorCode(null)
    void (async () => {
      try {
        await session.completeSocialLogin(provider, { code, state })
        if (run === runRef.current) {
          onSignedIn?.()
        }
      } catch (error) {
        if (run === runRef.current) {
          setErrorCode(errorCodeOf(error))
          setStatus('failed')
        }
      }
    })()
  }, [pair, code, state, provider, session, attempt])

  const retry = (): void => {
    handledPairRef.current = null
    setStatus('pending')
    setAttempt((value) => value + 1)
  }

  if (status === 'failed') {
    return (
      <Box
        sx={{
          display: 'flex',
          flexDirection: 'column',
          alignItems: 'flex-start',
          gap: 1.5,
          width: '100%',
        }}
      >
        <InlineError code={errorCode} />
        <Button variant="outlined" onClick={retry}>
          {t('socialCallback.retry')}
        </Button>
      </Box>
    )
  }

  return (
    <Box
      role="status"
      sx={{ display: 'flex', alignItems: 'center', gap: 1.5, width: '100%' }}
    >
      {/* Decorative: the pending text on its own announces the state from
          the role=status container; naming the spinner too would read the
          notice twice. */}
      <CircularProgress size={18} aria-hidden={true} />
      <Typography>{t('socialCallback.pending')}</Typography>
    </Box>
  )
}
