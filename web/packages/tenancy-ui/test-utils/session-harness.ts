/**
 * session-harness.ts -- the scripting harness for @speed/tenancy-ui tests.
 *
 * TenantSwitcher calls the session's switchTenant operation, which hits
 * the network; a fake request function bound through the @speed/api-sdk
 * runtime seam (bindRequestFn, the same seam a host's real client binds)
 * drives a real session over a fresh memory store, and tests assert
 * observable state only: the store's token, the request script's bodies
 * and the raw ApiErrors a failed switch rejects with.
 *
 * This is a mirror of @speed/auth-core's own test-utils/session-harness
 * (and of auth-ui's copy of it), trimmed to the operations the
 * tenancy-ui surface drives -- the password login a host journey performs
 * before a tenant switch can exist, and the switch operation itself. The
 * endpoint path constants are the same literal keys. The core package's
 * harness is deliberately not published, so a package whose tests drive
 * sessions carries its own copy -- keeping the copies in lockstep when
 * the endpoint surface changes.
 *
 * Lives in test-utils/ so every src/*.test.tsx drives sessions through
 * it.
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
import { createAuthSession } from '@speed/auth-core'
import type { AuthSession } from '@speed/auth-core'

export const LOGIN_PASSWORD = 'POST /api/v1/authn/login/password'
export const SWITCH_TENANT = 'POST /api/v1/authn/tenant/switch'

function principal(
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
