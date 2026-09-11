/**
 * real-client.ts -- the real-client network rig for @speed/billing-ui's
 * journey and component tests: a real @speed/api-client createClient
 * over a fetch stand-in answering with genuine Response objects and the
 * memory access-token store the client reads on every send (see
 * @speed/test-utils/real-client's header for the shared rig's
 * contract). The billing read surface has no session layer of its own
 * -- the sign-in that preceded the billing page is the auth-core story,
 * out of this package's scope -- so the rig runs without the refresh
 * seam: the store starts empty and a test plants the bearer token the
 * page's reads ride (store.set('access-1')), exactly the state a real
 * host's sign-in flow leaves behind, and a 401 answer surfaces directly,
 * which is what lets a test script a dead-session code and see the
 * surface's own banner for it.
 *
 * The projection below is this package's recorded call shape: the
 * request leg plus the raw query string a page read rides (the invoice
 * page size). Every answer the surface can render -- error codes
 * included -- is scriptable as a genuine Response carrying the API
 * envelope, so no test bypasses the real transport.
 */

import type { AccessTokenStore } from '@speed/api-client'
import type { BillingInvoice } from '@speed/api-sdk'
import {
  makeRealClientRig as makeRig,
} from '@speed/test-utils/real-client'
import type { ObservedRequest } from '@speed/test-utils/real-client'

export { errorResponse, jsonResponse } from '@speed/test-utils/real-client'

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

function projectCall(request: ObservedRequest): RealCall {
  return {
    method: request.method,
    path: request.path,
    query: request.query,
    authorization: request.authorization,
  }
}

/**
 * Binds a real client whose fetch stand-in answers from the script and
 * returns the store the client shares with the host's sign-in flow.
 * Each call binds anew: the runtime seam is last-bind-wins by contract.
 */
export function makeRealClientRig(respond: RealResponder): RealClientRig {
  const { store, calls } = makeRig(respond, projectCall, { refresh: false })
  return { store, calls }
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
