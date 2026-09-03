// @vitest-environment jsdom
/**
 * The README usage example, compiled and executed by the suite.
 *
 * The README's Quick start wires the session over a host-built
 * @speed/api-client transport (createClient + bindRequestFn). A unit
 * suite cannot run that composition -- no network -- so this file
 * drives the identical session API through the scripted request seam
 * the package's own tests use (test-utils/session-harness.ts, bound
 * through the same bindRequestFn seam a host's real client binds), and
 * a probe component renders the README's hook reads -- useAuthState,
 * useCurrentTenant, usePermission -- with login and logout called from
 * event handlers, exactly as the hooks' rules prescribe. The
 * real-client composition itself (silent-401 refresh through
 * createClient) is proven separately in session.test.ts.
 *
 * All strings are English fixtures standing in for a host's own
 * translations: data in a test file (exempt from the no-literal-text
 * rule), never rendered product text.
 */

import {
  act,
  cleanup,
  fireEvent,
  render,
} from '@testing-library/react'
import { afterEach, describe, expect, it } from 'vitest'
import type { AuthSession } from './session'
import {
  attachSession,
  useAuthState,
  useCurrentTenant,
  usePermission,
} from './hooks'
import {
  LOGIN_PASSWORD,
  LOGOUT,
  makeHarness,
  makePair,
  snapshotLog,
} from '../test-utils/session-harness'

afterEach(() => {
  cleanup()
})

const credentials = {
  identifier: 'ada@example.com',
  password: 'pw',
}

/**
 * The README's component: the hooks read the attached session, the
 * sign-in and sign-out buttons drive it from event handlers (a hook
 * never does), and the permission checks read the host-attached sets.
 */
function SessionPanel({ session }: { readonly session: AuthSession }) {
  const snapshot = useAuthState()
  const tenant = useCurrentTenant()
  const canCreateNotes = usePermission('tenant', 'notes:write')
  const canManageUsers = usePermission('system', 'users:manage')
  return (
    <div>
      <p data-testid="state">{snapshot.state}</p>
      <p data-testid="tenant">{tenant === null ? 'none' : tenant.tenantId}</p>
      <p data-testid="notes-write">{String(canCreateNotes)}</p>
      <p data-testid="users-manage">{String(canManageUsers)}</p>
      <button
        type="button"
        onClick={() => void session.loginWithPassword(credentials)}
      >
        Sign in
      </button>
      <button type="button" onClick={() => void session.logout()}>
        Sign out
      </button>
    </div>
  )
}

describe('README usage example', () => {
  it('runs the quick-start journey: login, host-attached sets, logout', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [LOGOUT]: () => undefined,
    })
    attachSession(harness.session)
    const transitions = snapshotLog(harness.session)
    const utils = render(<SessionPanel session={harness.session} />)
    const reads = () => ({
      state: utils.getByTestId('state').textContent,
      tenant: utils.getByTestId('tenant').textContent,
      notesWrite: utils.getByTestId('notes-write').textContent,
      usersManage: utils.getByTestId('users-manage').textContent,
    })

    // Anonymous: every hook fails closed.
    expect(reads()).toEqual({
      state: 'anonymous',
      tenant: 'none',
      notesWrite: 'false',
      usersManage: 'false',
    })

    // The host's sign-in button drives the session; the store then
    // holds the access token the host's client reads on every send.
    await act(async () => {
      fireEvent.click(utils.getByRole('button', { name: 'Sign in' }))
    })
    expect(harness.store.get()).toBe('access-1')
    expect(reads()).toEqual({
      state: 'authenticated',
      tenant: 'tenant-1',
      notesWrite: 'false',
      usersManage: 'false',
    })

    // Authenticated, but permission-less until the host attaches the
    // /me-derived lists -- one set per domain, tenant and system.
    act(() => {
      harness.session.setPermissionSet('tenant', ['notes:write'])
    })
    act(() => {
      harness.session.setPermissionSet('system', ['users:manage'])
    })
    expect(reads()).toEqual({
      state: 'authenticated',
      tenant: 'tenant-1',
      notesWrite: 'true',
      usersManage: 'true',
    })

    // Sign out: the server call first, then the store empties and
    // every hook fails closed again -- a completed logout cannot be
    // resurrected by anything still in flight.
    await act(async () => {
      fireEvent.click(utils.getByRole('button', { name: 'Sign out' }))
    })
    expect(harness.store.get()).toBeNull()
    expect(reads()).toEqual({
      state: 'anonymous',
      tenant: 'none',
      notesWrite: 'false',
      usersManage: 'false',
    })
    // The subscriber view of the same journey: one notification per
    // committed transition -- login, the two set attaches, logout
    // (subscribe never pushes the current snapshot, so the initial
    // anonymous state is read through getSnapshot, not notified).
    expect(transitions.map((snapshot) => snapshot.state)).toEqual([
      'authenticated',
      'authenticated',
      'authenticated',
      'anonymous',
    ])
  })
})
