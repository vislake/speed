/**
 * The README usage example, compiled and executed by the suite.
 *
 * The README's Quick start composes the whole tenant-facing shell: the
 * three namespaces registered once on one i18n instance, one session
 * attached before render, and a ProductShell whose signIn slot is
 * auth-ui's SignInScreen, whose userMenu slot carries auth-ui's
 * SignOutButton, and whose frame wraps the host's own app content. This
 * file renders that exact composition and drives one minimal journey
 * through it -- a password sign-in, the frame appearing, a sign-out
 * through the userMenu, the default session-ended screen, and a return
 * to the sign-in view -- over the real-client rig (a genuine
 * @speed/api-client bound through the same bindRequestFn seam a host's
 * real client binds), pinning every request in order, so the documented
 * usage cannot drift from the API. Host-content strings (nav labels, the
 * app title and content) are English fixtures on purpose: they stand in
 * for a host's own content and are data in a test file (exempt from the
 * no-literal-text rule), not rendered product text. Assertions derive
 * every built-in string from the shipped sibling locale bundles, never
 * inline translations.
 */

import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { screen } from '@testing-library/react'
import { createI18n, registerNamespace } from '@speed/i18n'
import { UI_KIT_NAMESPACE, uiKitResources } from '@speed/ui-kit'
import {
  LAYOUT_KIT_NAMESPACE,
  layoutKitResources,
} from '@speed/layout-kit'
import {
  AUTH_UI_NAMESPACE,
  authUiResources,
  SignInScreen,
  SignOutButton,
} from '@speed/auth-ui'
import { attachSession } from '@speed/auth-core'
import type { AuthSession } from '@speed/auth-core'
import authUiZhCN from '../../auth-ui/src/locales/zh-CN.json' with { type: 'json' }
import layoutKitZhCN from '../../layout-kit/src/locales/zh-CN.json' with { type: 'json' }
import { renderWithProviders } from '../test-utils/render.js'
import {
  jsonResponse,
  makePair,
  makeRealClientRig,
} from '../test-utils/real-client.js'
import { ProductShell } from './components/ProductShell.js'

const LOGIN_PASSWORD = 'POST /api/v1/authn/login/password'
const LOGOUT = 'POST /api/v1/authn/logout'
const IDENTIFIER = 'alice@example.com'
const PASSWORD = 'password-1'

// The rig of the quick start: one session over one real client, whose
// script covers exactly the two requests the journey makes -- a
// token-issuing password login and a 204 logout -- and fails loudly on
// anything else, so an unpinned request fails the test.
const rig = makeRealClientRig((call) => {
  const key = `${call.method} ${call.path}`
  if (key === LOGIN_PASSWORD) {
    return jsonResponse(200, makePair())
  }
  if (key === LOGOUT) {
    return new Response(null, { status: 204 })
  }
  throw new Error(`unexpected request: ${key}`)
})

// The quick start's bootstrap: the three namespaces the composed views
// render under, registered exactly once on the one instance (double
// registration throws), and the session attached before any render.
// Module scope matches the README; the single journey below is the only
// consumer of this instance, so nothing double-registers.
const i18n = createI18n({
  supportedLanguages: ['zh-CN', 'en-US'],
  defaultLanguage: 'zh-CN',
  storage: null,
  urlParameterName: null,
  navigatorLanguages: [],
})
registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)
registerNamespace(i18n, LAYOUT_KIT_NAMESPACE, layoutKitResources)
registerNamespace(i18n, AUTH_UI_NAMESPACE, authUiResources)
attachSession(rig.session)

/** The README's example tenant app, rendered under the provider tree
 * renderWithProviders builds around the shared instance above. */
function TenantApp({ session }: { readonly session: AuthSession }) {
  return (
    <ProductShell
      navItems={[{ id: 'home', label: 'Home', href: '/', selected: true }]}
      header="My App"
      signIn={<SignInScreen session={session} />}
      userMenu={<SignOutButton session={session} />}
    >
      <p>App content</p>
    </ProductShell>
  )
}

describe('README usage example', () => {
  it('drives the documented journey: sign in, frame, sign out, session ended, sign in again', async () => {
    const user = userEvent.setup()
    renderWithProviders(<TenantApp session={rig.session} />, { i18n })

    // The anonymous start: the host's sign-in surface, password channel
    // first by default -- and no app frame anywhere.
    const identifier = await screen.findByLabelText(
      authUiZhCN.passwordSignIn.identifierLabel,
    )
    expect(
      screen.getByRole('tab', { name: authUiZhCN.passwordSignIn.title }),
    ).toBeInTheDocument()
    expect(screen.queryByRole('banner')).not.toBeInTheDocument()
    expect(screen.queryByRole('main')).not.toBeInTheDocument()
    await user.type(identifier, IDENTIFIER)
    await user.type(
      screen.getByLabelText(authUiZhCN.passwordSignIn.passwordLabel),
      PASSWORD,
    )
    await user.click(
      screen.getByRole('button', { name: authUiZhCN.passwordSignIn.submit }),
    )

    // The sign-in committed: the frame appears around the app content,
    // with the host's chrome and the userMenu sign-out control.
    const main = await screen.findByRole('main')
    expect(main).toBeInTheDocument()
    expect(screen.getByRole('banner')).toBeInTheDocument()
    expect(
      screen.getByRole('navigation', {
        name: layoutKitZhCN.appShell.navLabel,
      }),
    ).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Home' })).toHaveAttribute(
      'href',
      '/',
    )
    expect(screen.getByText('My App')).toBeInTheDocument()
    expect(screen.getByText('App content')).toBeInTheDocument()
    expect(
      screen.queryByLabelText(authUiZhCN.passwordSignIn.identifierLabel),
    ).not.toBeInTheDocument()

    // Sign out through the userMenu slot: the frame is gone and the
    // default session-ended screen stands in its place (the machine
    // remembers the app was reached).
    await user.click(
      screen.getByRole('button', { name: authUiZhCN.signOut.label }),
    )
    await screen.findByText(authUiZhCN.sessionEnded.title)
    expect(
      screen.getByText(authUiZhCN.sessionEnded.description),
    ).toBeInTheDocument()
    expect(screen.queryByRole('banner')).not.toBeInTheDocument()
    expect(screen.queryByRole('main')).not.toBeInTheDocument()

    // The ended screen's action returns to the host's sign-in view.
    await user.click(
      screen.getByRole('button', { name: authUiZhCN.sessionEnded.signInAction }),
    )
    await screen.findByLabelText(authUiZhCN.passwordSignIn.identifierLabel)

    // Every request the journey made, pinned in order -- and the logout
    // travelled with the session's own access token.
    expect(rig.calls.map((call) => `${call.method} ${call.path}`)).toEqual([
      LOGIN_PASSWORD,
      LOGOUT,
    ])
    expect(rig.calls[1]?.authorization).toBe('Bearer access-1')
  })
})
