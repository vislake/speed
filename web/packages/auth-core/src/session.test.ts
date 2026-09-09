/**
 * session.test.ts -- the memory-only session state machine over the
 * generated authn surface, tested through the same seams a host uses:
 * a fake request function bound via bindRequestFn (the @speed/api-sdk
 * runtime seam) for the state-machine scenarios, and one composition
 * test running the real @speed/api-client against a scripted fetch to
 * prove the store bridge: a refused request retries with the freshly
 * refreshed token.
 *
 * Behaviour is asserted through the observable surface only: the
 * store's token, getSnapshot, subscriber notifications, the request
 * script's bodies and the raw ApiErrors a failed operation rejects.
 * The scripted harness itself lives in test-utils/session-harness.ts,
 * shared with src/hooks.test.ts.
 */

import { describe, expect, it } from 'vitest'
import {
  createClient,
  createMemoryAccessTokenStore,
  ERROR_CODE_PROTOCOL,
  isApiError,
} from '@speed/api-client'
import { bindRequestFn } from '@speed/api-sdk/runtime'
import { authnGetMe, orgListMembers } from '@speed/api-sdk'
import {
  apiError,
  captureRejection,
  LOGIN_PASSWORD,
  LOGIN_SMS,
  LOGOUT,
  makeHarness,
  makePair,
  principal,
  REFRESH,
  REGISTER,
  REQUEST_SMS_CODE,
  snapshotLog,
  SOCIAL_AUTHORIZE,
  SOCIAL_CALLBACK,
  STEP_UP,
  SWITCH_TENANT,
} from '../test-utils/session-harness'
import type { Harness } from '../test-utils/session-harness'
import {
  createAuthSession,
  isOperationSuperseded,
  OperationSupersededError,
} from './session'
import type { AuthSnapshot } from './session'

async function expectProtocolViolation(
  promise: Promise<unknown>,
  harness: Harness,
): Promise<void> {
  // The user-operation failure contract: a rejected operation changes
  // nothing. Capture the pre-operation state so the assertions also
  // hold for an attempt made from an authenticated session -- a failed
  // switch must leave the login's tokens in place, not clear them.
  const accessTokenBefore = harness.store.get()
  const snapshotBefore = harness.session.getSnapshot()
  const error = await captureRejection(promise)
  expect(isApiError(error)).toBe(true)
  if (!isApiError(error)) {
    return
  }
  expect(error.status).toBe(200)
  expect(error.code).toBe(ERROR_CODE_PROTOCOL)
  expect(harness.store.get()).toBe(accessTokenBefore)
  expect(harness.session.getSnapshot()).toEqual(snapshotBefore)
}

describe('initial state', () => {
  it('is anonymous with a null principal', () => {
    const { session, store } = makeHarness()
    expect(session.getSnapshot()).toEqual({
      state: 'anonymous',
      principal: null,
      permissionSets: { tenant: null, system: null },
    })
    expect(store.get()).toBeNull()
  })

  it('notifies subscribers on every transition and stops after unsubscribe', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [LOGOUT]: () => undefined,
    })
    const seen = snapshotLog(harness.session)
    const gone: AuthSnapshot[] = []
    const unsubscribe = harness.session.subscribe((snapshot) => {
      gone.push(snapshot)
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    unsubscribe()
    await harness.session.logout()
    expect(seen.map((snapshot) => snapshot.state)).toEqual([
      'authenticated',
      'anonymous',
    ])
    // The unsubscribed listener saw the login but not the logout.
    expect(gone.map((snapshot) => snapshot.state)).toEqual(['authenticated'])
    expect(harness.session.getSnapshot()).toEqual({
      state: 'anonymous',
      principal: null,
      permissionSets: { tenant: null, system: null },
    })
  })
})

describe('login with password', () => {
  it('commits the issued pair into the store and the snapshot', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: (call) => {
        expect(call.options?.body).toEqual({
          identifier: 'ada@example.com',
          password: 'pw',
        })
        return makePair()
      },
    })
    const snapshot = await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    expect(snapshot.state).toBe('authenticated')
    expect(harness.store.get()).toBe('access-1')
    expect(harness.session.getSnapshot()).toEqual({
      state: 'authenticated',
      principal: principal(),
      permissionSets: { tenant: null, system: null },
    })
    expect(harness.calls).toHaveLength(1)
  })

  it('rejects the raw ApiError on failure and leaves the session untouched', async () => {
    const refused = apiError(401, 'authn.invalid_credentials')
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => {
        throw refused
      },
    })
    const seen = snapshotLog(harness.session)
    const error = await captureRejection(
      harness.session.loginWithPassword({
        identifier: 'ada@example.com',
        password: 'wrong',
      }),
    )
    expect(error).toBe(refused)
    expect(harness.store.get()).toBeNull()
    expect(harness.session.getSnapshot()).toEqual({
      state: 'anonymous',
      principal: null,
      permissionSets: { tenant: null, system: null },
    })
    expect(seen).toHaveLength(0)
    // Nothing was rotated into the session by the failed attempt.
    await expect(harness.session.refresh()).resolves.toBe(false)
  })
})

describe('login with sms code', () => {
  it('commits the issued pair and passes the request through', async () => {
    const harness = makeHarness({
      [LOGIN_SMS]: (call) => {
        expect(call.options?.body).toEqual({
          phone: '+8613800138000',
          code: '123456',
        })
        return makePair()
      },
    })
    await harness.session.loginWithSMSCode({
      phone: '+8613800138000',
      code: '123456',
    })
    expect(harness.store.get()).toBe('access-1')
    expect(harness.session.getSnapshot().principal?.user_id).toBe('user-1')
    expect(harness.calls).toHaveLength(1)
    expect(harness.calls[0]?.path).toBe('/api/v1/authn/login/sms')
  })
})

describe('protocol violations fail closed', () => {
  it.each([
    ['an access token', { refresh_token: 'refresh-1', principal: principal() }],
    ['a principal', { access_token: 'access-1', refresh_token: 'refresh-1' }],
    ['a refresh token', { access_token: 'access-1', principal: principal() }],
  ])(
    'rejects with a client.protocol error and stays anonymous when a login 2xx carries no %s',
    async (_label, body) => {
      const harness = makeHarness({
        [LOGIN_PASSWORD]: () => body,
      })
      await expectProtocolViolation(
        harness.session.loginWithPassword({
          identifier: 'ada@example.com',
          password: 'pw',
        }),
        harness,
      )
      // The invalid 2xx rotated nothing into the session: no held
      // token exists, so refresh has nothing to send and fires no
      // request.
      expect(harness.calls).toHaveLength(1)
      await expect(harness.session.refresh()).resolves.toBe(false)
      expect(harness.calls).toHaveLength(1)
    },
  )

  it('rejects when a switch 2xx carries a malformed refresh token', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SWITCH_TENANT]: () =>
        makePair({
          access_token: 'access-2',
          refresh_token: '',
          principal: principal('user-1', 'tenant-2'),
        }),
      [REFRESH]: () => makePair({ access_token: 'access-2' }),
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    await expectProtocolViolation(
      harness.session.switchTenant('tenant-2'),
      harness,
    )
    // The malformed 2xx rotated nothing and discarded nothing: the
    // pre-switch held token family is intact, so a refresh still
    // succeeds on it, and the invalid pair's access token never
    // reached the store.
    await expect(harness.session.refresh()).resolves.toBe(true)
    expect(harness.store.get()).toBe('access-2')
  })
})

describe('logout', () => {
  it('ends the session: the store empties, the snapshot goes anonymous', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [LOGOUT]: (call) => {
        expect(call.options?.method).toBe('POST')
      },
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    await harness.session.logout()
    expect(harness.store.get()).toBeNull()
    expect(harness.session.getSnapshot()).toEqual({
      state: 'anonymous',
      principal: null,
      permissionSets: { tenant: null, system: null },
    })
    await expect(harness.session.refresh()).resolves.toBe(false)
  })

  it('rejects the raw ApiError on failure and changes nothing', async () => {
    const refused = apiError(500, 'authn.server_error')
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [LOGOUT]: () => {
        throw refused
      },
      [REFRESH]: () => makePair({ access_token: 'access-2' }),
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    const error = await captureRejection(harness.session.logout())
    expect(error).toBe(refused)
    expect(harness.store.get()).toBe('access-1')
    expect(harness.session.getSnapshot().state).toBe('authenticated')
    // The held refresh token survived the failed logout.
    await expect(harness.session.refresh()).resolves.toBe(true)
  })

  it('does not clear a session that a newer login committed meanwhile', async () => {
    let releaseLogout!: () => void
    const logoutGate = new Promise<void>((resolve) => {
      releaseLogout = resolve
    })
    let loginCount = 0
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => {
        loginCount += 1
        if (loginCount === 2) {
          return makePair({
            access_token: 'access-2',
            refresh_token: 'refresh-2',
            principal: principal('user-2'),
          })
        }
        return makePair()
      },
      [LOGOUT]: () => logoutGate,
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    const logoutRequest = harness.session.logout()
    await harness.session.loginWithPassword({
      identifier: 'betty@example.com',
      password: 'pw2',
    })
    expect(harness.store.get()).toBe('access-2')
    releaseLogout()
    await logoutRequest
    // The logout revocation concerned the superseded session only:
    // the newer login owns the store and the snapshot.
    expect(harness.store.get()).toBe('access-2')
    expect(harness.session.getSnapshot().principal?.user_id).toBe('user-2')
  })

  it('converges to signed-out when a same-session switch pre-empts the logout', async () => {
    // A pre-empted logout must not silently no-op. The logout request
    // revokes the session server-side (immediate revocation in the
    // shipped composition); a concurrent switchTenant whose response
    // lands first commits under the pre-logout generation -- and a
    // switch keeps the held refresh token, so it continues the very
    // session the logout's revocation ends. Left in place, the
    // winner's committed state would keep the user who clicked
    // sign-out signed in locally on a session the server has ended,
    // until the next 401 converged it. The local state converges to
    // signed-out instead: the operation this call started was a
    // logout and its server-side revocation succeeded, so the
    // pre-empting operation's own committed state must not swallow
    // it.
    let releaseSwitch!: (pair: unknown) => void
    const switchGate = new Promise((resolve) => {
      releaseSwitch = resolve
    })
    let releaseLogout!: () => void
    const logoutGate = new Promise<void>((resolve) => {
      releaseLogout = resolve
    })
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SWITCH_TENANT]: () => switchGate,
      [LOGOUT]: () => logoutGate,
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    const seen = snapshotLog(harness.session)
    // A tenant switch is in flight when the user clicks sign-out: both
    // operations capture the same pre-commit generation.
    const switching = harness.session.switchTenant('tenant-2')
    const logoutRequest = harness.session.logout()
    // The switch's response lands first and commits. It kept the held
    // refresh token (a switch mints no new one), so its committed
    // state describes the same session the logout's revocation is
    // about to end.
    releaseSwitch(
      makePair({
        access_token: 'access-2',
        principal: principal('user-1', 'tenant-2'),
      }),
    )
    await expect(switching).resolves.toMatchObject({
      state: 'authenticated',
      principal: { tenant_id: 'tenant-2' },
    })
    expect(harness.store.get()).toBe('access-2')
    // The logout's own revocation resolves now, pre-empted by the
    // switch's commit. The user who clicked sign-out is signed out:
    // the state the pre-empting operation committed spoke for the
    // session the revocation just ended.
    releaseLogout()
    await logoutRequest
    expect(harness.store.get()).toBeNull()
    expect(harness.session.getSnapshot()).toEqual({
      state: 'anonymous',
      principal: null,
      permissionSets: { tenant: null, system: null },
    })
    await expect(harness.session.refresh()).resolves.toBe(false)
    // Subscribers heard the convergence: the last notified snapshot is
    // the anonymous one.
    expect(seen.at(-1)?.state).toBe('anonymous')
  })
})

describe('switch tenant', () => {
  it('commits the new pair and keeps the held refresh token when none is minted', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SWITCH_TENANT]: (call) => {
        expect(call.options?.body).toEqual({ tenant_id: 'tenant-2' })
        return makePair({
          access_token: 'access-2',
          // No refresh_token: a switch reuses the caller's.
          principal: principal('user-1', 'tenant-2'),
        })
      },
      [REFRESH]: () => makePair({ access_token: 'access-3' }),
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    const snapshot = await harness.session.switchTenant('tenant-2')
    expect(snapshot.principal?.tenant_id).toBe('tenant-2')
    expect(harness.store.get()).toBe('access-2')
    // The pre-switch refresh token still refreshes: it was preserved.
    await expect(harness.session.refresh()).resolves.toBe(true)
    expect(harness.store.get()).toBe('access-3')
  })

  it('rotates the held refresh token when the response mints one', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SWITCH_TENANT]: () =>
        makePair({
          access_token: 'access-2',
          refresh_token: 'refresh-2',
          principal: principal('user-1', 'tenant-2'),
        }),
      [REFRESH]: (call) => {
        expect(call.options?.body).toEqual({ refresh_token: 'refresh-2' })
        return makePair({ access_token: 'access-3' })
      },
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    await harness.session.switchTenant('tenant-2')
    await expect(harness.session.refresh()).resolves.toBe(true)
  })

  it('rejects the raw ApiError on failure and leaves the session as it was', async () => {
    const refused = apiError(403, 'authn.not_a_member')
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SWITCH_TENANT]: () => {
        throw refused
      },
      [REFRESH]: () => makePair({ access_token: 'access-2' }),
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    const error = await captureRejection(harness.session.switchTenant('tenant-9'))
    expect(error).toBe(refused)
    expect(harness.store.get()).toBe('access-1')
    expect(harness.session.getSnapshot().principal?.tenant_id).toBe('tenant-1')
    await expect(harness.session.refresh()).resolves.toBe(true)
  })
})

describe('operation supersession', () => {
  // A superseded token-issuing operation rejects instead of resolving
  // with the winner's snapshot: a silent resolve with no notify would
  // give the caller no way to tell "my own request committed" from "I
  // lost the race and someone else's operation is now the truth" --
  // exactly what TenantSwitcher's onSwitched contract ("fired exactly
  // once after a switch commits") depends on, since a resolving loser
  // would fire it too.

  it('rejects OperationSupersededError for the switchTenant call that loses the generation race, never resolving with the winner\'s tenant', async () => {
    let releaseTenant2!: (pair: unknown) => void
    let releaseTenant3!: (pair: unknown) => void
    const tenant2Gate = new Promise((resolve) => {
      releaseTenant2 = resolve
    })
    const tenant3Gate = new Promise((resolve) => {
      releaseTenant3 = resolve
    })
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SWITCH_TENANT]: (call) => {
        const body = call.options?.body as { tenant_id?: string }
        return body?.tenant_id === 'tenant-2' ? tenant2Gate : tenant3Gate
      },
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    const seen = snapshotLog(harness.session)
    // Two concurrent switchTenant calls, exactly the shape two
    // TenantSwitcher instances racing a click each produce: both
    // capture the same pre-switch generation synchronously, before
    // either request's response arrives.
    const toTenant2 = harness.session.switchTenant('tenant-2')
    const toTenant3 = harness.session.switchTenant('tenant-3')

    // tenant-2's response arrives first and commits: it owns the
    // generation now.
    releaseTenant2(
      makePair({
        access_token: 'access-tenant-2',
        principal: principal('user-1', 'tenant-2'),
      }),
    )
    await expect(toTenant2).resolves.toMatchObject({
      state: 'authenticated',
      principal: { tenant_id: 'tenant-2' },
    })
    expect(harness.store.get()).toBe('access-tenant-2')

    // tenant-3's response arrives after tenant-2 already committed: its
    // own request answered successfully server-side (this is not an
    // ApiError), but it lost the generation race. It must reject
    // distinguishably rather than resolve with tenant-2's snapshot.
    releaseTenant3(
      makePair({
        access_token: 'access-tenant-3',
        principal: principal('user-1', 'tenant-3'),
      }),
    )
    const error = await captureRejection(toTenant3)
    expect(isOperationSuperseded(error)).toBe(true)
    expect(isApiError(error)).toBe(false)
    if (isOperationSuperseded(error)) {
      // The rejection carries the session's actual (winning) state,
      // for a caller that wants to compare it against its own request.
      expect(error.snapshot.principal?.tenant_id).toBe('tenant-2')
    }
    // The loser's tokens were never applied: the session still runs
    // on tenant-2's access token and principal, exactly as before
    // tenant-3's response arrived.
    expect(harness.store.get()).toBe('access-tenant-2')
    expect(harness.session.getSnapshot().principal?.tenant_id).toBe('tenant-2')
    // Only the winning commit notified subscribers -- the superseded
    // rejection is silent on the notify() side too, since nothing
    // observable changed for it.
    expect(seen.map((snapshot) => snapshot.principal?.tenant_id)).toEqual([
      'tenant-2',
    ])
  })

  it('rejects OperationSupersededError for a loginWithPassword call that loses a concurrent login race', async () => {
    let releaseFirst!: (pair: unknown) => void
    let releaseSecond!: (pair: unknown) => void
    const firstGate = new Promise((resolve) => {
      releaseFirst = resolve
    })
    const secondGate = new Promise((resolve) => {
      releaseSecond = resolve
    })
    let loginCall = 0
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => {
        loginCall += 1
        return loginCall === 1 ? firstGate : secondGate
      },
    })
    // A double-submitted login form: two calls fired back to back,
    // both anonymous, before either response arrives.
    const first = harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    const second = harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })

    releaseFirst(makePair({ access_token: 'access-first' }))
    await expect(first).resolves.toMatchObject({ state: 'authenticated' })
    expect(harness.store.get()).toBe('access-first')

    // The second submission's response answers successfully too, but
    // only after the first already committed. Silently resolving here
    // would tell the caller its own (possibly different) credentials
    // are the ones now signed in, which is not true: the first
    // request's identity is what the session actually holds.
    releaseSecond(
      makePair({
        access_token: 'access-second',
        refresh_token: 'refresh-second',
      }),
    )
    const error = await captureRejection(second)
    expect(isOperationSuperseded(error)).toBe(true)
    expect(isApiError(error)).toBe(false)
    // The loser's tokens never applied: the session still runs on the
    // first login's access token, and a refresh still presents the
    // first login's refresh token (not the second's).
    expect(harness.store.get()).toBe('access-first')
  })

  it('recognises a structurally-shaped superseded error from outside the class', () => {
    // isOperationSuperseded mirrors isApiError's lenient shape: an
    // instanceof first, then the structural fallback, so a rejection
    // that crossed a package boundary or a realm (a double-copied or
    // re-packaged error) still reads as superseded instead of
    // collapsing into a caller's unknown-error path.
    // The structural fallback must not depend on the snapshot's
    // literal narrowness -- a real cross-realm copy arrives as plain
    // data -- so the duck carries a fully typed snapshot.
    const winnerSnapshot: AuthSnapshot = {
      state: 'authenticated',
      principal: { user_id: 'user-1', tenant_id: 'tenant-2', session_id: 'session-1' },
      permissionSets: { tenant: null, system: null },
    }
    const duck = {
      name: 'OperationSupersededError',
      message: 'a concurrent operation committed to the session before this one settled',
      snapshot: winnerSnapshot,
    }
    expect(isOperationSuperseded(duck)).toBe(true)
    // The real class instance keeps matching through the instanceof
    // leg.
    expect(isOperationSuperseded(new OperationSupersededError(duck.snapshot))).toBe(
      true,
    )
    // Shapes that merely resemble the name do not match: the fallback
    // demands the message and the snapshot payload.
    expect(
      isOperationSuperseded({ name: 'OperationSupersededError', snapshot: {} }),
    ).toBe(false)
    expect(
      isOperationSuperseded({ name: 'OperationSupersededError', message: 'x' }),
    ).toBe(false)
    expect(isOperationSuperseded('OperationSupersededError')).toBe(false)
    expect(isOperationSuperseded(null)).toBe(false)
    expect(isOperationSuperseded(new Error('a concurrent operation'))).toBe(false)
  })
})

describe('step-up verification', () => {
  it('commits the elevated pair and preserves the held refresh token', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [STEP_UP]: (call) => {
        expect(call.options?.body).toEqual({ code: '654321' })
        return makePair({
          access_token: 'access-elevated',
          principal: principal(),
        })
      },
      [REFRESH]: () => makePair({ access_token: 'access-2' }),
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    const snapshot = await harness.session.verifyStepUp('654321')
    expect(snapshot.state).toBe('authenticated')
    expect(harness.store.get()).toBe('access-elevated')
    await expect(harness.session.refresh()).resolves.toBe(true)
  })

  it('rejects the raw ApiError on a wrong code and changes nothing', async () => {
    const refused = apiError(401, 'authn.step_up_failed')
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [STEP_UP]: () => {
        throw refused
      },
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    const error = await captureRejection(harness.session.verifyStepUp('000000'))
    expect(error).toBe(refused)
    expect(harness.store.get()).toBe('access-1')
    expect(harness.session.getSnapshot().state).toBe('authenticated')
  })
})

describe('refresh', () => {
  it('resolves false without a request when no refresh token is held', async () => {
    const harness = makeHarness()
    const seen = snapshotLog(harness.session)
    await expect(harness.session.refresh()).resolves.toBe(false)
    expect(harness.calls).toHaveLength(0)
    expect(harness.store.get()).toBeNull()
    expect(seen).toHaveLength(0)
  })

  it('refreshes silently, rotates the held token and notifies', async () => {
    let refreshCount = 0
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [REFRESH]: () => {
        refreshCount += 1
        return makePair({
          access_token: `access-${refreshCount + 1}`,
          refresh_token: `refresh-${refreshCount + 1}`,
        })
      },
    })
    const seen = snapshotLog(harness.session)
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    await expect(harness.session.refresh()).resolves.toBe(true)
    expect(harness.store.get()).toBe('access-2')
    expect(harness.session.getSnapshot().state).toBe('authenticated')
    // A second refresh rotates onto the token the first minted.
    await expect(harness.session.refresh()).resolves.toBe(true)
    expect(harness.store.get()).toBe('access-3')
    const refreshCalls = harness.calls.filter(
      (call) => call.path === '/api/v1/authn/token/refresh',
    )
    expect(refreshCalls).toHaveLength(2)
    expect(refreshCalls[1]?.options?.body).toEqual({ refresh_token: 'refresh-2' })
    expect(seen.map((snapshot) => snapshot.state)).toEqual([
      'authenticated',
      'authenticated',
      'authenticated',
    ])
  })

  it('signs the session out when the server refuses the held token', async () => {
    const refused = apiError(401, 'authn.session_expired')
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [REFRESH]: () => {
        throw refused
      },
    })
    const seen = snapshotLog(harness.session)
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    await expect(harness.session.refresh()).resolves.toBe(false)
    expect(harness.store.get()).toBeNull()
    expect(harness.session.getSnapshot()).toEqual({
      state: 'anonymous',
      principal: null,
      permissionSets: { tenant: null, system: null },
    })
    expect(seen.map((snapshot) => snapshot.state)).toEqual([
      'authenticated',
      'anonymous',
    ])
    expect(
      harness.calls.filter((call) => call.path === '/api/v1/authn/token/refresh'),
    ).toHaveLength(1)
    // A fresh login works afterwards.
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    expect(harness.store.get()).toBe('access-1')
  })

  it('signs the session out on a contract-violating 2xx', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      // The held token was consumed, but the response carries no new
      // refresh token where one is mandatory.
      [REFRESH]: () => ({ access_token: 'access-2', principal: principal() }),
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    await expect(harness.session.refresh()).resolves.toBe(false)
    expect(harness.store.get()).toBeNull()
    expect(harness.session.getSnapshot().state).toBe('anonymous')
  })

  it('keeps the session tokens and rethrows raw on a transport failure', async () => {
    // The refresh request never touched the store on the way out (it
    // travels credential-less by declaration), so a transport failure
    // has nothing to restore: the session stands exactly as it was.
    const refused = apiError(0, 'client.network')
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [REFRESH]: () => {
        throw refused
      },
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    const error = await captureRejection(harness.session.refresh())
    expect(error).toBe(refused)
    expect(harness.store.get()).toBe('access-1')
    expect(harness.session.getSnapshot().state).toBe('authenticated')
  })

  it('keeps the session tokens and rethrows raw on a server-side error', async () => {
    const refused = apiError(503, 'authn.temporarily_unavailable')
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [REFRESH]: () => {
        throw refused
      },
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    const error = await captureRejection(harness.session.refresh())
    expect(error).toBe(refused)
    expect(harness.store.get()).toBe('access-1')
    expect(harness.session.getSnapshot().state).toBe('authenticated')
  })

  it('shares one in-flight request between concurrent callers', async () => {
    let releaseRefresh!: () => void
    const refreshGate = new Promise<void>((resolve) => {
      releaseRefresh = resolve
    })
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [REFRESH]: () => refreshGate.then(() => makePair({ access_token: 'access-2' })),
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    const first = harness.session.refresh()
    const second = harness.session.refresh()
    expect(
      harness.calls.filter((call) => call.path === '/api/v1/authn/token/refresh'),
    ).toHaveLength(1)
    releaseRefresh()
    await expect(first).resolves.toBe(true)
    await expect(second).resolves.toBe(true)
    expect(harness.store.get()).toBe('access-2')
    expect(
      harness.calls.filter((call) => call.path === '/api/v1/authn/token/refresh'),
    ).toHaveLength(1)
  })

  it('keeps the token store populated while the refresh is in flight', async () => {
    // The store must hold the current token throughout the refresh:
    // the request travels credential-less by declaration (the
    // generated refresh operation carries omitAccessToken), never by
    // clearing the store -- clearing would momentarily strip the
    // token from every concurrent request, and under api-client's
    // bearer-only rule their 401s would surface as spurious auth
    // failures.
    let releaseRefresh!: () => void
    const refreshGate = new Promise<void>((resolve) => {
      releaseRefresh = resolve
    })
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [REFRESH]: () =>
        refreshGate.then(() => makePair({ access_token: 'access-2' })),
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    const refreshing = harness.session.refresh()
    // The refresh request has gone out; the store still holds the
    // token a concurrent request would present right now.
    expect(harness.store.get()).toBe('access-1')
    releaseRefresh()
    await expect(refreshing).resolves.toBe(true)
    expect(harness.store.get()).toBe('access-2')
  })

  it('cannot resurrect a session a logout ended while it was in flight', async () => {
    let releaseRefresh!: () => void
    const refreshGate = new Promise<void>((resolve) => {
      releaseRefresh = resolve
    })
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [REFRESH]: () => refreshGate.then(() => makePair({ access_token: 'access-2' })),
      [LOGOUT]: () => undefined,
    })
    const seen = snapshotLog(harness.session)
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    const refreshing = harness.session.refresh()
    // The logout completes while the refresh is still in flight (the
    // refresh request never touched the store; clearing is the
    // logout's own work).
    await harness.session.logout()
    expect(harness.store.get()).toBeNull()
    expect(harness.session.getSnapshot().state).toBe('anonymous')
    releaseRefresh()
    await expect(refreshing).resolves.toBe(false)
    // The refresh's freshly minted pair was discarded, not applied.
    expect(harness.store.get()).toBeNull()
    expect(harness.session.getSnapshot()).toEqual({
      state: 'anonymous',
      principal: null,
      permissionSets: { tenant: null, system: null },
    })
    expect(seen.map((snapshot) => snapshot.state)).toEqual([
      'authenticated',
      'anonymous',
    ])
  })

  it('cannot overwrite a session a newer login committed while it was in flight', async () => {
    let releaseRefresh!: () => void
    const refreshGate = new Promise<void>((resolve) => {
      releaseRefresh = resolve
    })
    let loginCount = 0
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => {
        loginCount += 1
        if (loginCount === 2) {
          return makePair({
            access_token: 'access-new-login',
            refresh_token: 'refresh-new-login',
            principal: principal('user-2', 'tenant-2'),
          })
        }
        return makePair()
      },
      [REFRESH]: () => refreshGate.then(() => makePair({ access_token: 'access-2' })),
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    const refreshing = harness.session.refresh()
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    expect(harness.store.get()).toBe('access-new-login')
    releaseRefresh()
    await expect(refreshing).resolves.toBe(false)
    // The stale refresh result lost the race: the newer login owns
    // the store, the held token and the snapshot.
    expect(harness.store.get()).toBe('access-new-login')
    expect(harness.session.getSnapshot().principal?.user_id).toBe('user-2')
  })

  it('lets a user operation that settles after a refused-token clear still commit', async () => {
    // Pins the deliberate asymmetry the file header documents: a
    // logout clears AND bumps (a refresh started under the old
    // generation must not resurrect the ended session), but the
    // refresh-side clears -- the server's verdict that the session is
    // over -- deliberately do NOT bump. A login that started before
    // the refusal and settles after it is the session's legitimate
    // successor: were the clear to bump, that login -- the one true
    // winner, with no sibling operation anywhere -- would reject as
    // superseded and the user would be stranded anonymous after a
    // successful server-side login.
    let releaseLogin!: (pair: unknown) => void
    const loginGate = new Promise((resolve) => {
      releaseLogin = resolve
    })
    const harness = makeHarness({
      [LOGIN_PASSWORD]: (call) => {
        const body = call.options?.body as { identifier?: string }
        return body?.identifier === 'first@example.com'
          ? makePair()
          : loginGate.then(() => makePair({ access_token: 'access-second' }))
      },
      [REFRESH]: () => {
        throw apiError(401, 'authn.session_expired')
      },
    })
    await harness.session.loginWithPassword({
      identifier: 'first@example.com',
      password: 'pw',
    })
    expect(harness.store.get()).toBe('access-1')
    // The second login goes out first, capturing the current
    // generation...
    const secondLogin = harness.session.loginWithPassword({
      identifier: 'second@example.com',
      password: 'pw',
    })
    // ...then a refresh presents the held token and is refused: the
    // session clears to anonymous without bumping the generation.
    await expect(harness.session.refresh()).resolves.toBe(false)
    expect(harness.session.getSnapshot().state).toBe('anonymous')
    // The second login's answer arrives after the clear. Its
    // generation is still current -- no sibling operation committed --
    // so it commits as the session's successor.
    releaseLogin(makePair({ access_token: 'access-second' }))
    await expect(secondLogin).resolves.toMatchObject({ state: 'authenticated' })
    expect(harness.store.get()).toBe('access-second')
    expect(harness.session.getSnapshot().principal?.user_id).toBe('user-1')
  })

  it('adopts the rotated token when a tenant switch wins the race, resolving false so the refused request never replays under the new tenant', async () => {
    // When a tenant switch commits while a refresh is in flight, the
    // switch's own access token and principal stand and only the
    // rotated refresh token is adopted onto them: the refresh's
    // success consumed the held token server-side, and dropping the
    // rotated one would leave the session presenting a consumed token
    // -- its next refresh would read as a replay, be refused, and
    // sign the session out. The resolution's meaning is pinned here
    // too: a switch changed the principal, so the refresh must
    // resolve false -- the api-client silent-401 hook reads true as
    // "retry the refused request with the store's token", and a retry
    // under the new tenant's token could answer with new-tenant data
    // cached under the old tenant's key. (A step-up -- same principal
    // -- keeps resolving true; see the test below.)
    let releaseRefresh!: () => void
    const refreshGate = new Promise<void>((resolve) => {
      releaseRefresh = resolve
    })
    let refreshCount = 0
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SWITCH_TENANT]: () =>
        makePair({
          access_token: 'access-switched',
          // No refresh_token: a switch reuses the caller's.
          principal: principal('user-1', 'tenant-2'),
        }),
      [REFRESH]: (call) => {
        const presented = (call.options?.body as { refresh_token?: string })
          ?.refresh_token
        if (presented !== 'refresh-1' && presented !== 'refresh-2') {
          // Presenting a consumed token reads as a replay and is
          // refused, exactly as the authn server answers it.
          throw apiError(401, 'authn.session_expired')
        }
        refreshCount += 1
        return refreshGate.then(() =>
          makePair({
            access_token: 'access-rotated',
            refresh_token: 'refresh-2',
          }),
        )
      },
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    const refreshing = harness.session.refresh()
    // The tenant switch commits while the refresh is still in flight.
    await harness.session.switchTenant('tenant-2')
    expect(harness.store.get()).toBe('access-switched')
    expect(harness.session.getSnapshot().principal?.tenant_id).toBe('tenant-2')
    releaseRefresh()
    // The refresh lost the race for the visible state -- the switch's
    // access token and principal stand untouched -- and resolves
    // false: the request whose 401 started it spoke for the old
    // tenant, and replaying it under the switched tenant's token could
    // cache new-tenant data under the old tenant's key. The rotated
    // refresh token WAS adopted (see below), so the session can still
    // refresh; only the "worth a retry" answer is withheld.
    await expect(refreshing).resolves.toBe(false)
    expect(harness.store.get()).toBe('access-switched')
    expect(harness.session.getSnapshot().principal?.tenant_id).toBe('tenant-2')
    await expect(harness.session.refresh()).resolves.toBe(true)
    // The second refresh presented the rotated token, not the consumed
    // one: it was served instead of refused as a replay.
    const refreshCalls = harness.calls.filter(
      (call) => call.path === '/api/v1/authn/token/refresh',
    )
    expect(refreshCalls).toHaveLength(2)
    expect(refreshCount).toBe(2)
    expect(refreshCalls[1]?.options?.body).toEqual({ refresh_token: 'refresh-2' })
    expect(harness.store.get()).toBe('access-rotated')
  })

  it('adopts the rotated token when a step-up wins the race', async () => {
    // The step-up twin of the tenant-switch race above: a step-up also
    // commits without minting a new refresh token, so an in-flight
    // refresh that succeeds afterwards must hand its rotated token to
    // the elevated session rather than let its next refresh die.
    let releaseRefresh!: () => void
    const refreshGate = new Promise<void>((resolve) => {
      releaseRefresh = resolve
    })
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [STEP_UP]: () =>
        makePair({
          access_token: 'access-elevated',
          principal: principal(),
        }),
      [REFRESH]: (call) => {
        const presented = (call.options?.body as { refresh_token?: string })
          ?.refresh_token
        if (presented !== 'refresh-1' && presented !== 'refresh-2') {
          throw apiError(401, 'authn.session_expired')
        }
        return refreshGate.then(() =>
          makePair({
            access_token: 'access-rotated',
            refresh_token: 'refresh-2',
          }),
        )
      },
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    const refreshing = harness.session.refresh()
    // The step-up commits while the refresh is still in flight.
    await harness.session.verifyStepUp('654321')
    expect(harness.store.get()).toBe('access-elevated')
    releaseRefresh()
    // True here, unlike the tenant-switch twin: a step-up keeps the
    // principal this refresh spoke for, so the silent-401 answer
    // "the store's token is worth a retry" stays safe -- the retried
    // request speaks for the same identity.
    await expect(refreshing).resolves.toBe(true)
    // The elevation stands; only the rotated refresh token was adopted.
    expect(harness.store.get()).toBe('access-elevated')
    expect(harness.session.getSnapshot().principal?.tenant_id).toBe('tenant-1')
    // The session still refreshes, on the rotated token.
    await expect(harness.session.refresh()).resolves.toBe(true)
    expect(harness.store.get()).toBe('access-rotated')
  })

  it('shares the switch-race request with a refresh started after the switch', async () => {
    // The single-flight slot is keyed on the held token, never the
    // generation: a tenant switch bumps the generation but keeps the
    // held token, so a refresh started after the switch must not fire
    // a second request presenting the same token while the first is
    // still in flight -- two parallel presentations of one token,
    // which the authn server reads as theft and answers by rotating
    // the whole family out from under the session. Keyed on the held
    // token, the later refresh shares the in-flight request instead.
    let releaseRefresh!: () => void
    const refreshGate = new Promise<void>((resolve) => {
      releaseRefresh = resolve
    })
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SWITCH_TENANT]: () =>
        makePair({
          access_token: 'access-switched',
          principal: principal('user-1', 'tenant-2'),
        }),
      [REFRESH]: () =>
        refreshGate.then(() =>
          makePair({
            access_token: 'access-rotated',
            refresh_token: 'refresh-2',
          }),
        ),
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    const first = harness.session.refresh()
    await harness.session.switchTenant('tenant-2')
    // Same held token, newer generation: the request in flight is
    // shared, never doubled.
    const second = harness.session.refresh()
    expect(
      harness.calls.filter((call) => call.path === '/api/v1/authn/token/refresh'),
    ).toHaveLength(1)
    releaseRefresh()
    // Both callers share one verdict, and the switch that won makes it
    // false: a tenant switch changed the principal this refresh spoke
    // for, so neither caller may treat the resolution as "the store's
    // token is worth a retry" (the same old-tenant-key pollution the
    // switch-race test above pins).
    await expect(first).resolves.toBe(false)
    await expect(second).resolves.toBe(false)
    // The shared result adopted the rotated refresh token onto the
    // switch's session; the switch's own access token and principal
    // stand.
    expect(harness.store.get()).toBe('access-switched')
    expect(harness.session.getSnapshot().principal?.tenant_id).toBe('tenant-2')
    expect(
      harness.calls.filter((call) => call.path === '/api/v1/authn/token/refresh'),
    ).toHaveLength(1)
    // And the session still refreshes -- on the rotated token.
    await expect(harness.session.refresh()).resolves.toBe(true)
    expect(harness.store.get()).toBe('access-rotated')
  })

  it('leaves an in-flight refresh intact when a login fails', async () => {
    let releaseRefresh!: () => void
    const refreshGate = new Promise<void>((resolve) => {
      releaseRefresh = resolve
    })
    const refused = apiError(401, 'authn.invalid_credentials')
    let loginCount = 0
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => {
        loginCount += 1
        if (loginCount === 2) {
          throw refused
        }
        return makePair()
      },
      [REFRESH]: () =>
        refreshGate.then(() => makePair({ access_token: 'access-2' })),
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    const refreshing = harness.session.refresh()
    const error = await captureRejection(
      harness.session.loginWithPassword({
        identifier: 'ada@example.com',
        password: 'pw',
      }),
    )
    expect(error).toBe(refused)
    // A failed login must not strand the in-flight refresh: with the
    // session unchanged, its result still applies.
    releaseRefresh()
    await expect(refreshing).resolves.toBe(true)
    expect(harness.store.get()).toBe('access-2')
    expect(harness.session.getSnapshot().state).toBe('authenticated')
  })
})

describe('subscriber notification isolation', () => {
  it('contains a throwing subscriber: the other listeners still hear the commit and the operation still resolves', async () => {
    // notify() isolates each listener: a host listener that throws --
    // registered first, Set order runs it before the hooks bridge --
    // must not freeze the bridge (the React tree would keep the stale
    // snapshot) nor let the exception escape settleIssued (a login
    // whose commit succeeded would reject with the listener's
    // non-ApiError throw, surfacing as an unknown failure while
    // onSignedIn never fired). The commit the listener was told about
    // stands, the other listeners hear it, and the throwing listener
    // is the host's bug to find, not the session's to surface.
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
    })
    const seen: AuthSnapshot[] = []
    // Registered first, so Set iteration runs it before the recorder.
    harness.session.subscribe(() => {
      throw new Error('host listener bug')
    })
    harness.session.subscribe((snapshot) => {
      seen.push(snapshot)
    })
    // The login resolves with its committed snapshot -- the listener's
    // throw never corrupts the operation's answer.
    const snapshot = await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    expect(snapshot.state).toBe('authenticated')
    expect(harness.store.get()).toBe('access-1')
    // The recorder heard the very same commit despite running after
    // the throwing listener.
    expect(seen).toHaveLength(1)
    expect(seen[0]?.state).toBe('authenticated')
  })

  it('contains a throwing subscriber during a refresh-side clear too', async () => {
    // The twin of the commit notify: a refresh whose held token is
    // refused clears the session and notifies; a throwing subscriber
    // must not turn that clear into an escaping exception (the
    // refresh resolves false, never rejects) or freeze the listeners
    // that run after it.
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [REFRESH]: () => {
        throw apiError(401, 'authn.session_expired')
      },
    })
    const seen: AuthSnapshot[] = []
    harness.session.subscribe(() => {
      throw new Error('host listener bug')
    })
    harness.session.subscribe((snapshot) => {
      seen.push(snapshot)
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    await expect(harness.session.refresh()).resolves.toBe(false)
    expect(harness.session.getSnapshot().state).toBe('anonymous')
    expect(seen.map((snapshot) => snapshot.state)).toEqual([
      'authenticated',
      'anonymous',
    ])
  })
})

describe('host-attached permission sets', () => {
  const credentials = {
    identifier: 'ada@example.com',
    password: 'pw',
  }

  it('replaces one domain, keeps the other, and notifies', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
    })
    await harness.session.loginWithPassword(credentials)
    const seen = snapshotLog(harness.session)
    harness.session.setPermissionSet('tenant', ['notes:read'])
    harness.session.setPermissionSet('system', ['users:manage'])
    harness.session.setPermissionSet('tenant', ['notes:read', 'notes:write'])
    expect(harness.session.getSnapshot().permissionSets).toEqual({
      tenant: ['notes:read', 'notes:write'],
      system: ['users:manage'],
    })
    // Each call notifies subscribers, like any other snapshot change.
    expect(seen).toHaveLength(3)
    expect(harness.session.getSnapshot().state).toBe('authenticated')
  })

  it('stores a defensive copy and clears a domain on null', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
    })
    await harness.session.loginWithPassword(credentials)
    const tenantPerms = ['notes:read']
    harness.session.setPermissionSet('tenant', tenantPerms)
    // The snapshot never aliases the caller's array: mutating it
    // afterwards changes nothing here.
    expect(harness.session.getSnapshot().permissionSets.tenant).not.toBe(
      tenantPerms,
    )
    tenantPerms.push('notes:write')
    expect(harness.session.getSnapshot().permissionSets.tenant).toEqual([
      'notes:read',
    ])
    harness.session.setPermissionSet('tenant', null)
    expect(harness.session.getSnapshot().permissionSets).toEqual({
      tenant: null,
      system: null,
    })
  })

  it('wipes sets attached before a login: no session inherits another', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
    })
    harness.session.setPermissionSet('tenant', ['notes:read'])
    harness.session.setPermissionSet('system', ['users:manage'])
    await harness.session.loginWithPassword(credentials)
    expect(harness.session.getSnapshot().permissionSets).toEqual({
      tenant: null,
      system: null,
    })
  })

  it('wipes sets a login as a different user would otherwise inherit', async () => {
    let loginCount = 0
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => {
        loginCount += 1
        if (loginCount === 2) {
          return makePair({
            principal: principal('user-2', 'tenant-2'),
          })
        }
        return makePair()
      },
    })
    await harness.session.loginWithPassword(credentials)
    harness.session.setPermissionSet('tenant', ['notes:read'])
    harness.session.setPermissionSet('system', ['users:manage'])
    await harness.session.loginWithPassword({
      identifier: 'betty@example.com',
      password: 'pw2',
    })
    expect(harness.session.getSnapshot().principal?.user_id).toBe('user-2')
    expect(harness.session.getSnapshot().permissionSets).toEqual({
      tenant: null,
      system: null,
    })
  })

  it('wipes sets a same-user login would otherwise inherit', async () => {
    let loginCount = 0
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => {
        loginCount += 1
        // The second login mints a brand-new session (a fresh token
        // pair) for the same user and tenant as the first: its lists
        // were fetched under a session that has ended.
        if (loginCount === 2) {
          return makePair({
            access_token: 'access-2',
            refresh_token: 'refresh-2',
          })
        }
        return makePair()
      },
    })
    await harness.session.loginWithPassword(credentials)
    harness.session.setPermissionSet('tenant', ['notes:read'])
    harness.session.setPermissionSet('system', ['users:manage'])
    await harness.session.loginWithPassword(credentials)
    expect(harness.session.getSnapshot().principal).toEqual(principal())
    expect(harness.session.getSnapshot().permissionSets).toEqual({
      tenant: null,
      system: null,
    })
  })

  it('a tenant switch drops the tenant set and keeps the system set', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SWITCH_TENANT]: () =>
        makePair({
          access_token: 'access-2',
          principal: principal('user-1', 'tenant-2'),
        }),
    })
    await harness.session.loginWithPassword(credentials)
    harness.session.setPermissionSet('tenant', ['notes:read'])
    harness.session.setPermissionSet('system', ['users:manage'])
    await harness.session.switchTenant('tenant-2')
    expect(harness.session.getSnapshot().principal?.tenant_id).toBe('tenant-2')
    expect(harness.session.getSnapshot().permissionSets).toEqual({
      tenant: null,
      system: ['users:manage'],
    })
  })

  it('a silent refresh keeps both sets', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [REFRESH]: () => makePair({ access_token: 'access-2' }),
    })
    await harness.session.loginWithPassword(credentials)
    harness.session.setPermissionSet('tenant', ['notes:read'])
    harness.session.setPermissionSet('system', ['users:manage'])
    await expect(harness.session.refresh()).resolves.toBe(true)
    expect(harness.store.get()).toBe('access-2')
    expect(harness.session.getSnapshot().permissionSets).toEqual({
      tenant: ['notes:read'],
      system: ['users:manage'],
    })
  })

  it('logout clears both sets', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [LOGOUT]: () => undefined,
    })
    await harness.session.loginWithPassword(credentials)
    harness.session.setPermissionSet('tenant', ['notes:read'])
    harness.session.setPermissionSet('system', ['users:manage'])
    await harness.session.logout()
    expect(harness.session.getSnapshot()).toEqual({
      state: 'anonymous',
      principal: null,
      permissionSets: { tenant: null, system: null },
    })
  })

  it('a failed operation leaves the sets untouched', async () => {
    const refused = apiError(403, 'authn.not_a_member')
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SWITCH_TENANT]: () => {
        throw refused
      },
    })
    await harness.session.loginWithPassword(credentials)
    harness.session.setPermissionSet('tenant', ['notes:read'])
    harness.session.setPermissionSet('system', ['users:manage'])
    const error = await captureRejection(harness.session.switchTenant('tenant-9'))
    expect(error).toBe(refused)
    expect(harness.session.getSnapshot().permissionSets).toEqual({
      tenant: ['notes:read'],
      system: ['users:manage'],
    })
  })
})

describe('request an sms code', () => {
  it('resolves on acceptance, passes the phone through and changes nothing', async () => {
    const harness = makeHarness({
      [REQUEST_SMS_CODE]: (call) => {
        expect(call.options?.body).toEqual({ phone: '+8613800138000' })
      },
    })
    const seen = snapshotLog(harness.session)
    await expect(
      harness.session.requestSMSCode({ phone: '+8613800138000' }),
    ).resolves.toBeUndefined()
    expect(harness.calls).toHaveLength(1)
    expect(harness.calls[0]?.path).toBe('/api/v1/authn/login/sms/request')
    // An acceptance commits nothing: no store write, no snapshot
    // change and no notification.
    expect(harness.store.get()).toBeNull()
    expect(harness.session.getSnapshot()).toEqual({
      state: 'anonymous',
      principal: null,
      permissionSets: { tenant: null, system: null },
    })
    expect(seen).toHaveLength(0)
  })

  it('rejects the raw ApiError when the request is rate-limited', async () => {
    const limited = apiError(429, 'authn.rate_limited')
    const harness = makeHarness({
      [REQUEST_SMS_CODE]: () => {
        throw limited
      },
    })
    const error = await captureRejection(
      harness.session.requestSMSCode({ phone: '+8613800138000' }),
    )
    expect(error).toBe(limited)
    expect(harness.store.get()).toBeNull()
    expect(harness.session.getSnapshot().state).toBe('anonymous')
  })
})

describe('register', () => {
  const registration = {
    email: 'ada@example.com',
    password: 'pw',
    display_name: 'Ada',
    locale: 'zh-CN',
  }

  it('resolves the created user and changes nothing', async () => {
    const created = { id: 'user-9', ...registration }
    const harness = makeHarness({
      [REGISTER]: (call) => {
        expect(call.options?.body).toEqual(registration)
        return created
      },
    })
    const seen = snapshotLog(harness.session)
    const user = await harness.session.register(registration)
    expect(user).toEqual(created)
    // A registration is not a session: no token, no snapshot change
    // and no notification -- the host follows up with a login.
    expect(harness.store.get()).toBeNull()
    expect(harness.session.getSnapshot().state).toBe('anonymous')
    expect(seen).toHaveLength(0)
    expect(harness.calls).toHaveLength(1)
    expect(harness.calls[0]?.path).toBe('/api/v1/authn/register')
  })

  it('rejects the raw ApiError and leaves an authenticated session as it was', async () => {
    const taken = apiError(409, 'authn.email_already_registered')
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [REGISTER]: () => {
        throw taken
      },
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    const error = await captureRejection(harness.session.register(registration))
    expect(error).toBe(taken)
    // Registering while signed in is still not a session operation:
    // the existing session stands untouched.
    expect(harness.store.get()).toBe('access-1')
    expect(harness.session.getSnapshot().principal?.user_id).toBe('user-1')
    expect(harness.calls).toHaveLength(2)
  })
})

describe('social authorize url', () => {
  const authorizeUrl =
    'https://accounts.google.com/o/oauth2/v2/auth?redirect_uri=cb&scope=openid'
  const redirectUri = 'https://app.example.com/social/callback/google'

  it('resolves the url and passes provider and redirect_uri through', async () => {
    const harness = makeHarness({
      [SOCIAL_AUTHORIZE]: (call) => {
        expect(call.options?.query).toEqual({ redirect_uri: redirectUri })
        return { authorize_url: authorizeUrl }
      },
    })
    const url = await harness.session.socialAuthorizeUrl('google', {
      redirect_uri: redirectUri,
    })
    expect(url).toBe(authorizeUrl)
    expect(harness.calls).toHaveLength(1)
    expect(harness.calls[0]?.path).toBe(
      '/api/v1/authn/social/google/authorize',
    )
    // A pure request: nothing about the session moved.
    expect(harness.store.get()).toBeNull()
    expect(harness.session.getSnapshot().state).toBe('anonymous')
  })

  it('threads an arbitrary provider into the endpoint path', async () => {
    const harness = makeHarness({
      'GET /api/v1/authn/social/feishu/authorize': () => ({
        authorize_url: 'https://open.feishu.cn/open-apis/authen/v1/index',
      }),
    })
    const url = await harness.session.socialAuthorizeUrl('feishu', {
      redirect_uri: 'https://app.example.com/social/callback/feishu',
    })
    expect(url).toContain('open.feishu.cn')
    expect(harness.calls[0]?.path).toBe(
      '/api/v1/authn/social/feishu/authorize',
    )
  })

  it('rejects the raw ApiError when the channel is unknown', async () => {
    const unknown = apiError(400, 'authn.provider_unknown')
    const harness = makeHarness({
      [SOCIAL_AUTHORIZE]: () => {
        throw unknown
      },
    })
    const error = await captureRejection(
      harness.session.socialAuthorizeUrl('google', {
        redirect_uri: redirectUri,
      }),
    )
    expect(error).toBe(unknown)
    expect(harness.session.getSnapshot().state).toBe('anonymous')
  })

  it('rejects with client.protocol when the 2xx carries no authorize url', async () => {
    const harness = makeHarness({
      [SOCIAL_AUTHORIZE]: () => ({}),
    })
    await expectProtocolViolation(
      harness.session.socialAuthorizeUrl('google', {
        redirect_uri: redirectUri,
      }),
      harness,
    )
  })
})

describe('complete social login', () => {
  const flow = { code: '4/0AX4XfF19S4Q2', state: 'state-1' }

  it('commits the issued pair like any other login', async () => {
    const harness = makeHarness({
      [SOCIAL_CALLBACK]: (call) => {
        expect(call.options?.body).toEqual(flow)
        return { tokens: makePair() }
      },
      [REFRESH]: () => makePair({ access_token: 'access-2' }),
    })
    const seen = snapshotLog(harness.session)
    const snapshot = await harness.session.completeSocialLogin('google', flow)
    expect(snapshot.state).toBe('authenticated')
    expect(harness.store.get()).toBe('access-1')
    expect(harness.session.getSnapshot()).toEqual({
      state: 'authenticated',
      principal: principal(),
      permissionSets: { tenant: null, system: null },
    })
    expect(harness.calls).toHaveLength(1)
    expect(harness.calls[0]?.path).toBe(
      '/api/v1/authn/social/google/callback',
    )
    expect(seen.map((s) => s.state)).toEqual(['authenticated'])
    // The new session is a full login: the held refresh token is in
    // place and rotates on the next refresh like any other login's.
    await expect(harness.session.refresh()).resolves.toBe(true)
    expect(harness.store.get()).toBe('access-2')
  })

  it('wipes host-attached permission sets like any other login', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SOCIAL_CALLBACK]: () => ({
        tokens: makePair({ access_token: 'access-2' }),
      }),
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    harness.session.setPermissionSet('tenant', ['notes:write'])
    harness.session.setPermissionSet('system', ['users:manage'])
    await harness.session.completeSocialLogin('google', flow)
    // Even the same user in the same tenant: a login starts a server
    // session whose lists the host has not fetched yet.
    expect(harness.session.getSnapshot().permissionSets).toEqual({
      tenant: null,
      system: null,
    })
    expect(harness.store.get()).toBe('access-2')
  })

  it('rejects the raw ApiError on failure and changes nothing', async () => {
    const invalid = apiError(401, 'authn.oauth_state_invalid')
    const harness = makeHarness({
      [SOCIAL_CALLBACK]: () => {
        throw invalid
      },
    })
    const seen = snapshotLog(harness.session)
    const error = await captureRejection(
      harness.session.completeSocialLogin('google', {
        code: 'stale',
        state: 'stale',
      }),
    )
    expect(error).toBe(invalid)
    expect(harness.store.get()).toBeNull()
    expect(harness.session.getSnapshot()).toEqual({
      state: 'anonymous',
      principal: null,
      permissionSets: { tenant: null, system: null },
    })
    expect(seen).toHaveLength(0)
  })

  it('refuses a binding-shaped response before any state change', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      // A bound identity with no tokens -- the server's answer to an
      // already-authenticated caller. This surface is a sign-in
      // surface, so the flow must refuse it, not commit a session
      // that carries no credentials.
      [SOCIAL_CALLBACK]: () => ({ bound: true }),
      [REFRESH]: () => makePair({ access_token: 'access-2' }),
    })
    await harness.session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    await expectProtocolViolation(
      harness.session.completeSocialLogin('google', flow),
      harness,
    )
    // The refused response consumed nothing: the pre-callback session
    // stands and its held token family still refreshes.
    await expect(harness.session.refresh()).resolves.toBe(true)
    expect(harness.store.get()).toBe('access-2')
  })
})

describe('with the real api-client', () => {
  it('retries a refused request with the freshly refreshed token', async () => {
    const store = createMemoryAccessTokenStore()
    const session = createAuthSession(store)
    const fetchCalls: Array<{
      path: string
      method: string
      authorization: string | null
    }> = []
    let meAttempts = 0
    const fetcher: typeof fetch = async (input, init) => {
      const url = new URL(String(input))
      const method = init?.method ?? 'GET'
      const authorization = new Headers(init?.headers).get('authorization')
      fetchCalls.push({ path: url.pathname, method, authorization })
      if (url.pathname === '/api/v1/authn/login/password') {
        return jsonResponse(200, makePair())
      }
      if (url.pathname === '/api/v1/authn/me') {
        meAttempts += 1
        if (meAttempts === 1) {
          // The access token the store holds is stale: refuse it.
          return jsonResponse(401, {
            code: 'authn.session_expired',
            traceId: 'trace-1',
            message: 'session expired',
          })
        }
        return jsonResponse(200, principal())
      }
      if (url.pathname === '/api/v1/authn/token/refresh') {
        return jsonResponse(
          200,
          makePair({ access_token: 'access-2', refresh_token: 'refresh-2' }),
        )
      }
      throw new Error(`unexpected fetch: ${url.pathname}`)
    }
    const client = createClient({
      baseUrl: 'https://api.test',
      fetch: fetcher,
      accessTokenStore: store,
      refreshAccessToken: () => session.refresh(),
    })
    bindRequestFn(client)

    await session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    expect(store.get()).toBe('access-1')

    // A real generated operation through the real client. The first
    // attempt presents the stale token and is refused; the silent-401
    // path runs one session refresh (credential-less) and retries
    // with the fresh token.
    const me = await authnGetMe()
    expect(me.user_id).toBe('user-1')

    expect(store.get()).toBe('access-2')
    expect(session.getSnapshot().state).toBe('authenticated')
    const meCalls = fetchCalls.filter((call) => call.path === '/api/v1/authn/me')
    expect(meCalls).toHaveLength(2)
    expect(meCalls[0]?.authorization).toBe('Bearer access-1')
    expect(meCalls[1]?.authorization).toBe('Bearer access-2')
    // The session's own refresh request travelled credential-less --
    // by declaration (the generated operation carries omitAccessToken),
    // never by clearing the store: the bearer-only rule that keeps it
    // out of the refresh path.
    const refreshCall = fetchCalls.find(
      (call) => call.path === '/api/v1/authn/token/refresh',
    )
    expect(refreshCall?.authorization).toBeNull()
  })

  it('keeps concurrent requests presenting their token through a refresh', async () => {
    // The store must hold the current token throughout the refresh:
    // the request travels credential-less by declaration (the
    // generated operation carries omitAccessToken), never by clearing
    // the store. A request starting mid-refresh presents the token it
    // holds, shares the in-flight refresh on its 401, and retries
    // with the fresh token -- a credential-less 401 would be terminal
    // under the bearer-only rule: a spurious auth failure.
    const store = createMemoryAccessTokenStore()
    const session = createAuthSession(store)
    const fetchCalls: Array<{
      path: string
      method: string
      authorization: string | null
    }> = []
    let releaseRefresh!: () => void
    const refreshGate = new Promise<void>((resolve) => {
      releaseRefresh = resolve
    })
    let refreshEntered!: () => void
    const refreshStarted = new Promise<void>((resolve) => {
      refreshEntered = resolve
    })
    let meAttempts = 0
    const fetcher: typeof fetch = async (input, init) => {
      const url = new URL(String(input))
      const method = init?.method ?? 'GET'
      const authorization = new Headers(init?.headers).get('authorization')
      fetchCalls.push({ path: url.pathname, method, authorization })
      if (url.pathname === '/api/v1/authn/login/password') {
        return jsonResponse(200, makePair())
      }
      if (url.pathname === '/api/v1/authn/me') {
        meAttempts += 1
        // Both first attempts carry the stale token and are refused;
        // both retries carry the fresh one.
        if (meAttempts <= 2) {
          return jsonResponse(401, {
            code: 'authn.session_expired',
            traceId: 'trace-1',
            message: 'session expired',
          })
        }
        return jsonResponse(200, principal())
      }
      if (url.pathname === '/api/v1/authn/token/refresh') {
        refreshEntered()
        await refreshGate
        return jsonResponse(
          200,
          makePair({ access_token: 'access-2', refresh_token: 'refresh-2' }),
        )
      }
      throw new Error(`unexpected fetch: ${url.pathname}`)
    }
    const client = createClient({
      baseUrl: 'https://api.test',
      fetch: fetcher,
      accessTokenStore: store,
      refreshAccessToken: () => session.refresh(),
    })
    bindRequestFn(client)

    await session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    expect(store.get()).toBe('access-1')

    // The first request's 401 starts the silent refresh; the second
    // starts while that refresh is still in flight. Its first attempt
    // must still carry the token the store holds -- not go out
    // credential-less into a terminal 401.
    const first = authnGetMe()
    const second = authnGetMe()
    await refreshStarted
    expect(store.get()).toBe('access-1')
    releaseRefresh()
    await expect(first).resolves.toMatchObject({ user_id: 'user-1' })
    await expect(second).resolves.toMatchObject({ user_id: 'user-1' })

    expect(store.get()).toBe('access-2')
    const meCalls = fetchCalls.filter((call) => call.path === '/api/v1/authn/me')
    expect(meCalls).toHaveLength(4)
    // Both first attempts -- one per request, fired before the refresh
    // released -- presented the stale token; both retries presented
    // the fresh one. Neither request ever lost its bearer.
    expect(meCalls[0]?.authorization).toBe('Bearer access-1')
    expect(meCalls[1]?.authorization).toBe('Bearer access-1')
    expect(meCalls[2]?.authorization).toBe('Bearer access-2')
    expect(meCalls[3]?.authorization).toBe('Bearer access-2')
    // The two 401s shared exactly one session refresh.
    expect(
      fetchCalls.filter((call) => call.path === '/api/v1/authn/token/refresh'),
    ).toHaveLength(1)
  })

  it('never replays a refused request under a tenant that won the refresh race', async () => {
    // A tenant switch committing while the silent-401 refresh is in
    // flight leaves the store holding the switched tenant's token;
    // resolving true would make the client retry the refused request
    // with it. The server would answer with the new tenant's data, and
    // a host that keys its cache by tenant (['tenant', tenantId, ...])
    // would cache that answer under the OLD tenant's key -- the replay
    // lands after a removeQueries eviction, so eviction cannot cover
    // it. The safe contract: a refresh that lost the race to a
    // principal change resolves false, and the original request fails
    // instead of replaying under a principal it never asked.
    const store = createMemoryAccessTokenStore()
    const session = createAuthSession(store)
    const fetchCalls: Array<{
      path: string
      method: string
      authorization: string | null
      body: string | null
    }> = []
    let membersAttempts = 0
    let releaseRefresh!: () => void
    const refreshGate = new Promise<void>((resolve) => {
      releaseRefresh = resolve
    })
    const fetcher: typeof fetch = async (input, init) => {
      const url = new URL(String(input))
      const method = init?.method ?? 'GET'
      const authorization = new Headers(init?.headers).get('authorization')
      const body = typeof init?.body === 'string' ? init.body : null
      fetchCalls.push({ path: url.pathname, method, authorization, body })
      if (url.pathname === '/api/v1/authn/login/password') {
        return jsonResponse(200, makePair())
      }
      if (url.pathname === '/api/v1/org/members') {
        membersAttempts += 1
        if (membersAttempts === 1) {
          // The tenant-1 token the store holds is stale: refuse it and
          // start the silent refresh.
          return jsonResponse(401, {
            code: 'authn.session_expired',
            traceId: 'trace-1',
            message: 'session expired',
          })
        }
        // The shape a replay would produce: the request retried under
        // the switched tenant's token, answered with tenant-2 data --
        // the very cache pollution under the tenant-1 key this guard
        // exists to prevent. A passing test must never see this call.
        return jsonResponse(200, [])
      }
      if (url.pathname === '/api/v1/authn/tenant/switch') {
        return jsonResponse(
          200,
          makePair({
            access_token: 'access-switched',
            principal: principal('user-1', 'tenant-2'),
          }),
        )
      }
      if (url.pathname === '/api/v1/authn/token/refresh') {
        await refreshGate
        return jsonResponse(
          200,
          makePair({
            access_token: 'access-rotated',
            refresh_token: 'refresh-2',
          }),
        )
      }
      throw new Error(`unexpected fetch: ${url.pathname}`)
    }
    const client = createClient({
      baseUrl: 'https://api.test',
      fetch: fetcher,
      accessTokenStore: store,
      refreshAccessToken: () => session.refresh(),
    })
    bindRequestFn(client)

    await session.loginWithPassword({
      identifier: 'ada@example.com',
      password: 'pw',
    })
    expect(store.get()).toBe('access-1')

    // The tenant-1 org-members read goes out with the stale token, gets 401
    // and starts the silent refresh -- which the gate holds open.
    const reading = orgListMembers()
    await waitForRefreshCall(fetchCalls)

    // The tenant switch commits while the refresh is still in flight.
    await session.switchTenant('tenant-2')
    expect(store.get()).toBe('access-switched')

    // The refresh resolves: its rotated token is adopted (the winner
    // kept the held token the refresh just consumed) but the verdict
    // is false -- the refused org-members read spoke for tenant-1 and must
    // not replay under tenant-2.
    releaseRefresh()
    const error = await captureRejection(reading)
    expect(isApiError(error)).toBe(true)
    if (isApiError(error)) {
      // The refused request surfaces its 401 (auth: true -- the
      // refresh path was tried and declined to retry), it never
      // resolves with tenant-2's data.
      expect(error.auth).toBe(true)
    }
    // Exactly one org-members request was sent: no replay under the
    // switched tenant's token, so no tenant-2 answer could land under
    // a tenant-1 cache key.
    expect(membersAttempts).toBe(1)
    expect(
      fetchCalls.filter((call) => call.path === '/api/v1/org/members'),
    ).toHaveLength(1)
    // The switch's session stands untouched by the losing pair.
    expect(store.get()).toBe('access-switched')
    expect(session.getSnapshot().principal?.tenant_id).toBe('tenant-2')
    // And the adoption really happened: the session still refreshes,
    // presenting the rotated token, never the consumed one.
    await expect(session.refresh()).resolves.toBe(true)
    const refreshCalls = fetchCalls.filter(
      (call) => call.path === '/api/v1/authn/token/refresh',
    )
    expect(refreshCalls).toHaveLength(2)
    expect(JSON.parse(refreshCalls[1]?.body ?? '{}')).toMatchObject({
      refresh_token: 'refresh-2',
    })
    expect(store.get()).toBe('access-rotated')
  })

  it('refuses an empty 2xx on a token-issuing login as client.protocol', async () => {
    // The generated seam declares no document-existence expectation --
    // it never sets RequestOptions.requireJsonBody, for any operation
    // (see the @speed/api-sdk runtime.ts seam doc) -- so an operation
    // whose spec declares a response body but that actually answers an
    // empty 2xx resolves as undefined through the real transport.
    // parseIssued's first property access on undefined would throw a
    // native TypeError, which is not an ApiError: isApiError(error)
    // would be false, so the rejection would bypass the ApiError
    // contract every surface resolves through (the reachable-code
    // whitelists with an unknown fallback) and escape as an unhandled
    // exception. The body-existence guard refuses the bodyless success
    // as client.protocol, the same answer every other
    // contract-violating token-issuing 2xx gets.
    const store = createMemoryAccessTokenStore()
    const session = createAuthSession(store)
    const fetcher: typeof fetch = async (input) => {
      const url = new URL(String(input))
      if (url.pathname === '/api/v1/authn/login/password') {
        return emptyResponse(200)
      }
      throw new Error(`unexpected fetch: ${url.pathname}`)
    }
    const client = createClient({
      baseUrl: 'https://api.test',
      fetch: fetcher,
      accessTokenStore: store,
    })
    bindRequestFn(client)

    const error = await captureRejection(
      session.loginWithPassword({
        identifier: 'ada@example.com',
        password: 'pw',
      }),
    )
    expect(isApiError(error)).toBe(true)
    if (!isApiError(error)) {
      return
    }
    expect(error.status).toBe(200)
    expect(error.code).toBe(ERROR_CODE_PROTOCOL)
    // A failed operation changes nothing: no token, still anonymous.
    expect(store.get()).toBeNull()
    expect(session.getSnapshot()).toEqual({
      state: 'anonymous',
      principal: null,
      permissionSets: { tenant: null, system: null },
    })
  })

  it('refuses an empty 2xx authorize answer as client.protocol', async () => {
    // The same document-existence guard, on the authorize endpoint
    // whose whole 2xx answer is the URL document: an empty 2xx
    // resolves as undefined and the property access would throw a
    // native TypeError -- never an ApiError -- before the
    // missing-field check could refuse it.
    const store = createMemoryAccessTokenStore()
    const session = createAuthSession(store)
    const fetcher: typeof fetch = async (input) => {
      const url = new URL(String(input))
      if (url.pathname === '/api/v1/authn/social/google/authorize') {
        return emptyResponse(200)
      }
      throw new Error(`unexpected fetch: ${url.pathname}`)
    }
    const client = createClient({
      baseUrl: 'https://api.test',
      fetch: fetcher,
      accessTokenStore: store,
    })
    bindRequestFn(client)

    const error = await captureRejection(
      session.socialAuthorizeUrl('google', {
        redirect_uri: 'https://app.example.com/social/callback/google',
      }),
    )
    expect(isApiError(error)).toBe(true)
    if (!isApiError(error)) {
      return
    }
    expect(error.status).toBe(200)
    expect(error.code).toBe(ERROR_CODE_PROTOCOL)
    // A pure request: nothing about the session moved.
    expect(store.get()).toBeNull()
    expect(session.getSnapshot().state).toBe('anonymous')
  })

  it('refuses an empty 2xx callback answer as client.protocol', async () => {
    // The social callback's own document-dereferencing cell
    // (response.tokens): an empty 2xx resolves as undefined through
    // the transport, and the property access must refuse it as
    // client.protocol rather than throw a native TypeError before the
    // binding-shaped-response check could run.
    const store = createMemoryAccessTokenStore()
    const session = createAuthSession(store)
    const fetcher: typeof fetch = async (input) => {
      const url = new URL(String(input))
      if (url.pathname === '/api/v1/authn/social/google/callback') {
        return emptyResponse(200)
      }
      throw new Error(`unexpected fetch: ${url.pathname}`)
    }
    const client = createClient({
      baseUrl: 'https://api.test',
      fetch: fetcher,
      accessTokenStore: store,
    })
    bindRequestFn(client)

    const error = await captureRejection(
      session.completeSocialLogin('google', {
        code: '4/0AX4XfF19S4Q2',
        state: 'state-1',
      }),
    )
    expect(isApiError(error)).toBe(true)
    if (!isApiError(error)) {
      return
    }
    expect(error.status).toBe(200)
    expect(error.code).toBe(ERROR_CODE_PROTOCOL)
    expect(store.get()).toBeNull()
    expect(session.getSnapshot().state).toBe('anonymous')
  })
})

/** Waits until the refresh request has gone out (the refresh gate
 * holds its answer, but the request itself must be on the wire before
 * the race under test can start). */
async function waitForRefreshCall(
  fetchCalls: Array<{
    path: string
    method: string
    authorization: string | null
    body: string | null
  }>,
): Promise<void> {
  for (let i = 0; i < 100; i += 1) {
    if (
      fetchCalls.some(
        (call) =>
          call.path === '/api/v1/authn/token/refresh' && call.method === 'POST',
      )
    ) {
      return
    }
    await new Promise((resolve) => {
      setTimeout(resolve, 5)
    })
  }
  throw new Error('the refresh request never went out')
}

/** Builds a Response with a JSON body for the fetch stand-in. */
function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  })
}

/** Builds a body-less 2xx for the fetch stand-in: the empty-success
 * shape the request function resolves as undefined when the request
 * declared no requireJsonBody. */
function emptyResponse(status: number): Response {
  return new Response(null, { status })
}
