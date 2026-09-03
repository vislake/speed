/**
 * real-client.ts -- the real-client network rig for @speed/auth-ui's
 * journey tests (usage-example.test.tsx, session-journey.test.tsx).
 *
 * Those journeys drive the composed sign-in family over the wiring the
 * package README's quick start documents -- a real @speed/api-client
 * createClient over a fetch stand-in answering with genuine Response
 * objects, the memory access-token store, and the session's own
 * refreshAccessToken: () => session.refresh() -- bound into the api-sdk
 * runtime seam (bindRequestFn, the same seam every generated call uses).
 * Because the transport is real api-client machinery, the 401-refresh
 * leg the journey scripts (a refused request answered 401, a silent
 * credential-less refresh, one retry) is exercised by the client itself
 * rather than scripted around.
 *
 * The rig mirrors the real-client legs of @speed/auth-core's own
 * session.test.ts (same fetcher shape, same jsonResponse over genuine
 * Response objects); it is the auth-ui half of that evidence, since this
 * package has no session.test.ts of its own. session-harness.ts is the
 * other half of the test story: it drives the same operations through a
 * scripted request function for component tests that need to throw raw
 * ApiErrors or inspect request bodies. Journey tests use this rig;
 * component tests use the harness.
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
  /** The request path without the base URL or query string. */
  readonly path: string
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
  // The fetch stand-in of the README quick start, narrowed to a script:
  // it records the request leg and answers from the responder, never
  // touching the network (nothing here invokes fetch). Response objects
  // are genuine, so api-client's envelope parsing runs for real.
  const fetcher: typeof fetch = async (input, init) => {
    const url = new URL(String(input))
    const method = init?.method ?? 'GET'
    const authorization = new Headers(init?.headers).get('authorization')
    const call: RealCall = { method, path: url.pathname, authorization }
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
