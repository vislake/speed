/**
 * account-view.tsx -- the account surface of the reference-app web
 * host: the account-ui family composed over the app's session and the
 * generated authn API the way a delivered consumer composes it. The
 * session-operating sections receive the app's own session (the
 * services context the host bootstrap wires); the read-only ones take
 * no props at all. Every list read and mutation goes through the
 * generated hooks of @speed/api-sdk over the host's QueryClient.
 *
 * The binding subroute (app.tsx's parsed fragment) renders the same
 * surface with the completion handler on top: the handler exchanges
 * the (code, state) pair over the app's client, invalidates the
 * identities list through the account-ui query-key builder, and cues
 * the host with onBound -- here the app's own navigation back to the
 * account fragment, passed down from the AppView switch. Until that
 * cue lands, the handler rests on its pending notice, so the host's
 * answer to the cue is what takes the completion UI off the page.
 *
 * Two host responsibilities live in this file. The add area's channels
 * are host configuration: the five demo providers of the app's binding
 * parser, each carrying the redirect URI of the app's own callback
 * route for that channel -- the <origin>/callback/social/<provider>
 * convention the account-ui usage example's host uses. A real
 * deployment serves that route (its SPA fallback delivering index.html
 * and the code/state exchange routing into the binding fragment); this
 * round's shell serves no such route, so the bridge from a callback
 * path to the binding fragment stays the recorded follow-up, and the
 * journeys never click an add-area button. The view's one navigation
 * duty is the authorize click itself: the section reports the
 * channel's URL upward and the view performs window.location.assign --
 * the host's own act, never one the package performs.
 *
 * The account surface deliberately sits outside the notes surface's
 * permission gate: the authn identity-domain endpoints require only a
 * signed-in principal (they are the surfaces that manage the account
 * itself), the /me answer carries no permissions, and rbac mounts no
 * HTTP routes -- gating this view on a permission would invent a
 * server rule the app does not have.
 */

import type { ReactElement } from 'react'
import Box from '@mui/material/Box'
import Typography from '@mui/material/Typography'
import {
  BindingCallbackHandler,
  LoginHistorySection,
  MfaSection,
  SessionsSection,
  SocialBindingsSection,
} from '@speed/account-ui'
import type { SocialProvider, SocialProviderConfig } from '@speed/account-ui'
import { useTranslation } from '@speed/i18n'
import { useAppServices } from '../app-services.js'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'

/** This app's origin, the root of every demo channel's callback route. */
const DEMO_APP_ORIGIN = 'https://app.example.test'

/**
 * The add area's channels: the app's demo provider set, each with the
 * redirect URI of this app's callback route for the channel. app.tsx's
 * DEMO_SOCIAL_PROVIDERS cannot be imported here (the app module
 * renders this view, so importing it would cycle), so the set is
 * declared locally; the binding journeys hold the two in step with the
 * parser, and a provider the parser does not know can never reach the
 * view anyway (it degrades to account content before rendering).
 */
const DEMO_SOCIAL_CHANNELS: readonly SocialProviderConfig[] = (
  ['google', 'github', 'wechat', 'dingtalk', 'feishu'] as const
).map((provider: SocialProvider) => ({
  provider,
  redirectUri: `${DEMO_APP_ORIGIN}/callback/social/${provider}`,
}))

/**
 * The binding exchange the app's fragment parser hands this surface on
 * the /auth/binding/<provider> subroute. Declared here -- a twin of the
 * app module's BindingTarget -- because this view never imports the
 * app module; the two are structurally identical by design.
 */
export interface AccountBindingTarget {
  readonly provider: SocialProvider
  readonly code: string
  readonly state: string
}

export interface AccountViewProps {
  /** Present while the frame is at the binding subroute: the (code,
   * state) pair the completion handler below exchanges. */
  readonly bindingTarget?: AccountBindingTarget
  /**
   * The host's cue once the exchange lands a binding-shaped answer (the
   * identities list refetched): navigate away from the binding
   * subroute, unmounting the completion UI. The app's AppView answers
   * by moving the hash back to the account fragment.
   */
  readonly onBound?: () => void
}

/**
 * The account surface: heading, the in-flight binding completion when
 * the frame is at the binding subroute, then the account-ui sections --
 * sessions, login history, social bindings and multi-factor setup.
 */
export function AccountView({
  bindingTarget,
  onBound,
}: AccountViewProps): ReactElement {
  const { t } = useTranslation(REFERENCE_APP_NAMESPACE)
  const { session } = useAppServices()

  /** The authorize click's one navigation: land the browser on the
   * channel's callback route (the section never navigates itself). */
  function handleAuthorizeUrl(url: string): void {
    window.location.assign(url)
  }

  return (
    <Box sx={{ p: 3, maxWidth: 720 }}>
      <Typography component="h1" variant="h4" sx={{ fontWeight: 600 }}>
        {t('account.heading')}
      </Typography>
      <Typography
        variant="body1"
        color="text.secondary"
        sx={{ marginTop: 1, marginBottom: 3 }}
      >
        {t('account.intro')}
      </Typography>
      {bindingTarget !== undefined && (
        <Box sx={{ marginBottom: 3 }}>
          <BindingCallbackHandler
            provider={bindingTarget.provider}
            code={bindingTarget.code}
            state={bindingTarget.state}
            onBound={onBound}
          />
        </Box>
      )}
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
        <SessionsSection />
        <LoginHistorySection />
        <SocialBindingsSection
          session={session}
          providers={DEMO_SOCIAL_CHANNELS}
          onAuthorizeUrl={handleAuthorizeUrl}
        />
        <MfaSection session={session} />
      </Box>
    </Box>
  )
}
