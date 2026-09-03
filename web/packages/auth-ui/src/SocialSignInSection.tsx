/**
 * SocialSignInSection: the social channels of the sign-in surface.
 *
 * One outlined button per configured provider, each answering the
 * bundle's name for it; the divider title above is the section's only
 * non-interactive text. Clicking a provider asks the session for that
 * channel's authorization URL -- a pure request, never a navigation:
 * the URL is reported upward through onAuthorizeUrl and the host decides
 * what it is for (a redirect in the host's router, a popup, a new tab).
 * While one request is in flight its button disables and the others stay
 * live; a failed answer (authn.provider_unknown, authn.redirect_uri_not_allowed)
 * renders through the one InlineError banner.
 */

import { useState } from 'react'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import Divider from '@mui/material/Divider'
import type { AuthSession } from '@speed/auth-core'
import { useAuthUiTranslation } from './internal/translation.js'
import { InlineError, errorCodeOf } from './internal/inline-error.js'

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
  const [busyProvider, setBusyProvider] = useState<SocialProvider | null>(null)
  const [errorCode, setErrorCode] = useState<string | null>(null)

  const authorize = async (config: SocialProviderConfig): Promise<void> => {
    setErrorCode(null)
    setBusyProvider(config.provider)
    try {
      const authorizeUrl = await session.socialAuthorizeUrl(config.provider, {
        redirect_uri: config.redirectUri,
      })
      onAuthorizeUrl?.(config.provider, authorizeUrl)
    } catch (error) {
      setErrorCode(errorCodeOf(error))
    } finally {
      setBusyProvider(null)
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
          disabled={busyProvider === config.provider}
          onClick={() => void authorize(config)}
        >
          {t(`social.provider.${config.provider}`)}
        </Button>
      ))}
      <InlineError code={errorCode} />
    </Box>
  )
}
