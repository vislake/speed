/**
 * usage-example.test.tsx -- the README quick start, compiled and run.
 *
 * The quick start wires the account page for a signed-in viewer: the
 * bilingual i18n instance with both namespaces registered (the
 * account-ui namespace for this package's strings, the ui-kit namespace
 * because the confirm dialogs speak ui-kit-namespace keys), the
 * AppThemeProvider tree and a QueryClientProvider (the account surfaces
 * read their data through the @tanstack/react-query hooks generated into
 * @speed/api-sdk -- the one provider this package's harness carries that
 * auth-ui's, the template for this file, does not), and the host's own
 * account page composing the four sections under a signed-in session.
 *
 * Why a real client: the component suites of this package drive each
 * surface in isolation through the same real-client rig (see the
 * MfaSection suite's header), but the wiring the README documents --
 * the session, the client bound to the api-sdk runtime, the providers,
 * the i18n instance, the theme and query providers, and the sections
 * side by side -- is a composition no single-component suite exercises.
 * This file is that journey: it signs a session in, mounts the page
 * and walks it through the surfaces' stories -- one session revoked
 * alone, the rest behind the double-confirmed dialog, a refused unbind
 * that stays on the page, the step-up-gated two-factor replacement with
 * its show-once recovery codes, and a WeChat binding that walks the
 * authorize URL to the host's callback route and back -- over a real
 * @speed/api-client whose scripted fetch answers genuine Response
 * objects, with the whole exchange pinned in order at the end.
 */

import { useState } from 'react'
import type { ReactElement } from 'react'
import { describe, expect, it } from 'vitest'
import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { switchLanguage } from '@speed/i18n'
import { uiKitResources } from '@speed/ui-kit'
import type { AuthSession } from '@speed/auth-core'
import {
  errorResponse,
  jsonResponse,
  makePair,
  makeRealClientRig,
} from '../test-utils/real-client.js'
import { renderWithProviders } from '../test-utils/render.js'
import { expectNoAxeViolations } from '../test-utils/axe.js'
import { SessionsSection } from './SessionsSection.js'
import { LoginHistorySection } from './LoginHistorySection.js'
import { SocialBindingsSection } from './SocialBindingsSection.js'
import type { SocialProviderConfig } from './SocialBindingsSection.js'
import { BindingCallbackHandler } from './BindingCallbackHandler.js'
import { MfaSection } from './MfaSection.js'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import enUS from './locales/en-US.json' with { type: 'json' }

// The ui-kit dialog strings the armed-confirm flow depends on, typed
// straight from the package's own exported resources.
const UI_KIT_ZH = uiKitResources['zh-CN'] as unknown as {
  readonly confirmDialog: { readonly confirmAgainLabel: string }
}

const SESSIONS_PATH = '/api/v1/authn/sessions'
const LOGIN_HISTORY_PATH = '/api/v1/authn/login-history'
const IDENTITIES_PATH = '/api/v1/authn/identities'
const REVOKE_OTHERS_PATH = '/api/v1/authn/sessions/revoke-others'
const ENROLL_PATH = '/api/v1/authn/mfa/totp/enroll'
const STEP_UP_PATH = '/api/v1/authn/mfa/step-up'
const CONFIRM_PATH = '/api/v1/authn/mfa/totp/confirm'

// The demo account of the quick start: a passwordless account whose one
// sign-in method is GitHub (the ground truth the refused unbind below
// rests on), with a TOTP authenticator already enrolled (the ground
// truth the refused enroll discovers).
const SECRET = 'JBSWY3DPEHPK3PXP'
const PROVISIONING_URI =
  'otpauth://totp/speed:owner@example.test?issuer=speed'
const CONFIRM_CODE = '123456'
const RECOVERY_CODES = [
  'able-able-1111',
  'beta-beta-2222',
  'charlie-3333',
  'delta-4444',
  'echo-5555',
  'foxtrot-6666',
  'golf-7777',
  'hotel-8888',
  'india-9999',
  'juliet-0000',
]

// The step-up 200: a fresh access token and no refresh token (a step-up
// reuses the caller's existing one, so the body omits it).
const STEP_UP_BODY = {
  access_token: 'access-2',
  principal: {
    user_id: 'user-1',
    tenant_id: 'tenant-1',
    session_id: 'session-1',
  },
}

// The host's two callback routes; the redirect_uri inside each
// provider's authorize URL is how a host routes a redirect back to the
// right one.
const GITHUB_REDIRECT_URI = 'https://app.example.test/callback/social/github'
const WECHAT_REDIRECT_URI = 'https://app.example.test/callback/social/wechat'
const PROVIDERS: readonly SocialProviderConfig[] = [
  { provider: 'github', redirectUri: GITHUB_REDIRECT_URI },
  { provider: 'wechat', redirectUri: WECHAT_REDIRECT_URI },
]
// The state travels in the authorize URL and comes back with the code;
// the code itself is the provider's to mint once the viewer approves.
const WECHAT_STATE = 'st-wc-1'
const WECHAT_CODE = 'wc-code-1'
const WECHAT_AUTHORIZE_URL =
  'https://open.weixin.qq.com/connect/qrconnect' +
  '?appid=wx-demo-1' +
  '&scope=snsapi_login' +
  `&redirect_uri=${encodeURIComponent(WECHAT_REDIRECT_URI)}` +
  `&state=${WECHAT_STATE}`

const SESSION_T0 = '2026-08-01T02:05:00.000Z'
const SESSION_T1 = '2026-07-28T01:10:00.000Z'
const SESSION_T2 = '2026-08-03T09:12:00.000Z'
const SESSION_T3 = '2026-08-02T20:40:00.000Z'
const SESSION_T4 = '2026-07-30T03:30:00.000Z'
const SESSION_T5 = '2026-08-03T06:45:00.000Z'

/** The formatted-time interpolation of the bundle key, computed the way
 * the components compute it, so the assertion tracks the host's own
 * Intl output for the current language. */
function timeText(key: string, iso: string): string {
  const formatter = new Intl.DateTimeFormat('zh-CN', {
    dateStyle: 'medium',
    timeStyle: 'short',
  })
  return key.replace('{{time}}', formatter.format(new Date(iso)))
}

/** The row-end revoke label of the row whose device label reads
 * `device`, interpolated the way the component interpolates it -- each
 * row's action is named after the row itself, so the action is
 * queryable per row without climbing the DOM. */
function revokeAriaOf(device: string): string {
  return zhCN.sessions.revokeAriaWithDevice.replace('{{device}}', device)
}

/**
 * The host's account page in the example: the four account surfaces the
 * quick start documents, plus the host-owned binding turn -- the add
 * area reports an authorize URL upward, the host records it and
 * "navigates" to the callback route the URL's redirect_uri names,
 * BindingCallbackHandler completes the exchange there, and onBound
 * navigates back. The account surfaces unmount with the route change
 * and remount on return, exactly as they would behind a router.
 */
function AccountPage({
  session,
  onAuthorizeUrl,
}: {
  readonly session: AuthSession
  readonly onAuthorizeUrl: (url: string) => void
}): ReactElement {
  const [redirectedTo, setRedirectedTo] = useState<string | null>(null)

  if (redirectedTo !== null) {
    const redirectUri = new URL(redirectedTo).searchParams.get('redirect_uri')
    const config = PROVIDERS.find(
      (candidate) => candidate.redirectUri === redirectUri,
    )
    if (config === undefined) {
      throw new Error(`no configured callback route for ${redirectedTo}`)
    }
    const state = new URL(redirectedTo).searchParams.get('state')
    return (
      <BindingCallbackHandler
        provider={config.provider}
        code={WECHAT_CODE}
        state={state ?? ''}
        onBound={() => setRedirectedTo(null)}
      />
    )
  }

  return (
    <div>
      <SessionsSection />
      <LoginHistorySection />
      <SocialBindingsSection
        session={session}
        providers={PROVIDERS}
        onAuthorizeUrl={(url) => {
          onAuthorizeUrl(url)
          setRedirectedTo(url)
        }}
      />
      <MfaSection session={session} />
    </div>
  )
}

describe('the README quick start, exercised over a real api-client', () => {
  it('walks the account page from a social sign-in through sessions, a refused unbind, a step-up-gated factor replacement and a social binding', async () => {
    // The scripted server. Flags hold the server-side story the
    // refetches converge on: the iPad session revoked alone, the other
    // sessions behind the double-confirmed revoke-others, and the
    // WeChat binding that arrives at the end.
    let ipadRevoked = false
    let othersRevoked = false
    let wechatBound = false
    let enrollCalls = 0
    // The binding exchange sits on a gate so the callback route's
    // in-flight state is observable before the exchange lands and the
    // host navigates back.
    let releaseWechatExchange: () => void = () => undefined
    const wechatExchangeGate = new Promise<void>((resolve) => {
      releaseWechatExchange = resolve
    })
    const rig = makeRealClientRig(async (call) => {
      switch (call.path) {
        case '/api/v1/authn/social/github/callback':
          // The sign-in exchange of the passwordless demo account.
          return jsonResponse(200, {
            user: { id: 'user-1', email: 'maya@example.test' },
            tokens: makePair(),
            created: false,
          })
        case SESSIONS_PATH:
          return jsonResponse(200, {
            sessions: [
              {
                id: 'session-1',
                status: 'active',
                is_current: true,
                device: 'This laptop',
                ip: '198.51.100.4',
                amr: ['social:github'],
                created_at: SESSION_T0,
                last_seen_at: SESSION_T2,
              },
              {
                id: 'session-2',
                status: othersRevoked ? 'revoked' : 'active',
                is_current: false,
                device: 'Windows desktop',
                ip: '203.0.113.10',
                amr: ['social:github'],
                created_at: SESSION_T1,
                last_seen_at: SESSION_T3,
              },
              {
                id: 'session-3',
                status: ipadRevoked ? 'revoked' : 'active',
                is_current: false,
                device: 'iPad Safari',
                ip: '203.0.113.55',
                amr: ['social:github'],
                created_at: SESSION_T4,
                last_seen_at: SESSION_T5,
              },
              {
                id: 'session-4',
                status: othersRevoked ? 'revoked' : 'active',
                is_current: false,
                device: 'Android phone',
                ip: '203.0.113.99',
                amr: ['social:github'],
                created_at: SESSION_T5,
                last_seen_at: SESSION_T2,
              },
            ],
          })
        case LOGIN_HISTORY_PATH:
          return jsonResponse(200, {
            attempts: [
              {
                method: 'social',
                result: 'success',
                created_at: '2026-08-03T02:30:00.000Z',
              },
              {
                method: 'password',
                result: 'failure',
                failure_reason: 'bad_password',
                created_at: '2026-08-02T11:05:00.000Z',
              },
              {
                method: 'sms',
                result: 'failure',
                failure_reason: 'bad_code',
                created_at: '2026-08-01T09:00:00.000Z',
              },
            ],
          })
        case IDENTITIES_PATH:
          return jsonResponse(200, {
            identities: [
              { id: 'github-1', provider: 'github', email: 'maya@example.test' },
              ...(wechatBound
                ? [{ id: 'wechat-1', provider: 'wechat' }]
                : []),
            ],
          })
        case `${SESSIONS_PATH}/session-3`:
          ipadRevoked = true
          return new Response(null, { status: 204 })
        case REVOKE_OTHERS_PATH:
          othersRevoked = true
          return jsonResponse(200, { revoked_count: 2 })
        case `${IDENTITIES_PATH}/github-1`:
          // The account's one sign-in method cannot be shed.
          return errorResponse(409, 'authn.last_login_method')
        case ENROLL_PATH:
          enrollCalls += 1
          if (enrollCalls === 1) {
            // An active factor exists: only the step-up can open the door.
            return errorResponse(403, 'authn.step_up_required')
          }
          return jsonResponse(200, {
            secret: SECRET,
            provisioning_uri: PROVISIONING_URI,
          })
        case STEP_UP_PATH:
          return jsonResponse(200, STEP_UP_BODY)
        case CONFIRM_PATH:
          return jsonResponse(200, { recovery_codes: RECOVERY_CODES })
        case '/api/v1/authn/social/wechat/authorize':
          return jsonResponse(200, { authorize_url: WECHAT_AUTHORIZE_URL })
        case '/api/v1/authn/social/wechat/callback':
          await wechatExchangeGate
          wechatBound = true
          return jsonResponse(200, {
            bound: true,
            identity: { id: 'wechat-1', provider: 'wechat' },
          })
      }
      throw new Error(`no scripted answer for ${call.method} ${call.path}`)
    })

    // The sign-in the account page presumes: the social exchange of the
    // passwordless account, driven through the real session operation
    // before the page mounts. The store holds the issued access token.
    await rig.session.completeSocialLogin('github', {
      code: 'gh-code-1',
      state: 'st-gh-1',
    })
    expect(rig.store.get()).toBe('access-1')

    // The quick start's bootstrap: mount the host's account page under
    // the i18n instance (zh-CN to start) with both namespaces, the theme
    // provider and a fresh query client.
    let capturedAuthorizeUrl: string | null = null
    const { i18n } = renderWithProviders(
      <AccountPage
        session={rig.session}
        onAuthorizeUrl={(url) => {
          capturedAuthorizeUrl = url
        }}
      />,
    )
    const user = userEvent.setup()

    // The surfaces settle on the server's answers: the four sessions
    // with the current one marked, the history rows with their method
    // and result labels, the GitHub binding listed (with the WeChat
    // channel offered for binding) and the two-factor entries.
    expect(
      await screen.findByRole('heading', { name: zhCN.sessions.title }),
    ).toBeTruthy()
    expect(await screen.findByText(zhCN.sessions.current)).toBeTruthy()
    expect(screen.getByText('This laptop')).toBeTruthy()
    expect(
      screen.getByText(timeText(zhCN.sessions.signedIn, SESSION_T0)),
    ).toBeTruthy()
    expect(
      await screen.findByText(zhCN.history.result.success),
    ).toBeTruthy()
    expect(screen.getByText(zhCN.history.reason.bad_password)).toBeTruthy()
    expect(screen.getByText(zhCN.history.reason.bad_code)).toBeTruthy()
    expect(
      await screen.findByText(zhCN.bindings.provider.github),
    ).toBeTruthy()
    expect(
      screen.getByRole('button', { name: zhCN.bindings.provider.wechat }),
    ).toBeTruthy()
    expect(screen.getByRole('heading', { name: zhCN.mfa.title })).toBeTruthy()
    expect(
      screen.getByRole('button', { name: zhCN.mfa.authenticator.enrollButton }),
    ).toBeTruthy()
    await expectNoAxeViolations()

    // Sign the iPad out of this account: the single-row revoke is a
    // one-click action on the row itself, and each row's action is named
    // after that row's device label -- the three revocable rows each
    // answer by name, the current laptop row carries no action.
    for (const device of ['Windows desktop', 'iPad Safari', 'Android phone']) {
      expect(
        screen.getByRole('button', { name: revokeAriaOf(device) }),
      ).toBeTruthy()
    }
    expect(
      screen.queryByRole('button', { name: revokeAriaOf('This laptop') }),
    ).toBeNull()
    await user.click(
      screen.getByRole('button', { name: revokeAriaOf('iPad Safari') }),
    )
    await waitFor(() => {
      expect(
        screen.getAllByText(zhCN.sessions.status.revoked),
      ).toHaveLength(1)
    })
    // The revoked row's action is gone; the other two rows keep theirs,
    // still each named after its own device.
    expect(
      screen.queryByRole('button', { name: revokeAriaOf('iPad Safari') }),
    ).toBeNull()
    for (const device of ['Windows desktop', 'Android phone']) {
      expect(
        screen.getByRole('button', { name: revokeAriaOf(device) }),
      ).toBeTruthy()
    }

    // Sign out everywhere else -- the one double-confirmed action on the
    // page: the first click on the danger confirm arms it (the ui-kit
    // confirm-again label), only the second revokes.
    await user.click(
      screen.getByRole('button', { name: zhCN.sessions.revokeOthers.label }),
    )
    const dialog = await screen.findByRole('dialog')
    expect(
      within(dialog).getByText(zhCN.sessions.revokeOthers.confirmTitle),
    ).toBeTruthy()
    await user.click(
      within(dialog).getByRole('button', {
        name: zhCN.sessions.revokeOthers.confirmLabel,
      }),
    )
    await user.click(
      within(dialog).getByRole('button', {
        name: UI_KIT_ZH.confirmDialog.confirmAgainLabel,
      }),
    )
    // The server's count surfaces and the refetched list shows both rows
    // revoked. The dialog's exit transition must finish before the
    // page behind it (aria-hidden while the modal is up) is queryable
    // by role again; only then is the section-top action gone.
    expect(
      await screen.findByText(
        zhCN.sessions.revokeOthers.done_other.replace('{{count}}', '2'),
      ),
    ).toBeTruthy()
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    await waitFor(() => {
      expect(
        screen.getAllByText(zhCN.sessions.status.revoked),
      ).toHaveLength(3)
    })
    expect(
      screen.queryByRole('button', {
        name: zhCN.sessions.revokeOthers.label,
      }),
    ).toBeNull()

    // Unlink the GitHub binding: refused -- this passwordless account
    // must keep its one sign-in method. The single-click danger confirm
    // sends the DELETE; the 409 renders its code text above the list,
    // the row stays and no refetch chases the refusal.
    await user.click(
      screen.getByRole('button', { name: zhCN.bindings.unbind }),
    )
    const unbindDialog = await screen.findByRole('dialog')
    await user.click(
      within(unbindDialog).getByRole('button', {
        name: zhCN.bindings.confirmLabel,
      }),
    )
    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toBe(zhCN.errors.authn.last_login_method)
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(screen.getByText(zhCN.bindings.provider.github)).toBeTruthy()
    expect(
      rig.calls.filter(
        (call) => call.method === 'GET' && call.path === IDENTITIES_PATH,
      ),
    ).toHaveLength(1)
    await expectNoAxeViolations()

    // Two-factor verification was set up earlier: a first enroll is
    // refused with step_up_required, which opens the challenge dialog.
    await user.click(
      screen.getByRole('button', { name: zhCN.mfa.authenticator.enrollButton }),
    )
    const stepUpDialog = await screen.findByRole('dialog')
    expect(
      within(stepUpDialog).getByText(zhCN.mfa.stepUp.title),
    ).toBeTruthy()

    // The verified step-up rotates the access token and re-runs exactly
    // the gated action: the dialog closes and the enroll retry lands in
    // the replacement wizard -- confirming replaces the current
    // authenticator and the current recovery codes stop working.
    await user.type(
      screen.getByLabelText(zhCN.mfa.stepUp.codeLabel),
      CONFIRM_CODE,
    )
    await user.click(
      screen.getByRole('button', { name: zhCN.mfa.stepUp.confirmLabel }),
    )
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(rig.store.get()).toBe('access-2')
    expect(await screen.findByText(SECRET)).toBeTruthy()
    expect(
      screen.getByText(zhCN.mfa.authenticator.replacingNotice),
    ).toBeTruthy()

    // The confirm opens the show-once recovery codes; saving them leaves
    // the section idle again.
    await user.type(
      screen.getByLabelText(zhCN.mfa.authenticator.codeLabel),
      CONFIRM_CODE,
    )
    await user.click(
      screen.getByRole('button', {
        name: zhCN.mfa.authenticator.confirmLabel,
      }),
    )
    expect(
      await screen.findByText(zhCN.mfa.recoveryCodes.showOnceTitle),
    ).toBeTruthy()
    for (const code of RECOVERY_CODES) {
      expect(screen.getByText(code)).toBeTruthy()
    }
    await expectNoAxeViolations()
    await user.click(
      screen.getByRole('button', { name: zhCN.mfa.recoveryCodes.savedLabel }),
    )
    expect(
      screen.queryByText(zhCN.mfa.recoveryCodes.showOnceTitle),
    ).toBeNull()
    expect(
      screen.getByRole('button', { name: zhCN.mfa.authenticator.enrollButton }),
    ).toBeTruthy()

    // Link the WeChat account: the add area's button asks the session
    // for that channel's authorization URL -- a pure request reported
    // upward; the host (miniature) opens it and its callback route
    // completes the exchange with the code and the echoed-back state.
    await user.click(
      screen.getByRole('button', {
        name: zhCN.bindings.provider.wechat,
      }),
    )
    const authorizeUrl = await waitFor(() => {
      expect(capturedAuthorizeUrl).not.toBeNull()
      return capturedAuthorizeUrl ?? ''
    })
    expect(authorizeUrl).toBe(WECHAT_AUTHORIZE_URL)
    // The callback route holds its in-flight state (a status notice)
    // while the exchange is out; releasing the gate lands the
    // binding-shaped answer.
    expect(await screen.findByText(zhCN.bindingCallback.pending)).toBeTruthy()
    releaseWechatExchange()

    // The binding-shaped answer fires onBound: the host navigates back
    // and the account page remounts. The remount's refetches converge
    // on the server's answer: the WeChat row appears and the add area
    // -- its offer button matches the row label by name -- has nothing
    // left to link.
    await waitFor(() =>
      expect(
        screen.queryByRole('button', {
          name: zhCN.bindings.provider.wechat,
        }),
      ).toBeNull(),
    )
    expect(
      await screen.findByText(zhCN.bindings.provider.wechat),
    ).toBeTruthy()
    expect(screen.getByText(zhCN.bindings.provider.github)).toBeTruthy()
    expect(screen.getByText('maya@example.test')).toBeTruthy()

    // The same page in the other supported language.
    await act(async () => {
      await switchLanguage(i18n, 'en-US')
    })
    for (const title of [
      enUS.sessions.title,
      enUS.history.title,
      enUS.bindings.title,
      enUS.mfa.title,
    ]) {
      expect(screen.getByRole('heading', { name: title })).toBeTruthy()
    }

    // The whole exchange, in order. The sign-in exchange is the page's
    // turn zero: it travels credential-less (the exchange of an
    // anonymous session), every later call rides the session's bearer
    // token -- access-1 until the step-up rotates it to access-2
    // mid-journey. The three reads after the binding callback are the
    // account page's own remount refetches, the host navigating back
    // from the callback route the way the quick start describes.
    expect(
      rig.calls.map((call) => `${call.method} ${call.path}`),
    ).toEqual([
      'POST /api/v1/authn/social/github/callback',
      'GET /api/v1/authn/sessions',
      'GET /api/v1/authn/login-history',
      'GET /api/v1/authn/identities',
      'DELETE /api/v1/authn/sessions/session-3',
      'GET /api/v1/authn/sessions',
      'POST /api/v1/authn/sessions/revoke-others',
      'GET /api/v1/authn/sessions',
      'DELETE /api/v1/authn/identities/github-1',
      'POST /api/v1/authn/mfa/totp/enroll',
      'POST /api/v1/authn/mfa/step-up',
      'POST /api/v1/authn/mfa/totp/enroll',
      'POST /api/v1/authn/mfa/totp/confirm',
      'GET /api/v1/authn/social/wechat/authorize',
      'POST /api/v1/authn/social/wechat/callback',
      'GET /api/v1/authn/sessions',
      'GET /api/v1/authn/login-history',
      'GET /api/v1/authn/identities',
    ])
    const calls = rig.calls
    expect(calls[0]?.authorization).toBeNull()
    expect(calls[1]?.authorization).toBe('Bearer access-1')
    expect(calls[2]?.query).toBe('?limit=20')
    expect(calls[4]?.authorization).toBe('Bearer access-1')
    expect(calls[6]?.authorization).toBe('Bearer access-1')
    expect(calls[9]?.authorization).toBe('Bearer access-1')
    // The step-up's own request still rides the stale token; everything
    // after it rides the rotated one.
    expect(calls[10]?.authorization).toBe('Bearer access-1')
    expect(calls[11]?.authorization).toBe('Bearer access-2')
    expect(calls[12]?.authorization).toBe('Bearer access-2')
    // The authorize request carries the redirect URI the host configured
    // for the channel as its query parameter; the binding callback --
    // signed-in all the way -- rides the rotated token.
    expect(calls[13]?.query).toBe(
      `?redirect_uri=${encodeURIComponent(WECHAT_REDIRECT_URI)}`,
    )
    expect(calls[14]?.authorization).toBe('Bearer access-2')
    expect(calls[15]?.authorization).toBe('Bearer access-2')

    await expectNoAxeViolations()
  })
})
