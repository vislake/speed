/**
 * real-client.ts -- the real-client network rig for @speed/account-ui's
 * journey tests (the component suites of the account surfaces that drive
 * an operation through the wiring a host builds -- a real
 * @speed/api-client createClient over a fetch stand-in answering with
 * genuine Response objects, the memory access-token store, and the
 * session's own refreshAccessToken: () => session.refresh() -- bound
 * into the api-sdk runtime seam (bindRequestFn, the same seam every
 * generated call uses). Because the transport is real api-client
 * machinery, the 401-refresh leg a journey scripts (a refused request
 * answered 401, a silent credential-less refresh, one retry) is
 * exercised by the client itself rather than scripted around.
 *
 * The rig mirrors the real-client leg of @speed/auth-ui's own suite
 * (same fetcher shape, same jsonResponse over genuine Response
 * objects), and it is the whole of this package's test story: the
 * component suites of every surface and the usage-example journey all
 * drive through it. auth-ui's other half -- a scripted request function
 * for tests that throw raw ApiErrors -- has no counterpart here: every
 * answer an account surface can render is scriptable as a genuine
 * Response carrying the API envelope, so no test needs to bypass the
 * real transport.
 */

import {
  createClient,
  createMemoryAccessTokenStore,
} from '@speed/api-client'
import type { AccessTokenStore } from '@speed/api-client'
import { bindRequestFn } from '@speed/api-sdk/runtime'
import { createAuthSession } from '@speed/auth-core'
import type { AuthSession } from '@speed/auth-core'

/** A request as the fetch stand-in observed it. */
export interface RealCall {
  readonly method: string
  /** The request path without the base URL. */
  readonly path: string
  /** The raw query string of the request URL, '' when the request sent
   * none (orval serializes hook params -- the login-history page size,
   * say -- as query parameters). */
  readonly query: string
  /** The authorization header value, or null for a credential-less
   * request (the refresh leg travels credential-less by declaration). */
  readonly authorization: string | null
}

/** One scripted endpoint: answer with a genuine Response, immediately
 * or after a deferred promise (journeys hold an exchange open to assert
 * the pending state). */
export type RealResponder = (
  call: RealCall,
) => Response | Promise<Response>

export interface RealClientRig {
  /** The session the journey drives; attached to the hooks with
   * attachSession before rendering. */
  readonly session: AuthSession
  /** The access-token store the client and the session share. */
  readonly store: AccessTokenStore
  /** Every request observed, in order. */
  readonly calls: RealCall[]
}

/**
 * Binds a real client whose fetch stand-in answers from the script and
 * returns the session over the same store. Each call binds anew: the
 * runtime seam is last-bind-wins by contract.
 */
export function makeRealClientRig(respond: RealResponder): RealClientRig {
  const store = createMemoryAccessTokenStore()
  const session = createAuthSession(store)
  const calls: RealCall[] = []
  // The fetch stand-in of a real host, narrowed to a script: it records
  // the request leg and answers from the responder, never touching the
  // network (nothing here invokes fetch). Response objects are genuine,
  // so api-client's envelope parsing runs for real.
  const fetcher: typeof fetch = async (input, init) => {
    const url = new URL(String(input))
    const method = init?.method ?? 'GET'
    const authorization = new Headers(init?.headers).get('authorization')
    const call: RealCall = {
      method,
      path: url.pathname,
      query: url.search,
      authorization,
    }
    calls.push(call)
    return respond(call)
  }
  const client = createClient({
    baseUrl: 'https://api.test',
    fetch: fetcher,
    accessTokenStore: store,
    refreshAccessToken: () => session.refresh(),
  })
  bindRequestFn(client)
  return { session, store, calls }
}

/** A JSON answer in the API's envelope shape, like the real server. */
export function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  })
}

/** A 401-style error envelope answering in the API's error shape. */
export function errorResponse(
  status: number,
  code: string,
  traceId = 'trace-1',
): Response {
  return jsonResponse(status, { code, traceId, message: code })
}

/**
 * The token-issuing answer of a sign-in endpoint, in the shape auth-core
 * parses (access_token + refresh_token plus a principal carrying the
 * identity claims the access token represents -- user_id, tenant_id and
 * session_id, the last the token the session list will mark current).
 */
export interface AuthnTokenPair {
  readonly access_token: string
  readonly refresh_token: string
  readonly principal: {
    readonly user_id: string
    readonly tenant_id: string
    readonly session_id: string
  }
}

/** A token pair for a scripted sign-in; overrides replace a top-level
 * field or the whole principal wholesale. */
export function makePair(
  overrides: Partial<AuthnTokenPair> = {},
): AuthnTokenPair {
  return {
    access_token: 'access-1',
    refresh_token: 'refresh-1',
    principal: {
      user_id: 'user-1',
      tenant_id: 'tenant-1',
      session_id: 'session-1',
    },
    ...overrides,
  }
}

/**
 * The shared first leg of a signed-in journey: the responder must answer
 * POST /api/v1/authn/login/password with jsonResponse(200, makePair()).
 * The rig's session has no seed path by contract -- a reload starts
 * anonymous -- so a journey signs in through the real session operation,
 * which is also what plants the access token in the shared store.
 */
export async function signInWithPassword(
  rig: RealClientRig,
): Promise<void> {
  await rig.session.loginWithPassword({
    identifier: 'owner@example.test',
    password: 'correct-horse-battery-staple',
  })
}
