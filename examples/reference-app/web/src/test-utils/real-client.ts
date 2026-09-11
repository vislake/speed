/**
 * real-client.ts -- the real-client network rig for reference-app-web's
 * journey tests (component suites and usage-example journeys that drive
 * an operation through the wiring a real host builds -- a real
 * @speed/api-client createClient over a fetch stand-in answering with
 * genuine Response objects, the memory access-token store, and the
 * session's own refreshAccessToken: () => session.refresh() -- bound
 * into the api-sdk runtime seam, see @speed/test-utils/real-client's
 * header for the shared rig's contract). Because the transport is real
 * api-client machinery, a request answered 401 would drive the client's
 * own single-flight refresh and one retry rather than a scripted leg --
 * but no journey at this tier scripts one today: the demo-server
 * answers no 401, so the refresh leg stays dormant here, its in-form
 * exercise living in the packages' own usage-example suites, where a
 * scripted responder answers 401.
 *
 * The projection below is this package's recorded call shape: the
 * request leg plus the raw serialized body, '' when the request sent
 * none. Responders whose answer depends on the payload (the
 * demo-server's tenant-switch answer reads the requested tenant_id from
 * here) parse it themselves, and suites that must pin a write's payload
 * assert on the same string. The rig also exposes the bound client
 * as the RequestFn a host slots into its services layer, so app-services
 * units consume the rig exactly as the app bootstrap's composition does.
 */

import type { AccessTokenStore, RequestFn } from '@speed/api-client'
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
   * none (orval serializes hook params as query parameters). */
  readonly query: string
  /** The authorization header value, or null for a credential-less
   * request (the refresh leg travels credential-less by declaration). */
  readonly authorization: string | null
  /** The raw request body as the fetch stand-in received it, '' when the
   * request sent none. Responders whose answer depends on the payload
   * (the demo-server's tenant-switch answer reads the requested
   * tenant_id from here) parse it themselves. */
  readonly body: string
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
  /**
   * The bound client as the RequestFn the config hooks fetch through
   * -- the same value a host passes to its AppServicesProvider's api
   * slot, so app-services units consume the rig exactly as the
   * bootstrap's composition does.
   */
  readonly api: RequestFn
  /** Every request observed, in order. */
  readonly calls: RealCall[]
}

function projectCall(request: ObservedRequest): RealCall {
  return {
    method: request.method,
    path: request.path,
    query: request.query,
    authorization: request.authorization,
    body: request.rawBody ?? '',
  }
}

/**
 * Binds a real client whose fetch stand-in answers from the script and
 * returns the session over the same store. Each call binds anew: the
 * runtime seam is last-bind-wins by contract.
 */
export function makeRealClientRig(respond: RealResponder): RealClientRig {
  const { session, store, api, calls } = makeRig(respond, projectCall)
  return { session, store, api, calls }
}
