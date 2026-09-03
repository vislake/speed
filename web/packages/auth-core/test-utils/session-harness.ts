/**
 * session-harness.ts -- the shared scripting harness for
 * @speed/auth-core tests. A fake request function bound through the
 * @speed/api-sdk runtime seam (bindRequestFn, the same seam a host's
 * real client uses) drives a real session over a fresh memory store;
 * tests assert observable state only: the store's token, getSnapshot,
 * subscriber notifications, the request script's bodies and the raw
 * ApiErrors a failed operation rejects.
 *
 * Lives in test-utils/ because both src/session.test.ts and
 * src/hooks.test.ts drive sessions through it.
 */

import {
  ApiError,
  createMemoryAccessTokenStore,
} from '@speed/api-client'
import type {
  AccessTokenStore,
  RequestFn,
  RequestOptions,
} from '@speed/api-client'
import { bindRequestFn } from '@speed/api-sdk/runtime'
import type { AuthnPrincipal, AuthnTokenPair } from '@speed/api-sdk'
import { createAuthSession } from '../src/session'
import type { AuthSession, AuthSnapshot } from '../src/session'

export const LOGIN_PASSWORD = 'POST /api/v1/authn/login/password'
export const LOGIN_SMS = 'POST /api/v1/authn/login/sms'
export const LOGOUT = 'POST /api/v1/authn/logout'
export const REFRESH = 'POST /api/v1/authn/token/refresh'
export const SWITCH_TENANT = 'POST /api/v1/authn/tenant/switch'
export const STEP_UP = 'POST /api/v1/authn/mfa/step-up'
export const REQUEST_SMS_CODE = 'POST /api/v1/authn/login/sms/request'
export const REGISTER = 'POST /api/v1/authn/register'
// The social endpoints embed the provider in the path, so the keys
// below pin 'google' like the other constants pin their whole path.
// A test that needs another provider writes its own literal key
// (e.g. 'GET /api/v1/authn/social/feishu/authorize') -- the session
// threads whatever provider string it is given through verbatim.
export const SOCIAL_AUTHORIZE =
  'GET /api/v1/authn/social/google/authorize'
export const SOCIAL_CALLBACK = 'POST /api/v1/authn/social/google/callback'

export function principal(
  userId = 'user-1',
  tenantId = 'tenant-1',
): AuthnPrincipal {
  return { user_id: userId, tenant_id: tenantId, session_id: 'session-1' }
}

export function makePair(overrides: Partial<AuthnTokenPair> = {}): AuthnTokenPair {
  return {
    access_token: 'access-1',
    refresh_token: 'refresh-1',
    principal: principal(),
    ...overrides,
  }
}

export function apiError(status: number, code: string): ApiError {
  return new ApiError({ status, code, attempts: 1 })
}

/** The scripted call a fake request function receives. */
interface ScriptedCall {
  path: string
  options?: RequestOptions
}

/** A fake endpoint: the handler's return value is the resolved
 * response body; throwing rejects the request with the thrown error
 * (tests throw ApiError to script HTTP failures). */
type Script = Record<
  string,
  (call: ScriptedCall) => unknown
>

interface Call {
  method: string
  path: string
  options?: RequestOptions
}

export interface Harness {
  session: AuthSession
  store: AccessTokenStore
  calls: Call[]
}

/** Binds a scripted fake request function and creates the session over
 * a fresh memory store. Each call binds anew: the runtime seam is
 * last-bind-wins by contract. */
export function makeHarness(script: Script = {}): Harness {
  const store = createMemoryAccessTokenStore()
  const session = createAuthSession(store)
  const calls: Call[] = []
  const requestFn: RequestFn = (async <T>(
    path: string,
    options?: RequestOptions,
  ): Promise<T> => {
    const method = options?.method ?? 'GET'
    const key = `${method} ${path}`
    calls.push({ method, path, options })
    const handler = script[key]
    if (handler === undefined) {
      throw new Error(`no scripted handler for ${key}`)
    }
    return (await handler({ path, options })) as T
  }) as RequestFn
  bindRequestFn(requestFn)
  return { session, store, calls }
}

export function snapshotLog(session: AuthSession): AuthSnapshot[] {
  const seen: AuthSnapshot[] = []
  session.subscribe((snapshot) => {
    seen.push(snapshot)
  })
  return seen
}

export async function captureRejection(
  promise: Promise<unknown>,
): Promise<unknown> {
  try {
    await promise
  } catch (error) {
    return error
  }
  throw new Error('expected the promise to reject')
}
