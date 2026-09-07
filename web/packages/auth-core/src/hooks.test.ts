// @vitest-environment jsdom
/**
 * hooks.test.ts -- useAuthState / useCurrentTenant / usePermission /
 * attachSession (hooks.ts): the fail-closed reads before any session
 * is attached, the tenant and permission values over a scripted
 * session, the re-renders on set changes, domain separation and the
 * last-bind-wins attach semantics. Everything flows through the real
 * session driven by the shared test-utils/session-harness.ts -- no
 * mocks of hooks.ts internals.
 *
 * The hooks' module-level attach state is pristine only until this
 * file's first attachSession call, so the no-session describe runs
 * first (vitest executes tests within a file in declaration order).
 *
 * The @vitest-environment pragma is per-file: the session state
 * machine tests run in the plain node environment, these hooks need a
 * DOM. Rendering leaves components mounted across tests, so cleanup
 * runs explicitly after each (vitest runs without globals, which
 * disables @testing-library/react's auto-cleanup).
 */

import { act, cleanup, renderHook } from '@testing-library/react'
import { createElement } from 'react'
import { renderToString } from 'react-dom/server'
import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  LOGIN_PASSWORD,
  LOGOUT,
  makeHarness,
  makePair,
  principal,
  REFRESH,
  SWITCH_TENANT,
} from '../test-utils/session-harness'
import {
  attachSession,
  useAuthState,
  useCurrentTenant,
  usePermission,
} from './hooks'

afterEach(() => {
  cleanup()
})

const credentials = {
  identifier: 'ada@example.com',
  password: 'pw',
}

describe('before any session is attached', () => {
  it('useAuthState fails closed on the stable anonymous snapshot', () => {
    const { result, rerender } = renderHook(() => useAuthState())
    expect(result.current).toEqual({
      state: 'anonymous',
      principal: null,
      permissionSets: { tenant: null, system: null },
    })
    // The no-session snapshot is referentially stable across renders,
    // so useSyncExternalStore never re-renders on its own.
    const first = result.current
    rerender()
    expect(result.current).toBe(first)
  })

  it('useCurrentTenant and usePermission fail closed', () => {
    const { result: tenant } = renderHook(() => useCurrentTenant())
    const { result: tenantPerm } = renderHook(() =>
      usePermission('tenant', 'notes:read'),
    )
    const { result: systemPerm } = renderHook(() =>
      usePermission('system', 'users:manage'),
    )
    expect(tenant.current).toBeNull()
    expect(tenantPerm.current).toBe(false)
    expect(systemPerm.current).toBe(false)
  })
})

describe('attached to a scripted session', () => {
  it('useCurrentTenant tracks anonymous -> login -> tenant switch', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SWITCH_TENANT]: () =>
        makePair({
          access_token: 'access-2',
          principal: principal('user-1', 'tenant-2'),
        }),
    })
    attachSession(harness.session)
    const { result } = renderHook(() => useCurrentTenant())
    // Attached but not signed in: still null.
    expect(result.current).toBeNull()

    await act(async () => {
      await harness.session.loginWithPassword(credentials)
    })
    expect(result.current).toEqual({ tenantId: 'tenant-1' })

    await act(async () => {
      await harness.session.switchTenant('tenant-2')
    })
    expect(result.current).toEqual({ tenantId: 'tenant-2' })
  })

  it('keeps one referentially stable tenant object for the whole stay in a tenant', async () => {
    // P3-12: useCurrentTenant used to mint a fresh { tenantId }
    // object on every render, so a consumer embedding the result in a
    // memoized query key or an effect dependency saw a new identity on
    // every re-render -- the referential instability that silently
    // defeated a host's tenant-namespaced cache keys. The object
    // identity now changes only when the tenant_id itself changes.
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [REFRESH]: () => makePair({ access_token: 'access-3' }),
      [SWITCH_TENANT]: () =>
        makePair({
          access_token: 'access-2',
          principal: principal('user-1', 'tenant-2'),
        }),
    })
    attachSession(harness.session)
    const { result } = renderHook(() => useCurrentTenant())
    await act(async () => {
      await harness.session.loginWithPassword(credentials)
    })
    const first = result.current
    expect(first).toEqual({ tenantId: 'tenant-1' })
    // A same-tenant re-login and a silent refresh -- transitions that
    // used to hand every re-render a fresh object -- keep the identity.
    await act(async () => {
      await harness.session.loginWithPassword(credentials)
    })
    expect(result.current).toBe(first)
    await act(async () => {
      await harness.session.refresh()
    })
    expect(result.current).toBe(first)
    // The identity changes exactly when the tenant does.
    await act(async () => {
      await harness.session.switchTenant('tenant-2')
    })
    expect(result.current).not.toBe(first)
    expect(result.current).toEqual({ tenantId: 'tenant-2' })
  })

  it('useCurrentTenant reads null for a principal without a tenant_id', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      // A system-domain principal: no tenant_id in the token claims.
      [SWITCH_TENANT]: () =>
        makePair({
          access_token: 'access-2',
          principal: { user_id: 'user-1', session_id: 'session-1' },
        }),
    })
    attachSession(harness.session)
    const { result } = renderHook(() => useCurrentTenant())
    await act(async () => {
      await harness.session.loginWithPassword(credentials)
    })
    expect(result.current).toEqual({ tenantId: 'tenant-1' })
    await act(async () => {
      await harness.session.switchTenant('tenant-2')
    })
    expect(result.current).toBeNull()
  })

  it('an unmounted component unsubscribes; live ones keep receiving updates', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SWITCH_TENANT]: () =>
        makePair({
          access_token: 'access-2',
          principal: principal('user-1', 'tenant-2'),
        }),
    })
    attachSession(harness.session)
    const gone = renderHook(() => useCurrentTenant())
    const live = renderHook(() => useCurrentTenant())
    await act(async () => {
      await harness.session.loginWithPassword(credentials)
    })
    expect(gone.result.current).toEqual({ tenantId: 'tenant-1' })
    expect(live.result.current).toEqual({ tenantId: 'tenant-1' })

    gone.unmount()
    // A later transition reaches the remaining subscriber and does not
    // throw for the unmounted one.
    await act(async () => {
      await harness.session.switchTenant('tenant-2')
    })
    expect(live.result.current).toEqual({ tenantId: 'tenant-2' })
  })

  it('usePermission fails closed until the domain set holds the permission', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
    })
    attachSession(harness.session)
    const { result } = renderHook(() => usePermission('tenant', 'notes:read'))
    // Anonymous: false.
    expect(result.current).toBe(false)
    await act(async () => {
      await harness.session.loginWithPassword(credentials)
    })
    // Authenticated, but no set attached yet: false.
    expect(result.current).toBe(false)
    act(() => {
      harness.session.setPermissionSet('tenant', ['notes:write'])
    })
    // A set is attached, but the permission is not in it: false.
    expect(result.current).toBe(false)
    act(() => {
      harness.session.setPermissionSet('tenant', ['notes:read'])
    })
    expect(result.current).toBe(true)
  })

  it("a domain's set never satisfies a check in the other domain", async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
    })
    attachSession(harness.session)
    await act(async () => {
      await harness.session.loginWithPassword(credentials)
    })
    const { result: tenantRead } = renderHook(() =>
      usePermission('tenant', 'notes:read'),
    )
    const { result: systemRead } = renderHook(() =>
      usePermission('system', 'notes:read'),
    )
    act(() => {
      harness.session.setPermissionSet('tenant', ['notes:read'])
    })
    expect(tenantRead.current).toBe(true)
    // The system domain has no set: the tenant set is not consulted.
    expect(systemRead.current).toBe(false)

    act(() => {
      harness.session.setPermissionSet('system', ['users:manage'])
    })
    // Still false: 'notes:read' is not in the system set either.
    expect(systemRead.current).toBe(false)

    const { result: tenantUsers } = renderHook(() =>
      usePermission('tenant', 'users:manage'),
    )
    expect(tenantUsers.current).toBe(false)
  })

  it('set changes re-render the selectors through the shared snapshot', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [LOGOUT]: () => undefined,
    })
    attachSession(harness.session)
    await act(async () => {
      await harness.session.loginWithPassword(credentials)
    })
    const { result: state } = renderHook(() => useAuthState())
    const { result: can } = renderHook(() => usePermission('tenant', 'notes:read'))
    act(() => {
      harness.session.setPermissionSet('tenant', ['notes:read'])
    })
    // The sets ride inside the snapshot; the hooks recompute on the
    // bump without any extra wiring.
    expect(state.current.permissionSets).toEqual({
      tenant: ['notes:read'],
      system: null,
    })
    expect(can.current).toBe(true)
    act(() => {
      harness.session.setPermissionSet('tenant', null)
    })
    expect(state.current.permissionSets).toEqual({
      tenant: null,
      system: null,
    })
    expect(can.current).toBe(false)
    // A logout clears the sets and drops every read to its
    // fail-closed default.
    await act(async () => {
      await harness.session.logout()
    })
    expect(state.current.state).toBe('anonymous')
    expect(can.current).toBe(false)
  })

  it('attachSession rebinds: the previous session stops reaching the hooks', async () => {
    const first = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
    })
    const second = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
    })
    attachSession(first.session)
    await act(async () => {
      await first.session.loginWithPassword(credentials)
    })
    const { result } = renderHook(() => useCurrentTenant())
    expect(result.current).toEqual({ tenantId: 'tenant-1' })

    // Rebinding to a fresh, still-anonymous session: last bind wins,
    // and the swap wakes the subscribers to re-read.
    act(() => {
      attachSession(second.session)
    })
    expect(result.current).toBeNull()

    // The first session's later transitions no longer reach the hooks.
    await act(async () => {
      await first.session.loginWithPassword(credentials)
    })
    expect(result.current).toBeNull()

    // Re-attaching the same session is a no-op.
    act(() => {
      attachSession(second.session)
    })
    expect(result.current).toBeNull()

    // The newly attached session's own transitions do reach the hooks.
    await act(async () => {
      await second.session.loginWithPassword(credentials)
    })
    expect(result.current).toEqual({ tenantId: 'tenant-1' })
  })
})

describe('server rendering', () => {
  /** The probe renderToString exercises: reads the auth state through
   * the real hook and renders its state word. */
  function ServerProbe() {
    const snapshot = useAuthState()
    return createElement('div', null, snapshot.state)
  }

  it('renders the anonymous snapshot on the server, even with an authenticated session attached', async () => {
    // P3-13: useAuthState used to call useSyncExternalStore without a
    // getServerSnapshot. Server rendering has no session -- attachSession
    // runs in browser bootstrap code -- so the server answer must be the
    // stable anonymous snapshot, never the attached session's. To prove
    // the getServerSnapshot path really is the one used, this renders
    // with an authenticated session attached (the state a client-side
    // only implementation would have served) and asserts the server
    // still renders anonymous, without a React warning.
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
    })
    attachSession(harness.session)
    await act(async () => {
      await harness.session.loginWithPassword(credentials)
    })
    expect(harness.session.getSnapshot().state).toBe('authenticated')
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
    try {
      const html = renderToString(createElement(ServerProbe))
      expect(html).toBe('<div>anonymous</div>')
      expect(errorSpy).not.toHaveBeenCalled()
    } finally {
      errorSpy.mockRestore()
    }
  })
})
