/**
 * real-client.ts -- the shared real-client network rig for the
 * workspace's journey tests: a real @speed/api-client createClient over
 * a fetch stand-in answering with genuine Response objects, the memory
 * access-token store, and -- unless the caller opts out -- the session's
 * own refreshAccessToken: () => session.refresh() bound into the
 * api-sdk runtime seam (bindRequestFn, the same seam every generated
 * call uses). Because the transport is real api-client machinery, the
 * 401-refresh leg a journey scripts (a refused request answered 401, a
 * silent credential-less refresh, one retry) is exercised by the client
 * itself rather than scripted around. Each call binds anew: the runtime
 * seam is last-bind-wins by contract.
 *
 * What a recorded call carries is per-package policy: the fetcher
 * observes one request (method, path, query string, authorization
 * header, raw body) and hands it to the caller's projection, which
 * returns the call shape that package's type declares and its suites
 * assert on -- the JSON-decoded body one surface pins, the serialized
 * body another records, the plain request leg a third. The observation
 * itself is the single implementation; only the projection differs.
 *
 * A rig whose caller opts out of the refresh seam (the
 * `refresh: false` option) has no session layer at all: the store
 * starts empty and a test plants the bearer token the reads ride
 * (store.set('access-1')), exactly the state a real host's sign-in flow
 * leaves behind, and a 401 answer surfaces directly -- which is what
 * lets a test script a dead-session code and see the surface's own
 * banner for it.
 *
 * The answer helpers (`jsonResponse`, `errorResponse`, `makePair`,
 * `signInWithPassword`) are the scripted-answer vocabulary every
 * journey shares: a genuine Response carrying the API's envelope
 * shape, the 401-style error envelope, a token-issuing sign-in answer
 * in the shape auth-core parses, and the shared first leg of a
 * signed-in journey.
 */

import {
  createClient,
  createMemoryAccessTokenStore,
} from '@speed/api-client'
import type { AccessTokenStore, RequestFn } from '@speed/api-client'
import { bindRequestFn } from '@speed/api-sdk/runtime'
import type { AuthnTokenPair } from '@speed/api-sdk'
import { createAuthSession } from '@speed/auth-core'
import type { AuthSession } from '@speed/auth-core'

/** A request as the fetch stand-in observed it, before the caller's
 * projection narrows it into that package's recorded call shape. */
export interface ObservedRequest {
  readonly method: string
  /** The request path without the base URL or query string. */
  readonly path: string
  /** The raw query string of the request URL, '' when the request sent
   * none (orval serializes hook params -- a page size, say -- as query
   * parameters). */
  readonly query: string
  /** The authorization header value, or null for a credential-less
   * request (the refresh leg travels credential-less by declaration). */
  readonly authorization: string | null
  /** The serialized request body when the request carried a string
   * body (api-client sends JSON strings), else null. */
  readonly rawBody: string | null
}

/** One scripted endpoint: answer with a genuine Response, immediately
 * or after a deferred promise (journeys hold an exchange open to assert
 * the pending state). */
export type RealResponder<Call> = (call: Call) => Response | Promise<Response>

export interface RealClientRigOptions {
  /**
   * Create an AuthSession over the shared store and bind its
   * refreshAccessToken into the client (the 401-refresh leg). Pass
   * false for a package with no session layer of its own (see the
   * header). Defaults to true.
   */
  readonly refresh?: boolean
}

/** The bound rig, generic over the caller's recorded call shape and
 * over the session half (null when the caller opted out of refresh). */
export interface BoundRealClientRig<Call, Session> {
  /** The access-token store the client (and the session, when there is
   * one) reads on every send. */
  readonly store: AccessTokenStore
  /** The session the journey drives, or null under `refresh: false`. */
  readonly session: Session
  /** The bound client as the RequestFn a host's own composition slots
   * into its services layer. */
  readonly api: RequestFn
  /** Every request observed, in order, in the caller's call shape. */
  readonly calls: Call[]
}

/** Binds a real client whose fetch stand-in answers from the script,
 * recording each request through `project`, and returns the session
 * over the same store (see the header). */
export function makeRealClientRig<Call>(
  respond: RealResponder<Call>,
  project: (request: ObservedRequest) => Call,
): BoundRealClientRig<Call, AuthSession>
export function makeRealClientRig<Call>(
  respond: RealResponder<Call>,
  project: (request: ObservedRequest) => Call,
  options: { readonly refresh: false },
): BoundRealClientRig<Call, null>
export function makeRealClientRig<Call>(
  respond: RealResponder<Call>,
  project: (request: ObservedRequest) => Call,
  options: RealClientRigOptions = {},
): BoundRealClientRig<Call, AuthSession | null> {
  const { refresh = true } = options
  const store = createMemoryAccessTokenStore()
  const session = refresh ? createAuthSession(store) : null
  const calls: Call[] = []
  // The fetch stand-in of a real host, narrowed to a script: it records
  // the request leg and answers from the responder, never touching the
  // network (nothing here invokes fetch). Response objects are genuine,
  // so api-client's envelope parsing runs for real.
  const fetcher: typeof fetch = async (input, init) => {
    const url = new URL(String(input))
    const call = project({
      method: init?.method ?? 'GET',
      path: url.pathname,
      query: url.search,
      authorization: new Headers(init?.headers).get('authorization'),
      rawBody: typeof init?.body === 'string' ? init.body : null,
    })
    calls.push(call)
    return respond(call)
  }
  const client = createClient({
    baseUrl: 'https://api.test',
    fetch: fetcher,
    accessTokenStore: store,
    ...(session === null ? {} : { refreshAccessToken: () => session.refresh() }),
  })
  bindRequestFn(client)
  return { store, session, api: client, calls }
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
export async function signInWithPassword(rig: {
  readonly session: AuthSession
}): Promise<void> {
  await rig.session.loginWithPassword({
    identifier: 'owner@example.test',
    password: 'correct-horse-battery-staple',
  })
}
