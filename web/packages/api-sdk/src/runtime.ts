/**
 * The single hand-written source file of @speed/api-sdk. Everything else
 * in src/ is orval-generated output carrying DO-NOT-EDIT headers (see
 * web/orval.config.ts); this seam exists because generated code must not
 * know how a host wires its HTTP transport.
 *
 * Generated operation functions and hooks (src/index.ts) call the
 * configured mutator -- `speedRequest`, or the credential-less
 * `speedRequestCredentialless` where web/orval.config.ts overrides it
 * per operation (the session-refresh operation today) -- with one
 * axios-shaped options object. The mutator adapts that call to the
 * @speed/api-client request-function shape and forwards it to the
 * function a host bound once at bootstrap with
 * `bindRequestFn(createClient({...}))`. The transport (base URL, token
 * store, retry, timeout, reporter) is entirely the host's client
 * configuration; the generated surface stays free of fetch/axios/tenant
 * concerns (the speed/no-direct-http ESLint rule whitelists only
 * @speed/api-client).
 *
 * Why a mutable binding instead of a direct import: @speed/api-client
 * exports `createClient`, a factory, not a per-request callable -- a
 * client instance exists only once a host constructs it, and a shared
 * package cannot run host bootstrap code at import time.
 *
 * Nothing here may be edited by regeneration: orval's output paths
 * cover only src/index.ts, never this file. M1 consumer shells bind
 * their client via `@speed/api-sdk/runtime`.
 */

import type {
  HttpMethod,
  RequestFn,
  RequestOptions,
} from '@speed/api-client'

/**
 * The call shape orval-generated functions pass to the configured
 * mutator: one options object, axios-flavoured -- always `url` and
 * `method`, plus `headers`/`params`/`data`/`signal` when the operation
 * has them. Structural only: orval emits plain literals against this
 * shape, so no type is shared across the generation boundary.
 */
interface OrvalCall {
  url: string
  method: HttpMethod
  headers?: Readonly<Record<string, string>>
  params?: Readonly<
    Record<string, string | number | boolean | null | undefined>
  >
  data?: unknown
  signal?: AbortSignal
}

let boundRequestFn: RequestFn | undefined

/**
 * Binds the host's request function -- the return value of
 * `createClient(...)` from @speed/api-client -- at bootstrap, before
 * any generated hook fires. Rebinding replaces the previous function
 * with no once-guard: tests and hot reload rebind freely, and the last
 * bind wins. A generated call made while unbound fails fast with a
 * programmer error instead of issuing an unconfigured request.
 */
export function bindRequestFn(requestFn: RequestFn): void {
  boundRequestFn = requestFn
}

/**
 * Forwards one orval-shaped call to the bound request function; the
 * credential-less flag is per call. With it unset the options object
 * stays exactly the mapped call -- the flag key is only ever present
 * when declared, which hosts' exact-shape assertions rely on.
 */
function forward<T>(call: OrvalCall, omitAccessToken: boolean): Promise<T> {
  const requestFn = boundRequestFn
  if (requestFn === undefined) {
    throw new Error(
      '[speed-api-sdk] no request function bound: call bindRequestFn(createClient(...)) once at bootstrap before any generated hook runs.',
    )
  }
  const options: RequestOptions = {
    method: call.method,
    headers: call.headers,
    query: call.params,
    body: call.data,
    signal: call.signal,
  }
  if (omitAccessToken) {
    options.omitAccessToken = true
  }
  return requestFn<T>(call.url, options)
}

/**
 * The mutator every generated operation function calls unless
 * web/orval.config.ts overrides it per operation. Adapts the
 * axios-shaped orval call to the @speed/api-client request function and
 * forwards it; the host-bound client then performs the whole request
 * lifecycle (authorization header, idempotent retry, timeout, ApiError
 * normalization).
 */
export function speedRequest<T>(call: OrvalCall): Promise<T> {
  return forward<T>(call, false)
}

/**
 * The credential-less mutator, selected per operation by
 * web/orval.config.ts for the session-refresh operation
 * (authn_refreshToken): that operation authenticates with the refresh
 * token in its request body, must never present an access token, and
 * its 401 must stay terminal -- a refused refresh surfaces to the
 * session instead of re-entering the client's own refresh path, which
 * would await the very refresh it is part of (the api-client
 * bearer-only rule). Declaring the omission on the request also keeps
 * the host's token store untouched for every concurrent request that
 * may still hold a valid token.
 */
export function speedRequestCredentialless<T>(call: OrvalCall): Promise<T> {
  return forward<T>(call, true)
}
