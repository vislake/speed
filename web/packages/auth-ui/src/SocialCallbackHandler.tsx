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
 * re-runs the exchange for the same pair. The exchange verdict settles
 * before the host callback runs, and a throwing onSignedIn is
 * contained: it is not an exchange failure (the exchange
 * committed), so it never flips the handler to the failed state -- which
 * would offer a retry of an already-consumed single-use code -- and
 * never escapes as an unhandled rejection; the success outcome (the
 * pending notice and the authenticated session) stays in place.
 * Nothing here navigates: the props come from the host's own route and
 * the success callback is the host's.
 *
 * An exchange that lost a concurrent sign-in race (a sibling login
 * committed to the session while this exchange was in flight) answers
 * with auth-core's OperationSupersededError, not a failure: the
 * session is authenticated now -- under the winner's identity, which
 * is not necessarily this exchange's -- so the handler fires no
 * onSignedIn (a one-shot side effect must not fire for an identity
 * the session is not running under; the winning call fires its own)
 * and never flips to the failed state, whose retry would re-submit an
 * already-consumed single-use code. The pending notice stays up and
 * the host's own snapshot hooks observe the winner's session; a
 * re-entry to the callback route with the session authenticated takes
 * the ordinary already-signed-in path above.
 */

import { useEffect, useRef, useState } from 'react'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import CircularProgress from '@mui/material/CircularProgress'
import Typography from '@mui/material/Typography'
import type { AuthSession } from '@speed/auth-core'
import { isOperationSuperseded } from '@speed/auth-core'
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
  // The pending live region must never mount together with its text: a
  // role="status" region announces only content changes that follow its
  // own insertion, so the pending text born in the same commit as the
  // region -- the initial mount, or a retry's re-entry into the pending
  // branch -- would be silent. The text below is therefore gated on
  // `pendingCommitted`, which lags the pending phase by one commit (the
  // effect after the phase's first commit arms it, leaving the phase
  // disarms it): the region's first committed frame is empty whatever
  // transition entered the phase, and the text fills an existing region
  // a commit later, which is what makes the announcement fire. The same
  // node survives the transition, so the later text change is an
  // announcement rather than another mount.
  const pending = status === 'pending'
  const [pendingCommitted, setPendingCommitted] = useState(false)
  useEffect(() => {
    setPendingCommitted(pending)
  }, [pending])
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
      } catch (error) {
        if (isOperationSuperseded(error)) {
          // This exchange lost a concurrent sign-in race (see the file
          // header): no failed state -- whose retry would re-submit the
          // already-consumed single-use code -- and no onSignedIn, which
          // the winning call fires for the identity the session is
          // actually running under. The pending notice stays up.
          return
        }
        if (run === runRef.current) {
          setErrorCode(errorCodeOf(error))
          setStatus('failed')
        }
        return
      }
      if (run === runRef.current) {
        try {
          onSignedIn?.()
        } catch {
          // A throwing host callback is not an exchange failure: the
          // exchange verdict (success) settled first, so nothing here
          // flips to the failed state -- which would offer a retry of
          // the already-consumed code -- and the containment keeps the
          // throw out of this fire-and-forget promise.
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
          notice twice. The text renders only once `pendingCommitted`
          arms it, one commit after this region mounts (see the comment
          on the state above). */}
      <CircularProgress size={18} aria-hidden={true} />
      <Typography>
        {pendingCommitted ? t('socialCallback.pending') : ''}
      </Typography>
    </Box>
  )
}
