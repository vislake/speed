/**
 * real-client.ts -- the real-client network rig for @speed/product-shell's
 * journey tests (usage-example.test.tsx) and its view-machine suite
 * (ProductShell.test.tsx).
 *
 * Those journeys drive the assembled shell over the wiring the package
 * README's quick start documents -- a real @speed/api-client
 * createClient over a fetch stand-in answering with genuine Response
 * objects, the memory access-token store, and the session's own
 * refreshAccessToken: () => session.refresh() -- bound into the api-sdk
 * runtime seam (see @speed/test-utils/real-client's header for the
 * shared rig's contract). Because the transport is real api-client
 * machinery, whatever the responder scripts (including the 401-refresh
 * leg, which api-client exercises itself) is handled by the client
 * rather than scripted around.
 *
 * The projection below is this package's recorded call shape: the
 * request leg plus the serialized body -- the gated-journey suite pins
 * what a tenant switch sent, and a journey test must be able to assert
 * the request it just made. makePair is re-exported here rather than
 * living with the session harness (auth-ui's home for it): the
 * product-shell journeys drive only the happy paths -- a token-issuing
 * login and a 204 logout -- so the pair is the only answer shape the
 * suite scripts, and one rig file keeps them together.
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
} from '@speed/test-utils/real-client'

/** A request as the fetch stand-in observed it. */
export interface RealCall {
  readonly method: string
  /** The request path without the base URL or query string. */
  readonly path: string
  /** The authorization header value, or null for a credential-less
   * request (the refresh leg travels credential-less by declaration). */
  readonly authorization: string | null
  /** The serialized request body -- api-client sends JSON strings --
   * or null when the request carried none. */
  readonly body: string | null
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
  return {
    method: request.method,
    path: request.path,
    authorization: request.authorization,
    body: request.rawBody,
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
