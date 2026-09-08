/**
 * real-client.ts -- the real-client network rig for @speed/billing-ui's
 * journey and component tests: a real @speed/api-client createClient
 * over a fetch stand-in answering with genuine Response objects and the
 * memory access-token store the client reads on every send, bound into
 * the api-sdk runtime seam (bindRequestFn, the same seam every
 * generated call uses). The billing read surface has no session layer
 * of its own -- the sign-in that preceded the billing page is the
 * auth-core story, out of this package's scope -- so the rig's store
 * starts empty and a test plants the bearer token the page's reads ride
 * (store.set('access-1')), exactly the state a real host's sign-in flow
 * leaves behind. No refreshAccessToken seam is bound: 401 answers
 * surface directly, which is what lets a test script a dead-session
 * code and see the surface's own banner for it.
 *
 * The rig mirrors the real-client legs of @speed/auth-ui's and
 * @speed/account-ui's suites (same fetcher shape, same jsonResponse
 * over genuine Response objects), and it is the whole of this package's
 * test story: every answer the surface can render -- error codes
 * included -- is scriptable as a genuine Response carrying the API
 * envelope, so no test bypasses the real transport.
 */

import { createClient, createMemoryAccessTokenStore } from '@speed/api-client'
import type { AccessTokenStore } from '@speed/api-client'
import { bindRequestFn } from '@speed/api-sdk/runtime'
import type { BillingInvoice } from '@speed/api-sdk'

/** A request as the fetch stand-in observed it. */
export interface RealCall {
  readonly method: string
  /** The request path without the base URL. */
  readonly path: string
  /** The raw query string of the request URL, '' when the request sent
   * none (orval serializes hook params -- the invoice page size, say --
   * as query parameters). */
  readonly query: string
  /** The authorization header value, or null for a credential-less
   * request. */
  readonly authorization: string | null
}

/** One scripted endpoint: answer with a genuine Response, immediately
 * or after a deferred promise (tests hold an exchange open to assert
 * the pending state). */
export type RealResponder = (call: RealCall) => Response | Promise<Response>

export interface RealClientRig {
  /** The access-token store the client reads on every send; plant the
   * bearer token the reads ride with store.set(...). */
  readonly store: AccessTokenStore
  /** Every request observed, in order. */
  readonly calls: RealCall[]
}

/**
 * Binds a real client whose fetch stand-in answers from the script and
 * returns the store the client shares with the host's sign-in flow.
 * Each call binds anew: the runtime seam is last-bind-wins by contract.
 */
export function makeRealClientRig(respond: RealResponder): RealClientRig {
  const store = createMemoryAccessTokenStore()
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
  })
  bindRequestFn(client)
  return { store, calls }
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

/** An invoice fixture in the wire shape the billing read surface
 * answers (camelCase fields, the BillingInvoice schema's own spelling).
 * The defaults are one full calendar-month cycle, and overrides replace
 * any field wholesale. */
export function makeInvoice(
  overrides: Partial<BillingInvoice> = {},
): BillingInvoice {
  return {
    id: '3f0c1f2a-6b9e-4c2d-8a7f-9e1b2c3d4e5f',
    subscriptionId: '7a2b3c4d-5e6f-4a1b-8c9d-0e1f2a3b4c5d',
    status: 'open',
    amountCents: 12000,
    currency: 'CNY',
    periodStart: '2026-07-01T00:00:00Z',
    periodEnd: '2026-07-31T23:59:59Z',
    createdAt: '2026-07-01T01:00:00Z',
    updatedAt: '2026-07-01T01:00:00Z',
    ...overrides,
  }
}
