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
 * onSignedIn once. The tab strip is wired to the mounted channel panel
 * the ARIA tabs way: each Tab carries an id and the aria-controls of its
 * own panel, and the mounted channel renders as the role=tabpanel with
 * that id and the tab as its aria-labelledby -- only the active panel
 * exists in the DOM, since switching unmounts the previous form. The
 * screen renders no heading and nothing here navigates: the page above
 * (branding, the heading, the register link) is host content.
 *
 * The channels themselves are the host's to declare through `channels`
 * (both offered by default): this package renders exactly the channels
 * the host composes, since no server the session talks to exposes a
 * channel discovery of its own. A deployment nothing can deliver an SMS
 * code to -- the seam resolves to the console sender, say -- leaves
 * 'sms' out of the list rather than offering a channel whose code can
 * never arrive. A single declared channel renders without a tab strip
 * at all: a tablist with nothing to switch between is not a choice.
 */

import { useId, useState } from 'react'
import type { ReactNode } from 'react'
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

/** The channels offered when the host declares none. */
const DEFAULT_CHANNELS: readonly SignInChannel[] = ['password', 'sms']

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
  /**
   * The channels the screen offers, in tab order; both channels by
   * default. A deployment must not offer a channel it cannot finish --
   * offering SMS sign-in while nothing can deliver a code is a promise
   * no phone will ever keep -- so a host with no working SMS delivery
   * declares ['password'] and the screen renders the password channel
   * alone, without a tab strip. An empty list means "no opinion" and
   * falls back to both channels.
   */
  readonly channels?: readonly SignInChannel[]
  /** The channel selected on first render; password by default. When
   * the named channel is not offered, the first offered one is shown
   * instead. */
  readonly defaultChannel?: SignInChannel
  /** Fired once after a sign-in commits on any channel; the host
   * navigates. */
  readonly onSignedIn?: () => void
}

export function SignInScreen({
  session,
  social,
  channels,
  defaultChannel = 'password',
  onSignedIn,
}: SignInScreenProps) {
  const { t } = useAuthUiTranslation()
  const offered: readonly SignInChannel[] =
    channels === undefined || channels.length === 0 ? DEFAULT_CHANNELS : channels
  const [channel, setChannel] = useState<SignInChannel>(() =>
    offered.includes(defaultChannel)
      ? defaultChannel
      : // An empty declaration fell back to DEFAULT_CHANNELS above, so
        // the list is never empty here; 'password' is the unreachable
        // index guard's answer, not a second default.
        (offered[0] ?? 'password'),
  )
  // The id base of the tab/tabpanel pair per channel: each channel's
  // tab names its panel through aria-controls, and the panel -- the
  // mounted form, unmounted on switch so a channel's half-typed state
  // and whole-attempt error reset with it -- answers through
  // aria-labelledby.
  const channelId = useId()
  const tabIdOf = (value: SignInChannel): string => `${channelId}-${value}-tab`
  const panelIdOf = (value: SignInChannel): string =>
    `${channelId}-${value}-panel`
  const channelTitle = (value: SignInChannel): string =>
    value === 'password' ? t('passwordSignIn.title') : t('smsSignIn.title')
  // The offered channel's form. A single offered channel renders it
  // directly, with no tabpanel wrapper: there is no tab for a panel to
  // answer to, so the ARIA tabs wiring would dangle.
  const renderChannel = (value: SignInChannel): ReactNode =>
    value === 'password' ? (
      <PasswordSignInForm session={session} onSignedIn={onSignedIn} />
    ) : (
      <SMSSignInForm session={session} onSignedIn={onSignedIn} />
    )

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2, width: '100%' }}>
      {offered.length > 1 ? (
        <Tabs
          value={channel}
          onChange={(_event, value: SignInChannel) => setChannel(value)}
          variant="fullWidth"
        >
          {offered.map((value) => (
            <Tab
              key={value}
              label={channelTitle(value)}
              value={value}
              id={tabIdOf(value)}
              aria-controls={panelIdOf(value)}
            />
          ))}
        </Tabs>
      ) : null}
      {offered.length > 1 ? (
        <Box
          role="tabpanel"
          id={panelIdOf(channel)}
          aria-labelledby={tabIdOf(channel)}
        >
          {renderChannel(channel)}
        </Box>
      ) : (
        renderChannel(channel)
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
