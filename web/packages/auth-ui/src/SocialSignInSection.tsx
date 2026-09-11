/**
 * SocialSignInSection: the social channels of the sign-in surface.
 *
 * One outlined button per configured provider, each answering the
 * bundle's name for it; the divider title above is the section's only
 * non-interactive text. Clicking a provider asks the session for that
 * channel's authorization URL -- a pure request, never a navigation:
 * the URL is reported upward through onAuthorizeUrl and the host decides
 * what it is for (a redirect in the host's router, a popup, a new tab).
 * While one request is in flight its own button disables and the others
 * stay live, each flight tracked per provider: an earlier attempt's
 * return can never re-enable a later provider's button mid-flight. A
 * failed answer (authn.provider_unknown, authn.redirect_uri_not_allowed)
 * renders through the one InlineError banner -- but the banner belongs
 * to the newest attempt only: attempts are numbered at start, a failure
 * writes its code only when it is still the newest attempt, and each
 * new attempt clears the banner, so an earlier attempt's failure can
 * never paint over a newer attempt's success or linger past it. The
 * authorize-URL report to the host runs only after the request verdict
 * settled, and a throwing onAuthorizeUrl is contained: it is not an
 * authorization failure, never renders an error and never escapes as
 * an unhandled rejection.
 */

import { useRef, useState } from 'react'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import Divider from '@mui/material/Divider'
import type { AuthSession } from '@speed/auth-core'
import { useAuthUiTranslation } from './internal/translation.js'
import { errorCodeOf } from '@speed/ui-kit'
import { InlineError } from './internal/inline-error.js'

/** The social sign-in channels the authn spec hosts. */
export type SocialProvider =
  | 'google'
  | 'github'
  | 'wechat'
  | 'dingtalk'
  | 'feishu'

/** One configured channel: which provider, and the redirect URI the
 * host's callback route listens on. */
export interface SocialProviderConfig {
  readonly provider: SocialProvider
  readonly redirectUri: string
}

export interface SocialSignInSectionProps {
  /** The session that builds authorization URLs. */
  readonly session: AuthSession
  /** The channels to render, in the order the host wants. */
  readonly providers: readonly SocialProviderConfig[]
  /**
   * Receives the authorization URL for one channel once it is built;
   * the host navigates. Omit to run the section without a follow-up
   * (a caller that only exercises the request path).
   */
  readonly onAuthorizeUrl?: (provider: SocialProvider, authorizeUrl: string) => void
}

export function SocialSignInSection({
  session,
  providers,
  onAuthorizeUrl,
}: SocialSignInSectionProps) {
  const { t } = useAuthUiTranslation()
  // One busy slot per provider, never a single shared value: two
  // overlapping flights (two provider buttons clicked in quick
  // succession -- each flight's own button is the only one it locks)
  // must each keep their own button locked until their own attempt
  // settles, and one attempt's return must never unlock another's
  // still-pending flight.
  const [busyProviders, setBusyProviders] = useState<
    ReadonlySet<SocialProvider>
  >(new Set())
  const [errorCode, setErrorCode] = useState<string | null>(null)
  // Attempt numbering: the failure banner belongs to the newest attempt
  // (see authorize below), so attemptsRef is the counter that decides.
  const attemptsRef = useRef(0)

  const authorize = async (config: SocialProviderConfig): Promise<void> => {
    const attempt = ++attemptsRef.current
    const provider = config.provider
    // The newest attempt owns the banner: a new attempt clears any
    // failure an older attempt left behind.
    setErrorCode(null)
    setBusyProviders((previous) => new Set(previous).add(provider))
    let authorizeUrl: string | null = null
    try {
      authorizeUrl = await session.socialAuthorizeUrl(provider, {
        redirect_uri: config.redirectUri,
      })
    } catch (error) {
      // A failure renders only while its own attempt is still the
      // newest one: an older attempt settling after a newer attempt
      // started must not paint over the newer attempt's outcome.
      if (attempt === attemptsRef.current) {
        setErrorCode(errorCodeOf(error))
      }
    } finally {
      setBusyProviders((previous) => {
        const next = new Set(previous)
        next.delete(provider)
        return next
      })
    }
    if (authorizeUrl !== null) {
      try {
        onAuthorizeUrl?.(provider, authorizeUrl)
      } catch {
        // A throwing host callback is not an authorization failure: the
        // URL was built, nothing here renders the host's error, and the
        // containment keeps the throw out of this fire-and-forget
        // click handler.
      }
    }
  }

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1, width: '100%' }}>
      <Divider textAlign="center" sx={{ color: 'text.secondary' }}>
        {t('social.title')}
      </Divider>
      {providers.map((config) => (
        <Button
          key={config.provider}
          variant="outlined"
          fullWidth
          disabled={busyProviders.has(config.provider)}
          onClick={() => void authorize(config)}
        >
          {t(`social.provider.${config.provider}`)}
        </Button>
      ))}
      <InlineError code={errorCode} />
    </Box>
  )
}
