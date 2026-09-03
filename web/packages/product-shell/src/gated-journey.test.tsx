/**
 * The host-side permission-gating composition over the product shell, as
 * package evidence.
 *
 * ProductShell itself gates nothing: it is the view machine between a
 * session and the AppShell frame, and route-level authorization is a
 * host concern composed inside its children (layout-kit's RouteGuard fed
 * by a status the host derives, auth-core's permission lists the host
 * attaches). This suite is the packaged proof that the composition works
 * end to end over the pieces the other packages ship -- the fixture host
 * below plays exactly the host role the README's "multi-tenant userMenu"
 * section and this package's AGENTS.md describe:
 *
 *   - the shell's authenticated frame with the tenant switcher
 *     (@speed/tenancy-ui) in the userMenu;
 *   - one destination per capability, each wrapped in layout-kit's
 *     RouteGuard with a status the host computes on every render from
 *     auth-core's usePermission over the lists it attached with
 *     session.setPermissionSet (never fetched or evaluated here);
 *   - the host's own role load: the fixture simulates the /me re-fetch a
 *     real host runs after a tenant switch commits, resolving it at the
 *     moment each journey wants the gate to lift (auth-core has already
 *     dropped the previous tenant's list by then -- the survival rules --
 *     so the route falls back to the pending gate and stays closed until
 *     the fresh list arrives);
 *   - a "Refresh roles" re-fetch that keeps the attached list while it
 *     is in flight, so the denied gate re-renders without re-entering
 *     (RouteGuard fires onDenied once per transition INTO denied, never
 *     per render).
 *
 * Journey 1 drives the gating across the tenancy-switch journey: an
 * allowed destination for one tenant's lists becomes pending the moment
 * a switch commits (the old list is gone), resolves to allowed or denied
 * when the new tenant's list arrives, refuses a switch to a tenant the
 * user is no member of (the error renders, the snapshot and the gate do
 * not move), and fires onDenied exactly once per denied spell, two
 * renders within one spell included. Journey 2 drives a server-side
 * session death to the same convergence the other suites prove: a
 * refused /me whose refresh is also refused signs the session out
 * locally, ProductShell replaces the frame with its session-ended
 * screen, and signing in again returns to the frame.
 *
 * Both journeys run over the real-client rig (the same bound client and
 * genuine Response objects the usage example uses), pinning every
 * request in order -- the switch requests' bodies and bearers included.
 * The fixture host renders English host content (nav labels, view text,
 * tenant names, its own buttons) -- test-file data standing in for a
 * host's own content, exempt from the no-literal-text rule -- while
 * every assertion on built-in strings imports the shipped sibling locale
 * bundles, never inline translations.
 */

import { act, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { Box, Button } from '@mui/material'
import { describe, expect, it, vi } from 'vitest'
import { registerNamespace } from '@speed/i18n'
import { RouteGuard } from '@speed/layout-kit'
import type { AppShellNavItem, RouteGuardStatus } from '@speed/layout-kit'
import {
  TENANCY_UI_NAMESPACE,
  tenancyUiResources,
  TenantSwitcher,
} from '@speed/tenancy-ui'
import type { TenantOption } from '@speed/tenancy-ui'
import { attachSession, useCurrentTenant, usePermission } from '@speed/auth-core'
import type { AuthSession } from '@speed/auth-core'
import { SignInScreen } from '@speed/auth-ui'
import { authnGetMe } from '@speed/api-sdk'
import authUiZhCN from '../../auth-ui/src/locales/zh-CN.json' with { type: 'json' }
import layoutKitZhCN from '../../layout-kit/src/locales/zh-CN.json' with { type: 'json' }
import tenancyUiZhCN from '../../tenancy-ui/src/locales/zh-CN.json' with { type: 'json' }
import uiKitZhCN from '../../ui-kit/src/locales/zh-CN.json' with { type: 'json' }
import { expectNoAxeViolations } from '../test-utils/axe.js'
import {
  createProductShellI18n,
  renderWithProviders,
} from '../test-utils/render.js'
import {
  errorResponse,
  jsonResponse,
  makePair,
  makeRealClientRig,
} from '../test-utils/real-client.js'
import { ProductShell } from './components/ProductShell.js'

const LOGIN_PASSWORD = 'POST /api/v1/authn/login/password'
const SWITCH_TENANT = 'POST /api/v1/authn/tenant/switch'
const ME = 'GET /api/v1/authn/me'
const REFRESH = 'POST /api/v1/authn/token/refresh'
const IDENTIFIER = 'alice@example.com'
const PASSWORD = 'password-1'

/** The host's tenant roster -- host data, the same fixture the
 * tenancy-ui README demo uses. tenant-3 is a tenant the user is not a
 * member of: the switcher's switch to it is refused by the server. */
const TENANTS: readonly TenantOption[] = [
  { id: 'tenant-1', name: 'Sunshine Dental' },
  { id: 'tenant-2', name: 'Bright Smile Clinic' },
  { id: 'tenant-3', name: 'Harbor View Orthodontics' },
]

/** Host data: the permission list each tenant's membership implies, the
 * list the fixture host re-attaches after a switch (auth-core drops the
 * tenant-domain list on switch -- the survival rules). tenant-1 can read
 * notes and manage members; tenant-2 can only read notes. */
const TENANT_ROLES = {
  'tenant-1': ['notes:read', 'members:manage'],
  'tenant-2': ['notes:read'],
} as const satisfies Readonly<Record<string, readonly string[]>>

type ViewId = 'home' | 'notes' | 'members'

const VIEWS: readonly { readonly id: ViewId; readonly label: string }[] = [
  { id: 'home', label: 'Overview' },
  { id: 'notes', label: 'Notes' },
  { id: 'members', label: 'Members' },
]

interface GatedTenantAppProps {
  readonly session: AuthSession
  /** The switcher's roster; host data. */
  readonly tenants: readonly TenantOption[]
  /**
   * The host's role-load queue: every load the fixture host starts (on
   * reaching an unsettled tenant, or on an explicit refresh) registers
   * its resolver here under the tenant id, so each journey resolves the
   * loads at the moment it wants the gate to lift. A real host would
   * fetch /me instead; this map is the seam the journey drives.
   */
  readonly roleLoads: Map<string, (permissions: readonly string[]) => void>
  /** The switcher's onSwitched; the fixture host's record of the event. */
  readonly onSwitched: (tenantId: string) => void
  /** Every RouteGuard's onDenied; the fixture host's record. */
  readonly onDenied: () => void
}

/**
 * The fixture host: the whole host side of the gating composition the
 * README documents, rendered inside ProductShell's authenticated frame.
 *
 * The host reads the session's hooks unconditionally (they fail closed
 * before any sign-in), derives each destination's RouteGuardStatus on
 * every render from usePermission over the lists it attached, and
 * re-attaches the lists itself when a role load completes -- the
 * setPermissionSet host duty this package's AGENTS.md keeps assigning
 * to hosts, played here for real.
 */
function GatedTenantApp({
  session,
  tenants,
  roleLoads,
  onSwitched,
  onDenied,
}: GatedTenantAppProps) {
  const current = useCurrentTenant()
  const tenantId = current?.tenantId ?? null
  const canReadNotes = usePermission('tenant', 'notes:read')
  const canManageMembers = usePermission('tenant', 'members:manage')
  const [view, setView] = useState<ViewId>('home')
  // The tenants whose role lists have been attached at least once; a
  // tenant missing from this set is still awaiting its first load, so
  // every gate under it reads pending -- never a stale allow.
  const [settled, setSettled] = useState<ReadonlySet<string>>(
    () => new Set(),
  )
  const [refreshing, setRefreshing] = useState(false)

  // The tenant a role-load resolution must attach for: a switch that
  // commits while the previous tenant's list is still in flight must
  // not attach it to the wrong tenant. Read at resolution time, synced
  // from the committed principal after every commit.
  const tenantRef = useRef<string | null>(null)
  useEffect(() => {
    tenantRef.current = tenantId
  }, [tenantId])

  // Reaching an unsettled tenant starts its role load (the effect runs
  // once per principal commit: after the first login, and after each
  // switch, whose commit already dropped the previous tenant's list).
  useEffect(() => {
    if (tenantId === null || settled.has(tenantId)) {
      return
    }
    // The tenant this load attaches for, frozen at start: the
    // resolution runs later, when the principal may have moved on.
    const tenant = tenantId
    const load = new Promise<readonly string[]>((resolve) => {
      roleLoads.set(tenant, resolve)
    })
    load.then((permissions) => {
      if (tenantRef.current !== tenant) {
        return
      }
      session.setPermissionSet('tenant', permissions)
      setSettled((previous) =>
        previous.has(tenant) ? previous : new Set(previous).add(tenant),
      )
    })
  }, [tenantId, settled, roleLoads, session])

  // The host's re-fetch: it keeps the attached list while the fresh one
  // is in flight, so an allowed or denied gate re-renders without ever
  // leaving its status -- RouteGuard must not re-fire onDenied for it.
  function handleRefreshRoles(): void {
    if (tenantId === null || !settled.has(tenantId)) {
      return
    }
    const tenant = tenantId
    setRefreshing(true)
    const load = new Promise<readonly string[]>((resolve) => {
      roleLoads.set(tenant, resolve)
    })
    load.then((permissions) => {
      if (tenantRef.current === tenant) {
        session.setPermissionSet('tenant', permissions)
      }
      setRefreshing(false)
    })
  }

  // A protected-call probe the host's home view offers: the composed
  // chain a real view runs (a generated operation over the bound
  // client). A refusal triggers the client's silent refresh leg; if the
  // refresh is refused too, the session signs itself out and the shell
  // replaces this whole view -- nothing to render here.
  async function checkSession(): Promise<void> {
    try {
      await authnGetMe()
    } catch {
      // The session layer already converged; see the module comment.
    }
  }

  function gate(destination: 'notes' | 'members'): RouteGuardStatus {
    if (tenantId === null || !settled.has(tenantId)) {
      return 'pending'
    }
    const can = destination === 'notes' ? canReadNotes : canManageMembers
    return can ? 'allowed' : 'denied'
  }

  const navItems: readonly AppShellNavItem[] = VIEWS.map((entry) => ({
    id: entry.id,
    label: entry.label,
    selected: view === entry.id,
    onClick: () => setView(entry.id),
  }))

  let content: ReactNode
  if (view === 'home') {
    content = (
      <>
        <h2>Overview</h2>
        <p>The default landing view, reachable by every member.</p>
        <Button onClick={() => void checkSession()}>Check session</Button>
      </>
    )
  } else if (view === 'notes') {
    content = (
      <RouteGuard status={gate('notes')} onDenied={onDenied}>
        <h2>Notes</h2>
        <p>The tenant&apos;s notes list.</p>
      </RouteGuard>
    )
  } else {
    content = (
      <RouteGuard status={gate('members')} onDenied={onDenied}>
        <h2>Members</h2>
        <p>The tenant&apos;s member roster.</p>
      </RouteGuard>
    )
  }

  return (
    <ProductShell
      navItems={navItems}
      header="My App"
      signIn={<SignInScreen session={session} />}
      userMenu={
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
          <TenantSwitcher
            session={session}
            tenants={tenants}
            currentTenantId={tenantId}
            onSwitched={onSwitched}
          />
        </Box>
      }
    >
      {view !== 'home' && (
        <Box sx={{ display: 'flex', justifyContent: 'flex-end' }}>
          <Button disabled={refreshing} onClick={handleRefreshRoles}>
            Refresh roles
          </Button>
        </Box>
      )}
      {content}
    </ProductShell>
  )
}

/** Fresh instance with the tenancy-ui namespace added on top of the
 * shell trio, the four namespaces a host composing the switcher
 * registers. */
function createGatedI18n() {
  const i18n = createProductShellI18n()
  registerNamespace(i18n, TENANCY_UI_NAMESPACE, tenancyUiResources)
  return i18n
}

/** A password sign-in through the host's own SignInScreen. */
async function signIn(user: ReturnType<typeof userEvent.setup>) {
  const identifier = await screen.findByLabelText(
    authUiZhCN.passwordSignIn.identifierLabel,
  )
  await user.type(identifier, IDENTIFIER)
  await user.type(
    screen.getByLabelText(authUiZhCN.passwordSignIn.passwordLabel),
    PASSWORD,
  )
  await user.click(
    screen.getByRole('button', { name: authUiZhCN.passwordSignIn.submit }),
  )
  await screen.findByRole('main')
}

describe('host-side gating composition', () => {
  it('gates every destination from the session lists across a switch, a denial spell and a refused switch', async () => {
    const user = userEvent.setup()
    const roleLoads = new Map<
      string,
      (permissions: readonly string[]) => void
    >()
    const onSwitched = vi.fn<(tenantId: string) => void>()
    const onDenied = vi.fn()
    const rig = makeRealClientRig((call) => {
      const key = `${call.method} ${call.path}`
      if (key === LOGIN_PASSWORD) {
        return jsonResponse(200, makePair())
      }
      if (key === SWITCH_TENANT) {
        const body = JSON.parse(call.body ?? 'null') as {
          readonly tenant_id?: string
        }
        if (body.tenant_id === 'tenant-2') {
          // The switch mints an access token and no refresh token.
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
        if (body.tenant_id === 'tenant-3') {
          return errorResponse(403, 'authn.tenant_membership_required')
        }
        throw new Error(`unexpected switch target: ${call.body}`)
      }
      throw new Error(`unexpected request: ${key}`)
    })
    act(() => attachSession(rig.session))
    renderWithProviders(
      <GatedTenantApp
        session={rig.session}
        tenants={TENANTS}
        roleLoads={roleLoads}
        onSwitched={onSwitched}
        onDenied={onDenied}
      />,
      { i18n: createGatedI18n() },
    )

    // Sign in through the host's sign-in surface: the frame appears
    // with the tenant switcher showing the signed-in tenant, and the
    // first tenant's role load is already in flight (home is exempt
    // from gating, so the frame is usable while it loads).
    await signIn(user)
    expect(
      screen.getByRole('button', { name: 'Sunshine Dental' }),
    ).toBeEnabled()
    expect(roleLoads.has('tenant-1')).toBe(true)
    expect(screen.getByRole('heading', { name: 'Overview' })).toBeInTheDocument()
    expect(onDenied).not.toHaveBeenCalled()

    // Notes: the gate reads pending until the host's first role load
    // lands -- the spinner carries layout-kit's own pending label --
    // then allows the view.
    await user.click(screen.getByRole('button', { name: 'Notes' }))
    expect(
      await screen.findByRole('progressbar', {
        name: layoutKitZhCN.routeGuard.pending,
      }),
    ).toBeInTheDocument()
    expect(
      screen.queryByRole('heading', { name: 'Notes' }),
    ).not.toBeInTheDocument()
    await act(async () => {
      const resolve = roleLoads.get('tenant-1')
      if (resolve === undefined) {
        throw new Error('expected a pending role load for tenant-1')
      }
      resolve(TENANT_ROLES['tenant-1'])
    })
    expect(
      await screen.findByRole('heading', { name: 'Notes' }),
    ).toBeInTheDocument()
    expect(
      screen.queryByRole('progressbar', {
        name: layoutKitZhCN.routeGuard.pending,
      }),
    ).not.toBeInTheDocument()
    expect(onDenied).not.toHaveBeenCalled()

    // Members: allowed for tenant-1's lists (members:manage attached
    // above); the frame is axe-clean in this allowed state.
    await user.click(screen.getByRole('button', { name: 'Members' }))
    expect(
      await screen.findByRole('heading', { name: 'Members' }),
    ).toBeInTheDocument()
    await expectNoAxeViolations()

    // The switch commits: auth-core's survival rules dropped the
    // tenant-domain list the moment tenant-2's principal landed, and
    // tenant-2's list has not arrived yet -- so the Members gate falls
    // back to pending and the previously allowed content is gone. The
    // store carries the new access token and the switcher relabels.
    await user.click(screen.getByRole('button', { name: 'Sunshine Dental' }))
    await user.click(
      await screen.findByRole('menuitem', { name: 'Bright Smile Clinic' }),
    )
    expect(
      await screen.findByRole('progressbar', {
        name: layoutKitZhCN.routeGuard.pending,
      }),
    ).toBeInTheDocument()
    expect(
      screen.queryByRole('heading', { name: 'Members' }),
    ).not.toBeInTheDocument()
    expect(rig.store.get()).toBe('access-2')
    expect(onSwitched).toHaveBeenCalledTimes(1)
    expect(onSwitched).toHaveBeenCalledWith('tenant-2')
    expect(
      screen.getByRole('button', { name: 'Bright Smile Clinic' }),
    ).toBeEnabled()
    expect(onDenied).not.toHaveBeenCalled()

    // The host's re-fetch for tenant-2 lands with only notes:read --
    // Members is now denied, behind ui-kit's own noPermission
    // EmptyState, and onDenied fired exactly once for the transition.
    await act(async () => {
      const resolve = roleLoads.get('tenant-2')
      if (resolve === undefined) {
        throw new Error('expected a pending role load for tenant-2')
      }
      resolve(TENANT_ROLES['tenant-2'])
    })
    expect(
      await screen.findByText(uiKitZhCN.emptyState.noPermission.title),
    ).toBeInTheDocument()
    expect(
      screen.getByText(uiKitZhCN.emptyState.noPermission.description),
    ).toBeInTheDocument()
    expect(
      screen.queryByRole('heading', { name: 'Members' }),
    ).not.toBeInTheDocument()
    expect(onDenied).toHaveBeenCalledTimes(1)

    // The denied frame is axe-clean too.
    await expectNoAxeViolations()

    // A role refresh inside the denied spell: the host keeps the
    // attached list while the fresh copy is in flight, so the gate
    // re-renders -- twice -- without ever leaving 'denied', and
    // onDenied does not re-fire.
    await user.click(screen.getByRole('button', { name: 'Refresh roles' }))
    expect(
      screen.getByRole('button', { name: 'Refresh roles' }),
    ).toBeDisabled()
    expect(onDenied).toHaveBeenCalledTimes(1)
    await act(async () => {
      const resolve = roleLoads.get('tenant-2')
      if (resolve === undefined) {
        throw new Error('expected a pending role refresh for tenant-2')
      }
      resolve(TENANT_ROLES['tenant-2'])
    })
    expect(
      await screen.findByRole('button', { name: 'Refresh roles' }),
    ).toBeEnabled()
    expect(
      screen.getByText(uiKitZhCN.emptyState.noPermission.title),
    ).toBeInTheDocument()
    expect(onDenied).toHaveBeenCalledTimes(1)

    // Leaving the denied view and coming back is a fresh denied spell:
    // the guard remounts and onDenied fires again -- once per visit.
    await user.click(screen.getByRole('button', { name: 'Overview' }))
    expect(screen.getByRole('heading', { name: 'Overview' })).toBeInTheDocument()
    expect(onDenied).toHaveBeenCalledTimes(1)
    await user.click(screen.getByRole('button', { name: 'Members' }))
    expect(
      await screen.findByText(uiKitZhCN.emptyState.noPermission.title),
    ).toBeInTheDocument()
    expect(onDenied).toHaveBeenCalledTimes(2)

    // Notes, by contrast, is allowed under tenant-2's list.
    await user.click(screen.getByRole('button', { name: 'Notes' }))
    expect(
      await screen.findByRole('heading', { name: 'Notes' }),
    ).toBeInTheDocument()

    // A refused switch to a tenant the user is not a member of: the
    // tenancy-ui error renders the code's text, and nothing moves --
    // the store still carries tenant-2's token, the switcher stays on
    // Bright Smile Clinic and stays enabled, the gate and onDenied are
    // untouched.
    await user.click(
      screen.getByRole('button', { name: 'Bright Smile Clinic' }),
    )
    await user.click(
      await screen.findByRole('menuitem', { name: 'Harbor View Orthodontics' }),
    )
    expect(
      await screen.findByRole('alert'),
    ).toHaveTextContent(tenancyUiZhCN.errors.authn.tenant_membership_required)
    expect(rig.store.get()).toBe('access-2')
    expect(onSwitched).toHaveBeenCalledTimes(1)
    expect(
      screen.getByRole('button', { name: 'Bright Smile Clinic' }),
    ).toBeEnabled()
    expect(screen.getByRole('heading', { name: 'Notes' })).toBeInTheDocument()
    expect(onDenied).toHaveBeenCalledTimes(2)

    // The switcher is retryable: the menu opens again with the full
    // roster.
    await user.click(screen.getByRole('button', { name: 'Bright Smile Clinic' }))
    expect(
      await screen.findByRole('menuitem', { name: 'Harbor View Orthodontics' }),
    ).toBeInTheDocument()
    expect(
      screen.getByRole('menuitem', { name: 'Sunshine Dental' }),
    ).toBeInTheDocument()

    // Every request the journey made, pinned in order: the login, the
    // accepted switch to tenant-2 and the refused switch to tenant-3,
    // each with the bearer it travelled with and the body it sent.
    expect(rig.calls.map((call) => `${call.method} ${call.path}`)).toEqual([
      LOGIN_PASSWORD,
      SWITCH_TENANT,
      SWITCH_TENANT,
    ])
    expect(rig.calls[1]?.authorization).toBe('Bearer access-1')
    expect(JSON.parse(rig.calls[1]?.body ?? 'null')).toEqual({
      tenant_id: 'tenant-2',
    })
    expect(rig.calls[2]?.authorization).toBe('Bearer access-2')
    expect(JSON.parse(rig.calls[2]?.body ?? 'null')).toEqual({
      tenant_id: 'tenant-3',
    })
  })

  it('converges a server-side session death to the session-ended screen and back into the frame', async () => {
    const user = userEvent.setup()
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    try {
      const roleLoads = new Map<
        string,
        (permissions: readonly string[]) => void
      >()
      const onSwitched = vi.fn<(tenantId: string) => void>()
      const onDenied = vi.fn()
      const rig = makeRealClientRig((call) => {
        const key = `${call.method} ${call.path}`
        if (key === LOGIN_PASSWORD) {
          return jsonResponse(200, makePair())
        }
        if (key === ME) {
          return errorResponse(401, 'authn.session_revoked')
        }
        if (key === REFRESH) {
          return errorResponse(401, 'authn.refresh_token_invalid')
        }
        throw new Error(`unexpected request: ${key}`)
      })
      act(() => attachSession(rig.session))
      renderWithProviders(
        <GatedTenantApp
          session={rig.session}
          tenants={TENANTS}
          roleLoads={roleLoads}
          onSwitched={onSwitched}
          onDenied={onDenied}
        />,
        { i18n: createGatedI18n() },
      )

      // Sign in and reach the frame, then hit the host's protected-call
      // probe: the server refuses the session (authn.session_revoked),
      // the client's silent refresh leg is refused too
      // (authn.refresh_token_invalid), and the session signs itself out
      // locally -- the store clears and ProductShell replaces the whole
      // frame with its default session-ended screen.
      await signIn(user)
      expect(
        screen.getByRole('heading', { name: 'Overview' }),
      ).toBeInTheDocument()
      await user.click(screen.getByRole('button', { name: 'Check session' }))
      expect(
        await screen.findByText(authUiZhCN.sessionEnded.title),
      ).toBeInTheDocument()
      expect(
        screen.getByText(authUiZhCN.sessionEnded.description),
      ).toBeInTheDocument()
      expect(screen.queryByRole('banner')).not.toBeInTheDocument()
      expect(screen.queryByRole('main')).not.toBeInTheDocument()
      expect(rig.store.get()).toBeNull()
      // The client reports the failed refresh through its reporter with
      // the original refusal's code, never the refresh's own.
      await waitFor(() =>
        expect(warn).toHaveBeenCalledWith(
          'access token refresh failed',
          expect.objectContaining({
            status: 401,
            code: 'authn.session_revoked',
          }),
        ),
      )

      // The ended screen's action returns to the host's sign-in view.
      await user.click(
        screen.getByRole('button', { name: authUiZhCN.sessionEnded.signInAction }),
      )
      await screen.findByLabelText(authUiZhCN.passwordSignIn.identifierLabel)

      // A fresh sign-in reaches the frame again.
      await signIn(user)
      expect(
        screen.getByRole('heading', { name: 'Overview' }),
      ).toBeInTheDocument()
      expect(rig.store.get()).toBe('access-1')

      // Every request the journey made, pinned in order -- the refused
      // /me travelled with the session's own access token, and the
      // refresh leg travelled credential-less by declaration.
      expect(rig.calls.map((call) => `${call.method} ${call.path}`)).toEqual([
        LOGIN_PASSWORD,
        ME,
        REFRESH,
        LOGIN_PASSWORD,
      ])
      expect(rig.calls[1]?.authorization).toBe('Bearer access-1')
      expect(rig.calls[2]?.authorization).toBeNull()
    } finally {
      warn.mockRestore()
    }
  })
})
