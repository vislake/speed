/**
 * SignInScreen: the assembled sign-in surface over the family's channels.
 *
 * A channel tab strip -- password or SMS code, labels from the bundle,
 * the password channel first by default -- switches the sign-in form
 * below it; when social options are given, the social section renders
 * under the form with the divider title between them. Switching channels
 * unmounts the previous form, so its half-typed state and whole-attempt
 * error are gone with it -- a deliberate reset: channel errors must not
 * leak across surfaces. A successful sign-in on any channel fires
 * onSignedIn once. The screen renders no heading and nothing here
 * navigates: the page above (branding, the heading, the register link)
 * is host content.
 */

import { useState } from 'react'
import Box from '@mui/material/Box'
import Tab from '@mui/material/Tab'
import Tabs from '@mui/material/Tabs'
import type { AuthSession } from '@speed/auth-core'
import { PasswordSignInForm } from './PasswordSignInForm.js'
import { SMSSignInForm } from './SMSSignInForm.js'
import { SocialSignInSection } from './SocialSignInSection.js'
import type {
  SocialProvider,
  SocialProviderConfig,
} from './SocialSignInSection.js'
import { useAuthUiTranslation } from './internal/translation.js'

/** The sign-in channels the screen toggles between. */
export type SignInChannel = 'password' | 'sms'

/** The social block of the screen, rendered when given. */
export interface SocialSignInOptions {
  /** The channels to render, in the order the host wants. */
  readonly providers: readonly SocialProviderConfig[]
  /** Receives each channel's authorization URL once built; the host
   * navigates. */
  readonly onAuthorizeUrl?: (provider: SocialProvider, authorizeUrl: string) => void
}

export interface SignInScreenProps {
  /** The session every channel on the screen drives. */
  readonly session: AuthSession
  /** The social block of the screen; omitted to run without one. */
  readonly social?: SocialSignInOptions
  /** The channel selected on first render; password by default. */
  readonly defaultChannel?: SignInChannel
  /** Fired once after a sign-in commits on any channel; the host
   * navigates. */
  readonly onSignedIn?: () => void
}

export function SignInScreen({
  session,
  social,
  defaultChannel = 'password',
  onSignedIn,
}: SignInScreenProps) {
  const { t } = useAuthUiTranslation()
  const [channel, setChannel] = useState<SignInChannel>(defaultChannel)

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2, width: '100%' }}>
      <Tabs
        value={channel}
        onChange={(_event, value: SignInChannel) => setChannel(value)}
        variant="fullWidth"
      >
        <Tab label={t('passwordSignIn.title')} value="password" />
        <Tab label={t('smsSignIn.title')} value="sms" />
      </Tabs>
      {channel === 'password' ? (
        <PasswordSignInForm session={session} onSignedIn={onSignedIn} />
      ) : (
        <SMSSignInForm session={session} onSignedIn={onSignedIn} />
      )}
      {social !== undefined ? (
        <SocialSignInSection
          session={session}
          providers={social.providers}
          onAuthorizeUrl={social.onAuthorizeUrl}
        />
      ) : null}
    </Box>
  )
}
