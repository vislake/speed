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
import { authnGetMe } from '@speed/api-sdk'
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
  snapshotLog,
  STEP_UP,
  SWITCH_TENANT,
} from '../test-utils/session-harness'
import type { Harness } from '../test-utils/session-harness'
import { createAuthSession } from './session'
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

  it('restores the access token and rethrows raw on a transport failure', async () => {
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

  it('restores the access token and rethrows raw on a server-side error', async () => {
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
    // The refresh request cleared the store; the logout completes
    // while the refresh is still in flight.
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
    // The session's own refresh request travelled credential-less:
    // the bearer-only rule that keeps it out of the refresh path.
    const refreshCall = fetchCalls.find(
      (call) => call.path === '/api/v1/authn/token/refresh',
    )
    expect(refreshCall?.authorization).toBeNull()
  })
})

/** Builds a Response with a JSON body for the fetch stand-in. */
function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  })
}
