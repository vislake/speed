/**
 * usage-example.test.tsx -- compiles and runs the wiring the README's
 * quick start documents, end to end over a real @speed/api-client.
 *
 * Every step of the quick start executes here: the bilingual i18n
 * instance with both namespaces registered (the tenancy-ui namespace
 * for every string the switcher renders -- its inline error banner
 * included, a plain MUI alert under that namespace, never ui-kit
 * chrome; the ui-kit namespace because a host app composes under
 * ui-kit's AppThemeProvider and any ui-kit chrome the host renders
 * reads that namespace), the AppThemeProvider tree, the
 * host gate over the auth-core hooks -- anonymous viewers see a plain
 * sign-in button (this package is the auth-agnostic neighbour of
 * auth-ui, never its importer, so the host brings its own sign-in
 * affordance), authenticated viewers see the tenant switcher in the
 * header with currentTenantId flowing from useCurrentTenant -- the
 * createClient over a fetch stand-in, the memory access-token store,
 * refreshAccessToken: () => session.refresh() bound through the
 * api-sdk runtime seam (bindRequestFn, the same seam a host's real
 * client binds), and attachSession. The journey then signs in, switches
 * to a second tenant (the trigger label flips because the host hook
 * re-reads the committed snapshot), has that switch's counterpart
 * refused with authn.tenant_membership_required (the alert renders the
 * answer's code text, nothing changes and the trigger stays ready), and
 * retries into the first tenant again. The scripted server answers with
 * genuine Response objects, the pattern of auth-core's own real-client
 * legs and of auth-ui's usage example; a switch response mints an
 * access token and no refresh token (the spec's AuthnTokenPair says
 * so -- the held one keeps rotating), which the pinned request list
 * proves by the absence of any refresh leg.
 *
 * Why this file runs the real client while the component tests drive
 * the scripted session-harness: auth-core ships real-client legs in
 * its own session.test.ts, and this journey is the tenancy-ui consumer
 * proof -- the transport, envelope parsing and bearer attachment of
 * @speed/api-client run for real here. Component tests keep using the
 * harness, which is the right tool when a test must script raw
 * ApiErrors or inspect the RequestFn contract directly.
 */

import type { ReactElement } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {
  I18nextProvider,
  createI18n,
  registerNamespace,
} from '@speed/i18n'
import {
  UI_KIT_NAMESPACE,
  uiKitResources,
  AppThemeProvider,
} from '@speed/ui-kit'
import { attachSession, useAuthState, useCurrentTenant } from '@speed/auth-core'
import type { AuthSession } from '@speed/auth-core'
import type { AuthnTokenPair } from '@speed/api-sdk'
import {
  errorResponse,
  jsonResponse,
  makeRealClientRig,
} from '../test-utils/real-client.js'
import { makePair } from '../test-utils/session-harness.js'
import { TENANCY_UI_NAMESPACE, tenancyUiResources } from './resources.js'
import { TenantSwitcher } from './TenantSwitcher.js'
import type { TenantOption } from './TenantSwitcher.js'
import zhCN from './locales/zh-CN.json' with { type: 'json' }

// The quick start's tenant list -- the same three rows the README shows.
const TENANTS: readonly TenantOption[] = [
  { id: 'tenant-1', name: 'Sunshine Dental' },
  { id: 'tenant-2', name: 'Bright Smile Clinic' },
  { id: 'tenant-3', name: 'Harbor View Orthodontics' },
]

const LOGIN_PASSWORD = '/api/v1/authn/login/password'
const SWITCH_TENANT = '/api/v1/authn/tenant/switch'

/** A switch answer: a fresh access token for the target tenant and no
 * refresh token -- the held one keeps rotating (spec: AuthnTokenPair's
 * refresh_token is absent when the response did not mint one). */
function switchPair(accessToken: string, tenantId: string): AuthnTokenPair {
  return {
    access_token: accessToken,
    principal: {
      user_id: 'user-1',
      tenant_id: tenantId,
      session_id: 'session-1',
    },
  }
}

/** The host's app view in the quick start: the auth-core hooks gate
 * between the sign-in affordance and the app, whose header renders the
 * switcher with currentTenantId flowing from useCurrentTenant -- so a
 * committed switch re-renders the trigger -- and onSwitched re-reading
 * the tenant's data (the host's own query-cache discipline, see the
 * README). The host owns its sign-in affordance; this package is the
 * auth-agnostic tier below auth-ui and never imports it. */
function DemoHost({
  session,
  onSwitched,
}: {
  session: AuthSession
  onSwitched: (tenantId: string) => void
}): ReactElement {
  const auth = useAuthState()
  const currentTenant = useCurrentTenant()
  if (auth.state === 'anonymous') {
    return (
      <div>
        <h1>Smile Studio</h1>
        <button
          type="button"
          onClick={() =>
            void session.loginWithPassword({
              identifier: 'alice@example.com',
              password: 's3cret-pass',
            })
          }
        >
          Sign in
        </button>
      </div>
    )
  }
  return (
    <div>
      <header>
        <TenantSwitcher
          session={session}
          tenants={TENANTS}
          currentTenantId={currentTenant?.tenantId ?? null}
          onSwitched={onSwitched}
        />
      </header>
      <main>
        <h1>Smile Studio</h1>
        <p>Workspace dashboard</p>
      </main>
    </div>
  )
}

describe('the README quick start, exercised over a real api-client', () => {
  it('signs in, switches tenants, surfaces a refused switch and retries it', async () => {
    // The scripted server: the login mints the first pair; the first
    // switch (to tenant-2) mints access-2, the second (back to tenant-1)
    // refuses -- the caller's session does not hold that membership
    // right now -- and the retry of the same switch mints access-3.
    let switchAttempts = 0
    const rig = makeRealClientRig((call) => {
      switch (call.path) {
        case LOGIN_PASSWORD:
          return jsonResponse(200, makePair())
        case SWITCH_TENANT:
          switchAttempts += 1
          if (switchAttempts === 1) {
            return jsonResponse(200, switchPair('access-2', 'tenant-2'))
          }
          if (switchAttempts === 2) {
            return errorResponse(403, 'authn.tenant_membership_required')
          }
          return jsonResponse(200, switchPair('access-3', 'tenant-1'))
      }
      throw new Error(`no scripted answer for ${call.method} ${call.path}`)
    })

    // The quick start's bootstrap: attach the session to the hooks and
    // mount the host tree -- the i18n instance with both namespaces, the
    // theme provider and the hook gate.
    const onSwitched = vi.fn()
    attachSession(rig.session)
    const i18n = createI18n({
      supportedLanguages: ['zh-CN', 'en-US'],
      defaultLanguage: 'zh-CN',
      storage: null,
      urlParameterName: null,
      navigatorLanguages: [],
    })
    registerNamespace(i18n, TENANCY_UI_NAMESPACE, tenancyUiResources)
    registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)
    render(
      <I18nextProvider i18n={i18n}>
        <AppThemeProvider i18n={i18n}>
          <DemoHost session={rig.session} onSwitched={onSwitched} />
        </AppThemeProvider>
      </I18nextProvider>,
    )
    const user = userEvent.setup()

    // Anonymous start: the host's sign-in affordance.
    await user.click(screen.getByRole('button', { name: 'Sign in' }))
    expect(await screen.findByText('Workspace dashboard')).toBeInTheDocument()
    expect(rig.store.get()).toBe('access-1')

    // The trigger shows the signed-in principal's tenant and opens the
    // host-supplied list; the current row is disabled, the others are
    // not.
    await user.click(
      screen.getByRole('button', { name: 'Sunshine Dental' }),
    )
    expect(
      await screen.findByRole('menuitem', { name: 'Sunshine Dental' }),
    ).toHaveAttribute('aria-disabled', 'true')
    expect(
      screen.getByRole('menuitem', { name: 'Bright Smile Clinic' }),
    ).not.toHaveAttribute('aria-disabled')
    expect(
      screen.getByRole('menuitem', { name: 'Harbor View Orthodontics' }),
    ).not.toHaveAttribute('aria-disabled')

    // Pick the second tenant: the switch commits access-2 and the host
    // hook re-renders the trigger onto the new current tenant. Success
    // is quiet and the host callback fires once.
    await user.click(
      screen.getByRole('menuitem', { name: 'Bright Smile Clinic' }),
    )
    await waitFor(() => expect(rig.store.get()).toBe('access-2'))
    expect(onSwitched).toHaveBeenCalledTimes(1)
    expect(onSwitched).toHaveBeenCalledWith('tenant-2')
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    await waitFor(() =>
      expect(
        screen.getByRole('button', { name: 'Bright Smile Clinic' }),
      ).toBeEnabled(),
    )

    // Switch back: this one the server refuses. The alert renders the
    // answer's code text, the session holds access-2 untouched, the
    // host callback does not fire and the trigger stays on tenant-2,
    // ready to retry.
    await user.click(
      screen.getByRole('button', { name: 'Bright Smile Clinic' }),
    )
    await user.click(
      await screen.findByRole('menuitem', { name: 'Sunshine Dental' }),
    )
    expect(await screen.findByRole('alert')).toHaveTextContent(
      zhCN.errors.authn.tenant_membership_required,
    )
    expect(rig.store.get()).toBe('access-2')
    expect(onSwitched).toHaveBeenCalledTimes(1)
    expect(
      screen.getByRole('button', { name: 'Bright Smile Clinic' }),
    ).toBeEnabled()

    // Retry the same switch: the alert clears and access-3 commits; the
    // trigger flips back to the first tenant.
    await user.click(
      screen.getByRole('button', { name: 'Bright Smile Clinic' }),
    )
    await user.click(
      await screen.findByRole('menuitem', { name: 'Sunshine Dental' }),
    )
    await waitFor(() => expect(rig.store.get()).toBe('access-3'))
    expect(onSwitched).toHaveBeenCalledTimes(2)
    expect(onSwitched).toHaveBeenCalledWith('tenant-1')
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    await waitFor(() =>
      expect(
        screen.getByRole('button', { name: 'Sunshine Dental' }),
      ).toBeEnabled(),
    )

    // The whole exchange, in order: one login, three switch attempts,
    // no refresh leg anywhere (a switch response mints no refresh
    // token, so the held one kept rotating and nothing needed a
    // refresh). Each switch carried the bearer of the moment -- the
    // refused attempt travelled on the committed access-2 and changed
    // nothing, so the retry presented it again.
    expect(
      rig.calls.map((call) => `${call.method} ${call.path}`),
    ).toEqual([
      'POST /api/v1/authn/login/password',
      'POST /api/v1/authn/tenant/switch',
      'POST /api/v1/authn/tenant/switch',
      'POST /api/v1/authn/tenant/switch',
    ])
    const switchCalls = rig.calls.filter(
      (call) => call.path === SWITCH_TENANT,
    )
    expect(
      switchCalls.map((call) => call.authorization),
    ).toEqual([
      'Bearer access-1',
      'Bearer access-2',
      'Bearer access-2',
    ])
    expect(
      switchCalls.map((call) => JSON.parse(call.body ?? 'null')),
    ).toEqual([
      { tenant_id: 'tenant-2' },
      { tenant_id: 'tenant-1' },
      { tenant_id: 'tenant-1' },
    ])
  })
})
