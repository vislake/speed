/**
 * Typed fetchers for go/config's two pre-auth public endpoints.
 *
 * Both endpoints resolve tenant entirely server-side, from the
 * request's host (go/config/http.go's handlePublic / handleFeatures) --
 * never from a header or query parameter -- so neither function below
 * accepts a tenant argument of any kind. Passing one would be a silent
 * no-op at best.
 *
 * The path constants mirror go/config's own PathPublic /
 * PathSystemFeatures exactly. No OpenAPI fragment exists for either
 * endpoint yet (go/config/AGENTS.md, Known limitations), so there is no
 * generator keeping these two constants and the two response shapes in
 * sync with the backend -- that is a real, hand-maintained seam, not an
 * oversight. Keep them in sync by hand until a spec fragment lands.
 */

import type { RequestFn, RequestOptions } from './client.js'

/**
 * Mirrors go/config.PathPublic. GET/HEAD, pre-auth; tenant resolved
 * server-side from the request host, falling back to platform defaults
 * rather than erroring when nothing matches.
 */
export const CONFIG_PUBLIC_PATH = '/api/config/public'

/**
 * Mirrors go/config.PathSystemFeatures. Same method/pre-auth/tenant-
 * resolution contract as CONFIG_PUBLIC_PATH.
 */
export const SYSTEM_FEATURES_PATH = '/api/system/features'

/**
 * The wire shape of a GET {@link CONFIG_PUBLIC_PATH} response. `config`
 * stays `Record<string, unknown>` rather than a closed type: the schema
 * is dynamically extensible per-module (a Public config item only
 * appears once its owning module registers it), so a narrower TS type
 * would be wrong today and would need a hand-edit from every future
 * module that adds one. `features` is always a JSON array, sorted,
 * never omitted or null even when empty.
 */
export interface PublicConfigResponse {
  readonly config: Record<string, unknown>
  readonly features: string[]
}

/** The wire shape of a GET {@link SYSTEM_FEATURES_PATH} response. */
export interface SystemFeaturesResponse {
  readonly features: string[]
}

/** Fetch options both functions below accept: cancellation only -- no
 * tenant parameter exists because the server never accepts one from the
 * client for these endpoints. */
export type ConfigFetchOptions = Pick<RequestOptions, 'signal'>

/**
 * Runs one config-endpoint GET and refuses an empty answer. Both
 * endpoints always write a JSON document, so a 2xx that resolves to no
 * body at all (an empty 204-style response -- the RequestFn's own
 * legitimate empty-success shape, which is exactly what a proxy or a
 * broken server can hand back here) is not an empty config: it is a
 * document-shaped failure. Refusing it keeps the empty case
 * distinguishable from real data -- callers see a coded
 * client.protocol ApiError instead of an `undefined` value that passes
 * for `PublicConfigResponse`/`SystemFeaturesResponse` until a consumer
 * reads a field off it.
 *
 * The refusal is the request's own, declared through
 * `RequestOptions.requireJsonBody` (client.ts): the request loop knows
 * the exchange's real outcome, so the client.protocol error it throws
 * for an empty 2xx carries the actual HTTP status and attempt count --
 * a wrapper around the RequestFn could only synthesize both (the
 * hardcoded 0/1 that would otherwise misreport a retried exchange as
 * one attempt that never reached a response).
 */
async function requireConfigDocument<T>(
  api: RequestFn,
  path: string,
  options: ConfigFetchOptions | undefined,
): Promise<T> {
  return api<T>(path, {
    signal: options?.signal,
    requireJsonBody: true,
  })
}

/**
 * Fetches the effective Public config values and enabled feature flags
 * for the tenant the request's host resolves to. `api` is the
 * `RequestFn` from `createClient` -- pass one built with no
 * `accessTokenStore`/token (or one that simply has none set) since the
 * endpoint is pre-auth and ignores Authorization either way.
 *
 * Rejects the same `ApiError` (or raw `AbortError` on cancellation)
 * `api` itself would reject with, plus one refusal: an empty 2xx body
 * (where a config document was required) rejects as client.protocol
 * rather than resolving `undefined` -- raised by the request itself
 * through `requireJsonBody`, so it carries the exchange's real status
 * and attempt count (see {@link requireConfigDocument}).
 */
export async function fetchPublicConfig(
  api: RequestFn,
  options?: ConfigFetchOptions,
): Promise<PublicConfigResponse> {
  return requireConfigDocument<PublicConfigResponse>(
    api,
    CONFIG_PUBLIC_PATH,
    options,
  )
}

/**
 * Fetches only the enabled feature flags for the tenant the request's
 * host resolves to -- the lighter of the two endpoints, for a caller
 * that has no use for the Public config values (e.g. an ops/debug
 * tool). Most consumers that also need config values are better served
 * by {@link fetchPublicConfig}, which returns `features` too.
 *
 * Rejects the same `ApiError` (or raw `AbortError` on cancellation)
 * `api` itself would reject with, plus one refusal: an empty 2xx body
 * (where a features document was required) rejects as client.protocol
 * rather than resolving `undefined` -- raised by the request itself
 * through `requireJsonBody`, so it carries the exchange's real status
 * and attempt count (see {@link requireConfigDocument}).
 */
export async function fetchSystemFeatures(
  api: RequestFn,
  options?: ConfigFetchOptions,
): Promise<SystemFeaturesResponse> {
  return requireConfigDocument<SystemFeaturesResponse>(
    api,
    SYSTEM_FEATURES_PATH,
    options,
  )
}
