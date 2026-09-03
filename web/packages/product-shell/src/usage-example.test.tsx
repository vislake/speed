/**
 * The README usage example, compiled and executed by the suite.
 *
 * The README's Quick start composes the whole tenant-facing shell: the
 * namespaces registered once on one i18n instance, one session attached
 * before render, and a ProductShell whose signIn slot is auth-ui's
 * SignInScreen, whose userMenu slot carries the tenant switcher
 * (@speed/tenancy-ui, the README's "multi-tenant userMenu" section)
 * beside auth-ui's SignOutButton, and whose frame wraps the host's own
 * app content. This file renders that exact composition and drives one
 * minimal journey through it -- a password sign-in, the frame appearing,
 * a tenant switch through the userMenu (the committed principal flips
 * the trigger; the switch mints an access token and no refresh token, so
 * no refresh leg ever appears), a sign-out through the same menu, the
 * default session-ended screen, and a return to the sign-in view -- over
 * the real-client rig (a genuine @speed/api-client bound through the
 * same bindRequestFn seam a host's real client binds), pinning every
 * request in order -- bodies included -- so the documented usage cannot
 * drift from the API. This example stays ungated by design; the host's
 * permission-gating composition over the same frame is the
 * gated-journey suite's (gated-journey.test.tsx). Host-content strings
 * (nav labels, the app title and content, tenant names) are English
 * fixtures on purpose: they stand in for a host's own content and are
 * data in a test file (exempt from the no-literal-text rule), not
 * rendered product text. Assertions derive every built-in string from
 * the shipped sibling locale bundles, never inline translations.
 */

import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { screen } from '@testing-library/react'
import { Box } from '@mui/material'
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
import {
  TENANCY_UI_NAMESPACE,
  tenancyUiResources,
  TenantSwitcher,
} from '@speed/tenancy-ui'
import type { TenantOption } from '@speed/tenancy-ui'
import { attachSession, useCurrentTenant } from '@speed/auth-core'
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
const SWITCH_TENANT = 'POST /api/v1/authn/tenant/switch'
const LOGOUT = 'POST /api/v1/authn/logout'
const IDENTIFIER = 'alice@example.com'
const PASSWORD = 'password-1'

/** The host's tenant roster -- host data, the same fixture the tenancy-ui
 * README demo uses. The switcher renders these names as host content. */
const TENANTS: readonly TenantOption[] = [
  { id: 'tenant-1', name: 'Sunshine Dental' },
  { id: 'tenant-2', name: 'Bright Smile Clinic' },
  { id: 'tenant-3', name: 'Harbor View Orthodontics' },
]

// The host's switch callback, wired into the userMenu. A real host
// re-attaches the tenant-domain permission lists (auth-core drops them
// on switch) and drops the previous tenant's query cache here. This
// example attaches no permission lists -- it gates nothing, by design;
// the gated-journey suite is where that host duty is played for real --
// so the callback only records the event for the journey to pin.
const onSwitched = vi.fn<(tenantId: string) => void>()

// The rig of the quick start: one session over one real client, whose
// script covers exactly the three requests the journey makes -- a
// token-issuing password login, a tenant switch answering with a new
// access token and no refresh token (the shape the authn API returns),
// and a 204 logout -- and fails loudly on anything else, so an unpinned
// request fails the test.
const rig = makeRealClientRig((call) => {
  const key = `${call.method} ${call.path}`
  if (key === LOGIN_PASSWORD) {
    return jsonResponse(200, makePair())
  }
  if (key === SWITCH_TENANT) {
    return jsonResponse(
      200,
      makePair({
        access_token: 'access-2',
        refresh_token: undefined,
        principal: {
          user_id: 'user-1',
          tenant_id: 'tenant-2',
          session_id: 'session-1',
        },
      }),
    )
  }
  if (key === LOGOUT) {
    return new Response(null, { status: 204 })
  }
  throw new Error(`unexpected request: ${key}`)
})

// The quick start's bootstrap: the namespaces the composed views render
// under -- the host's shell trio plus tenancy-ui's own, whose switcher
// sits in the userMenu -- registered exactly once on the one instance
// (double registration throws), and the session attached before any
// render. Module scope matches the README; the single journey below is
// the only consumer of this instance, so nothing double-registers.
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
registerNamespace(i18n, TENANCY_UI_NAMESPACE, tenancyUiResources)
attachSession(rig.session)

/** The README multi-tenant section's userMenu: the host's own menu bar
 * (host content) -- the current tenant read through auth-core's
 * useCurrentTenant feeds tenancy-ui's switcher, with auth-ui's sign-out
 * button beside it. */
function UserMenu({
  session,
  onSwitched,
}: {
  readonly session: AuthSession
  readonly onSwitched: (tenantId: string) => void
}) {
  const current = useCurrentTenant()
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
      <TenantSwitcher
        session={session}
        tenants={TENANTS}
        currentTenantId={current?.tenantId ?? null}
        onSwitched={onSwitched}
      />
      <SignOutButton session={session} />
    </Box>
  )
}

/** The README's example tenant app, rendered under the provider tree
 * renderWithProviders builds around the shared instance above. */
function TenantApp({
  session,
  onSwitched,
}: {
  readonly session: AuthSession
  readonly onSwitched: (tenantId: string) => void
}) {
  return (
    <ProductShell
      navItems={[{ id: 'home', label: 'Home', href: '/', selected: true }]}
      header="My App"
      signIn={<SignInScreen session={session} />}
      userMenu={<UserMenu session={session} onSwitched={onSwitched} />}
    >
      <p>App content</p>
    </ProductShell>
  )
}

describe('README usage example', () => {
  it('drives the documented journey: sign in, frame, tenant switch, sign out, session ended, sign in again', async () => {
    const user = userEvent.setup()
    renderWithProviders(
      <TenantApp session={rig.session} onSwitched={onSwitched} />,
      { i18n },
    )

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
    // with the host's chrome and the userMenu controls -- the tenant
    // switcher showing the signed-in tenant, and the sign-out button.
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
      screen.getByRole('button', { name: 'Sunshine Dental' }),
    ).toBeEnabled()
    expect(
      screen.queryByLabelText(authUiZhCN.passwordSignIn.identifierLabel),
    ).not.toBeInTheDocument()

    // The multi-tenant turn: the userMenu's switcher shows the host's
    // roster, and switching to Bright Smile Clinic commits the new
    // tenant's principal -- the access-token store flips, the host's
    // onSwitched fires with the tenant id, and the trigger relabels
    // itself with the new tenant's name. The switch mints an access
    // token and no refresh token, so no refresh leg ever appears; the
    // frame itself does not change (nothing tenant-dependent is in the
    // quick start's content -- permission gating is the gated-journey
    // suite's business).
    await user.click(screen.getByRole('button', { name: 'Sunshine Dental' }))
    await user.click(
      await screen.findByRole('menuitem', { name: 'Bright Smile Clinic' }),
    )
    expect(
      await screen.findByRole('button', { name: 'Bright Smile Clinic' }),
    ).toBeEnabled()
    expect(rig.store.get()).toBe('access-2')
    expect(onSwitched).toHaveBeenCalledTimes(1)
    expect(onSwitched).toHaveBeenCalledWith('tenant-2')

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

    // Every request the journey made, pinned in order -- the switch's
    // body, and the bearer each leg travelled with: the switch carried
    // the original tenant's token, and the logout carried the
    // switched-to tenant's.
    expect(rig.calls.map((call) => `${call.method} ${call.path}`)).toEqual([
      LOGIN_PASSWORD,
      SWITCH_TENANT,
      LOGOUT,
    ])
    expect(rig.calls[1]?.authorization).toBe('Bearer access-1')
    expect(JSON.parse(rig.calls[1]?.body ?? 'null')).toEqual({
      tenant_id: 'tenant-2',
    })
    expect(rig.calls[2]?.authorization).toBe('Bearer access-2')
  })
})
