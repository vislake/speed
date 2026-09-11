/**
 * real-client.ts -- the real-client network rig for @speed/account-ui's
 * journey tests (the component suites of the account surfaces that drive
 * an operation through the wiring a host builds -- a real
 * @speed/api-client createClient over a fetch stand-in answering with
 * genuine Response objects, the memory access-token store, and the
 * session's own refreshAccessToken: () => session.refresh() -- bound
 * into the api-sdk runtime seam, see @speed/test-utils/real-client's
 * header for the shared rig's contract). Because the transport is real
 * api-client machinery, the 401-refresh leg a journey scripts (a
 * refused request answered 401, a silent credential-less refresh, one
 * retry) is exercised by the client itself rather than scripted around.
 *
 * The projection below is this package's recorded call shape: the
 * observed request's JSON-decoded body, `null` when the request carried
 * none or carried an unparseable one -- the suites pin a write's exact
 * payload (the preferences PATCH's partial update), so the decoded
 * object is what they assert on. It is the whole of this package's
 * test story: auth-ui's other half -- a scripted request function for
 * tests that throw raw ApiErrors -- has no counterpart here, because
 * every answer an account surface can render is scriptable as a
 * genuine Response carrying the API envelope.
 */

import type { AccessTokenStore } from '@speed/api-client'
import type { AuthSession } from '@speed/auth-core'
import {
  makeRealClientRig as makeRig,
} from '@speed/test-utils/real-client'
import type { ObservedRequest } from '@speed/test-utils/real-client'

export {
  errorResponse,
  jsonResponse,
  makePair,
  signInWithPassword,
} from '@speed/test-utils/real-client'
export type { AuthnTokenPair } from '@speed/api-sdk'

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
  /** The JSON-decoded request body when the request carried one (a
   * string body that parses as JSON), else null -- for the suites that
   * pin a write's exact payload (the preferences PATCH's partial
   * update, say). */
  readonly body: unknown
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

function projectCall(request: ObservedRequest): RealCall {
  let body: unknown = null
  if (request.rawBody !== null && request.rawBody !== '') {
    try {
      body = JSON.parse(request.rawBody)
    } catch {
      body = null
    }
  }
  return {
    method: request.method,
    path: request.path,
    query: request.query,
    authorization: request.authorization,
    body,
  }
}

/**
 * Binds a real client whose fetch stand-in answers from the script and
 * returns the session over the same store. Each call binds anew: the
 * runtime seam is last-bind-wins by contract.
 */
export function makeRealClientRig(respond: RealResponder): RealClientRig {
  const { session, store, calls } = makeRig(respond, projectCall)
  return { session, store, calls }
}
