/**
 * BindingCallbackHandler: completes a social-account binding at the
 * host's callback route.
 *
 * The host renders this handler with the provider and the code/state the
 * provider redirected back after its authorize step; an effect -- a
 * keyed exchange over the (code, state) pair, the same mechanism
 * auth-ui's sign-in callback uses, so the double effect invocation of
 * StrictMode development starts exactly one exchange -- posts the pair
 * to the callback endpoint of the authn spec's social surface (the same
 * operation the add area's authorize URLs feed). The exchange is a
 * plain generated call, never a session operation: binding adds an
 * identity to the caller's own account, it does not sign anyone in.
 *
 * The answer shape dispatches the outcome. A binding-shaped answer (no
 * tokens) invalidates the identities list query -- the bound row
 * appears for any observer of that cache -- and fires onBound, the
 * host's cue to navigate back to the account surface. A login-shaped
 * answer (tokens present: the caller's sign-in had died and the
 * exchange turned into a login, signing some account in) renders the
 * dedicated signed-elsewhere panel and fires nothing: the code is
 * consumed, the other account is signed in, and this handler must not
 * tell the account surface anything happened. A failed exchange
 * (authn.oauth_state_invalid, authn.social_exchange_failed,
 * authn.identity_requires_binding) renders its code text in the
 * InlineError banner under a retry button that re-runs the exchange for
 * the same pair. Nothing here navigates: the props come from the host's
 * own route and the callbacks are the host's.
 */

import { useEffect, useRef, useState } from 'react'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import CircularProgress from '@mui/material/CircularProgress'
import Typography from '@mui/material/Typography'
import { useQueryClient } from '@tanstack/react-query'
import {
  authnSocialCallback,
  getAuthnListIdentitiesQueryKey,
} from '@speed/api-sdk'
import { useAccountUiTranslation } from './internal/translation.js'
import { InlineError, errorCodeOf } from './internal/inline-error.js'
import type { SocialProvider } from './SocialBindingsSection.js'

export interface BindingCallbackHandlerProps {
  /**
   * The channel the redirect came back on, a path segment of the
   * callback endpoint, threaded through to the exchange verbatim. The
   * plan's original prop table omitted it (the exchange reads the
   * provider from the URL); the spec's callback endpoint is per-provider
   * segments, so the channel must travel as a prop -- see the round
   * report.
   */
  readonly provider: SocialProvider
  /** The authorization response the provider redirected back. */
  readonly code: string
  /** The state the authorize URL carried; the server validates it. */
  readonly state: string
  /**
   * Fired once after a binding-shaped answer commits (and the
   * identities list refetched); the host navigates back to the account
   * surface. A handler without one still completes the exchange and
   * refetches the list -- the bound row lands for any observer -- and
   * stays on the pending notice until the host reacts.
   */
  readonly onBound?: () => void
}

/**
 * The handler's local outcomes: pending (the notice the exchange is
 * running -- and, on a completed exchange whose host has not reacted
 * yet, the state it rests in), failed (code text under a same-pair
 * retry) and signed-elsewhere (the exchange signed a session in; the
 * dedicated panel).
 */
type Status = 'pending' | 'failed' | 'signed-elsewhere'

export function BindingCallbackHandler({
  provider,
  code,
  state,
  onBound,
}: BindingCallbackHandlerProps) {
  const { t } = useAccountUiTranslation()
  const queryClient = useQueryClient()
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
    setStatus('pending')
    setErrorCode(null)
    void (async () => {
      try {
        const answer = await authnSocialCallback(provider, { code, state })
        if (run !== runRef.current) {
          return
        }
        if (answer.tokens !== undefined) {
          // A login-shaped answer: the caller's sign-in had died and
          // the exchange signed the provider's account in. The binding
          // did not happen and onBound must not fire.
          setStatus('signed-elsewhere')
          return
        }
        // A binding-shaped answer: the identity is bound to the
        // caller's account. Refetch the identities list so the bound
        // row lands, then cue the host.
        await queryClient.invalidateQueries({
          queryKey: getAuthnListIdentitiesQueryKey(),
        })
        if (run === runRef.current) {
          onBound?.()
        }
      } catch (error) {
        if (run === runRef.current) {
          setErrorCode(errorCodeOf(error))
          setStatus('failed')
        }
      }
    })()
  }, [pair, code, state, provider, attempt, queryClient, onBound])

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
          {t('bindingCallback.retry')}
        </Button>
      </Box>
    )
  }

  if (status === 'signed-elsewhere') {
    return (
      <Box
        sx={{
          display: 'flex',
          flexDirection: 'column',
          alignItems: 'flex-start',
          gap: 1,
          width: '100%',
        }}
      >
        <Typography variant="body1" sx={{ fontWeight: 500 }}>
          {t('bindingCallback.signedInElsewhere.title')}
        </Typography>
        <Typography variant="body2" color="text.secondary">
          {t('bindingCallback.signedInElsewhere.description')}
        </Typography>
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
      <Typography>{t('bindingCallback.pending')}</Typography>
    </Box>
  )
}
