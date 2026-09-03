/**
 * session-journey.test.tsx -- the sign-in family's journeys beyond the
 * usage example: the SMS channel end to end, an explicit sign-out, and
 * the social callback route, each asserting the UI-visible states a
 * viewer would see.
 *
 * The journeys compose the same rig and gate as usage-example.test.tsx --
 * a real @speed/api-client over a fetch stand-in answering with genuine
 * Response objects, the memory store, refreshAccessToken:
 * () => session.refresh() bound through the api-sdk runtime seam, and
 * the host gate flipping between the sign-in surface and the app on the
 * auth-core snapshot -- so every leg runs the transport a host's wiring
 * runs. The gate and the network rig live in test-utils/ (shared-helper
 * rule); this file is named for the behaviour it chronicles rather than
 * for one source file, because it crosses the whole composed family.
 */

import { useState } from 'react'
import type { ReactElement } from 'react'
import { describe, expect, it } from 'vitest'
import { act, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { attachSession } from '@speed/auth-core'
import type { AuthSession } from '@speed/auth-core'
import { SessionGate } from '../test-utils/session-gate.js'
import {
  jsonResponse,
  makeRealClientRig,
} from '../test-utils/real-client.js'
import { renderWithProviders } from '../test-utils/render.js'
import { makePair } from '../test-utils/session-harness.js'
import { SignInScreen } from './SignInScreen.js'
import { SignOutButton } from './SignOutButton.js'
import { SocialCallbackHandler } from './SocialCallbackHandler.js'
import zhCN from './locales/zh-CN.json' with { type: 'json' }

const PHONE = '+8613800138000'
const PHONE_LABEL_ZH = zhCN.smsSignIn.phoneLabel
const SEND_CODE_ZH = zhCN.smsSignIn.sendCode
const SENT_NOTICE_ZH = zhCN.smsSignIn.sentNotice.replace('{{phone}}', PHONE)
const CODE_LABEL_ZH = zhCN.smsSignIn.codeLabel
const SMS_TAB_ZH = zhCN.smsSignIn.title
const IDENTIFIER_LABEL_ZH = zhCN.passwordSignIn.identifierLabel
const PASSWORD_LABEL_ZH = zhCN.passwordSignIn.passwordLabel
const SUBMIT_ZH = zhCN.passwordSignIn.submit
const SIGN_OUT_ZH = zhCN.signOut.label
const SESSION_ENDED_TITLE_ZH = zhCN.sessionEnded.title
const SESSION_ENDED_DESCRIPTION_ZH = zhCN.sessionEnded.description
const SIGN_IN_AGAIN_ZH = zhCN.sessionEnded.signInAction
const GOOGLE_LABEL_ZH = zhCN.social.provider.google
const PENDING_ZH = zhCN.socialCallback.pending

const LOGIN_PASSWORD = '/api/v1/authn/login/password'
const LOGOUT = '/api/v1/authn/logout'
const REQUEST_SMS_CODE = '/api/v1/authn/login/sms/request'
const LOGIN_SMS = '/api/v1/authn/login/sms'
const SOCIAL_AUTHORIZE = '/api/v1/authn/social/google/authorize'
const SOCIAL_CALLBACK = '/api/v1/authn/social/google/callback'

/** The host's app content the journeys land in after a sign-in: a
 * heading and the sign-out action in app chrome. */
function AppView({ session }: { session: AuthSession }): ReactElement {
  return (
    <div>
      <h1>Signed in</h1>
      <SignOutButton session={session} />
    </div>
  )
}

describe('session journeys over the composed sign-in family', () => {
  it('signs in with an SMS code, from the phone step to the app', async () => {
    const rig = makeRealClientRig((call) => {
      switch (call.path) {
        case REQUEST_SMS_CODE:
          // 202 acceptance, whatever the phone number -- the request
          // step's terminal state.
          return new Response('', { status: 202 })
        case LOGIN_SMS:
          return jsonResponse(200, makePair())
      }
      throw new Error(`no scripted answer for ${call.method} ${call.path}`)
    })
    attachSession(rig.session)
    renderWithProviders(
      <SessionGate
        app={<AppView session={rig.session} />}
        signIn={<SignInScreen session={rig.session} />}
      />,
    )
    const user = userEvent.setup()

    // The SMS channel: the phone step requests the code.
    await user.click(screen.getByRole('tab', { name: SMS_TAB_ZH }))
    await user.type(screen.getByLabelText(PHONE_LABEL_ZH), PHONE)
    await user.click(screen.getByRole('button', { name: SEND_CODE_ZH }))
    // The sent notice announces the receiving number.
    const sent = await screen.findByRole('status')
    expect(sent).toHaveTextContent(SENT_NOTICE_ZH)
    // The request step was a 202 acceptance, never a state change: the
    // gate still shows the sign-in surface for the code step.
    expect(rig.store.get()).toBeNull()

    // The code step completes the sign-in.
    await user.type(screen.getByLabelText(CODE_LABEL_ZH), '123456')
    await user.click(screen.getByRole('button', { name: SUBMIT_ZH }))
    expect(
      await screen.findByRole('heading', { name: 'Signed in' }),
    ).toBeInTheDocument()
    expect(rig.store.get()).toBe('access-1')
    expect(
      rig.calls.map((call) => `${call.method} ${call.path}`),
    ).toEqual(['POST /api/v1/authn/login/sms/request', 'POST /api/v1/authn/login/sms'])
    // Both requests of an anonymous SMS sign-in travel credential-less.
    expect(rig.calls[0]?.authorization).toBeNull()
    expect(rig.calls[1]?.authorization).toBeNull()
  })

  it('signs out from the app chrome and lands back on the sign-in surface', async () => {
    const rig = makeRealClientRig((call) => {
      switch (call.path) {
        case LOGIN_PASSWORD:
          return jsonResponse(200, makePair())
        case LOGOUT:
          // 204 answers carry no body by construction (the fetch spec
          // forbids one), and this environment's Response constructor
          // enforces that.
          return new Response(null, { status: 204 })
      }
      throw new Error(`no scripted answer for ${call.method} ${call.path}`)
    })
    attachSession(rig.session)
    renderWithProviders(
      <SessionGate
        app={<AppView session={rig.session} />}
        signIn={<SignInScreen session={rig.session} />}
      />,
    )
    const user = userEvent.setup()

    // Sign in through the password channel to reach the app.
    await user.type(
      screen.getByLabelText(IDENTIFIER_LABEL_ZH),
      'alice@example.com',
    )
    await user.type(screen.getByLabelText(PASSWORD_LABEL_ZH), 's3cret-pass')
    await user.click(screen.getByRole('button', { name: SUBMIT_ZH }))
    await screen.findByRole('heading', { name: 'Signed in' })
    expect(rig.store.get()).toBe('access-1')

    // The explicit sign-out is quiet in the button; its success is the
    // snapshot flip the gate observes -- the app view becomes the
    // session-ended screen.
    await user.click(screen.getByRole('button', { name: SIGN_OUT_ZH }))
    expect(await screen.findByText(SESSION_ENDED_TITLE_ZH))
      .toBeInTheDocument()
    expect(screen.getByText(SESSION_ENDED_DESCRIPTION_ZH))
      .toBeInTheDocument()
    expect(rig.store.get()).toBeNull()
    expect(
      rig.calls.map((call) => `${call.method} ${call.path}`),
    ).toEqual(['POST /api/v1/authn/login/password', 'POST /api/v1/authn/logout'])
    // The logout of an authenticated session travels with its access
    // token.
    expect(rig.calls[1]?.authorization).toBe('Bearer access-1')

    // Sign in again from the ended screen: the gate hands the viewer
    // back to the sign-in surface.
    await user.click(screen.getByRole('button', { name: SIGN_IN_AGAIN_ZH }))
    expect(screen.getByLabelText(IDENTIFIER_LABEL_ZH)).toBeInTheDocument()
  })

  it('completes a social sign-in from the authorize URL to the app', async () => {
    const REDIRECT_URI = 'https://app.example.test/social/callback'
    const SOCIAL_STATE = 'state-abc-123'
    const SOCIAL_CODE = '4/0AX4Xf-9dKm'
    const authorizeUrl =
      'https://accounts.google.com/o/oauth2/v2/auth' +
      '?response_type=code' +
      '&client_id=demo-client' +
      `&redirect_uri=${encodeURIComponent(REDIRECT_URI)}` +
      '&scope=openid%20email' +
      `&state=${SOCIAL_STATE}`

    // The callback exchange stays open until the test resolves it, so
    // the pending state of the callback route is assertable.
    let resolveCallback: (response: Response) => void = () => {}
    const callbackPending = new Promise<Response>((resolve) => {
      resolveCallback = resolve
    })
    const rig = makeRealClientRig((call) => {
      switch (call.path) {
        case SOCIAL_AUTHORIZE:
          return jsonResponse(200, { authorize_url: authorizeUrl })
        case SOCIAL_CALLBACK:
          return callbackPending
      }
      throw new Error(`no scripted answer for ${call.method} ${call.path}`)
    })

    /** The sign-in surface of the social journey, standing in for the
     * host's two routes: the sign-in screen, then -- once the authorize
     * URL is built -- the callback route the provider's redirect lands
     * on, with the code and state the redirect would carry. */
    const SocialSurface = ({
      session,
      onAuthorizeUrl,
    }: {
      session: AuthSession
      onAuthorizeUrl: (authorizeUrl: string) => void
    }): ReactElement => {
      const [redirectedTo, setRedirectedTo] = useState<string | null>(null)
      if (redirectedTo === null) {
        return (
          <SignInScreen
            session={session}
            social={{
              providers: [{ provider: 'google', redirectUri: REDIRECT_URI }],
              onAuthorizeUrl: (provider, url) => {
                onAuthorizeUrl(url)
                setRedirectedTo(url)
              },
            }}
          />
        )
      }
      // The state the authorize URL carried is the state the redirect
      // echoes back; the code is the provider's to mint.
      const state = new URL(redirectedTo).searchParams.get('state')
      return (
        <SocialCallbackHandler
          session={session}
          provider="google"
          code={SOCIAL_CODE}
          state={state ?? ''}
        />
      )
    }

    let capturedAuthorizeUrl: string | null = null
    attachSession(rig.session)
    renderWithProviders(
      <SessionGate
        app={<AppView session={rig.session} />}
        signIn={
          <SocialSurface
            session={rig.session}
            onAuthorizeUrl={(url) => {
              capturedAuthorizeUrl = url
            }}
          />
        }
      />,
    )
    const user = userEvent.setup()

    // The provider button builds the authorize URL and hands it to the
    // host (the host opens it; the fixture captures it).
    await user.click(screen.getByRole('button', { name: GOOGLE_LABEL_ZH }))
    const gotAuthorizeUrl = await waitFor(() => {
      expect(capturedAuthorizeUrl).not.toBeNull()
      return capturedAuthorizeUrl ?? ''
    })
    expect(gotAuthorizeUrl).toContain(encodeURIComponent(REDIRECT_URI))
    expect(new URL(gotAuthorizeUrl).searchParams.get('state'))
      .toBe(SOCIAL_STATE)

    // The host's callback route mounted the handler; the exchange is
    // pending while the provider answers.
    expect(await screen.findByText(PENDING_ZH)).toBeInTheDocument()
    const callbackCalls = (): typeof rig.calls =>
      rig.calls.filter((call) => call.path === SOCIAL_CALLBACK)
    await waitFor(() => expect(callbackCalls()).toHaveLength(1))
    // The callback exchange of an anonymous session travels
    // credential-less.
    expect(callbackCalls()[0]?.authorization).toBeNull()

    // The provider answers with a token pair; the snapshot flips and the
    // gate lands the app view.
    await act(async () => {
      resolveCallback(jsonResponse(200, { tokens: makePair() }))
    })
    expect(
      await screen.findByRole('heading', { name: 'Signed in' }),
    ).toBeInTheDocument()
    expect(rig.store.get()).toBe('access-1')
    expect(
      rig.calls.map((call) => `${call.method} ${call.path}`),
    ).toEqual([
      'GET /api/v1/authn/social/google/authorize',
      'POST /api/v1/authn/social/google/callback',
    ])
  })
})
