/**
 * real-client.ts -- the real-client network rig for @speed/product-shell's
 * journey tests (usage-example.test.tsx) and its view-machine suite
 * (ProductShell.test.tsx).
 *
 * Those journeys drive the assembled shell over the wiring the package
 * README's quick start documents -- a real @speed/api-client createClient
 * over a fetch stand-in answering with genuine Response objects, the
 * memory access-token store, and the session's own refreshAccessToken:
 * () => session.refresh() -- bound into the api-sdk runtime seam
 * (bindRequestFn, the same seam every generated call uses). Because the
 * transport is real api-client machinery, whatever the responder scripts
 * (including the 401-refresh leg, which api-client exercises itself) is
 * handled by the client rather than scripted around.
 *
 * The rig mirrors the real-client leg of @speed/auth-ui's own
 * test-utils/real-client.ts (same fetcher shape, same jsonResponse over
 * genuine Response objects); it is the product-shell half of that
 * evidence, since this package has no session logic of its own to test.
 * makePair rides along here (auth-ui keeps it in its session-harness):
 * product-shell journeys drive only the happy paths -- a token-issuing
 * login and a 204 logout -- so the pair is the only answer shape the
 * suite scripts, and one rig file keeps them together. Like every
 * cross-package test-utility copy, this file stays in lockstep with its
 * auth-ui original by hand.
 */

import {
  createClient,
  createMemoryAccessTokenStore,
} from '@speed/api-client'
import type { AccessTokenStore } from '@speed/api-client'
import { bindRequestFn } from '@speed/api-sdk/runtime'
import type { AuthnTokenPair } from '@speed/api-sdk'
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

/**
 * A token-issuing login answer in the shape the session parser reads
 * (snake_case body fields, the principal inline). The session lifecycle
 * needs nothing else on its happy paths.
 */
export function makePair(overrides: Partial<AuthnTokenPair> = {}): AuthnTokenPair {
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
