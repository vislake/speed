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
 * Fetches the effective Public config values and enabled feature flags
 * for the tenant the request's host resolves to. `api` is the
 * `RequestFn` from `createClient` -- pass one built with no
 * `accessTokenStore`/token (or one that simply has none set) since the
 * endpoint is pre-auth and ignores Authorization either way.
 *
 * Rejects the same `ApiError` (or raw `AbortError` on cancellation)
 * `api` itself would reject with; this function adds no error handling
 * of its own.
 */
export async function fetchPublicConfig(
  api: RequestFn,
  options?: ConfigFetchOptions,
): Promise<PublicConfigResponse> {
  return api<PublicConfigResponse>(CONFIG_PUBLIC_PATH, {
    signal: options?.signal,
  })
}

/**
 * Fetches only the enabled feature flags for the tenant the request's
 * host resolves to -- the lighter of the two endpoints, for a caller
 * that has no use for the Public config values (e.g. an ops/debug
 * tool). Most consumers that also need config values are better served
 * by {@link fetchPublicConfig}, which returns `features` too.
 *
 * Rejects the same `ApiError` (or raw `AbortError` on cancellation)
 * `api` itself would reject with; this function adds no error handling
 * of its own.
 */
export async function fetchSystemFeatures(
  api: RequestFn,
  options?: ConfigFetchOptions,
): Promise<SystemFeaturesResponse> {
  return api<SystemFeaturesResponse>(SYSTEM_FEATURES_PATH, {
    signal: options?.signal,
  })
}
