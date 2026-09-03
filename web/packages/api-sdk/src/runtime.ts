/**
 * The single hand-written source file of @speed/api-sdk. Everything else
 * in src/ is orval-generated output carrying DO-NOT-EDIT headers (see
 * web/orval.config.ts); this seam exists because generated code must not
 * know how a host wires its HTTP transport.
 *
 * Generated operation functions and hooks (src/index.ts) call the
 * configured mutator -- `speedRequest` -- with one axios-shaped options
 * object. `speedRequest` adapts that call to the @speed/api-client
 * request-function shape and forwards it to the function a host bound
 * once at bootstrap with `bindRequestFn(createClient({...}))`. The
 * transport (base URL, token store, retry, timeout, reporter) is
 * entirely the host's client configuration; the generated surface stays
 * free of fetch/axios/tenant concerns (the speed/no-direct-http ESLint
 * rule whitelists only @speed/api-client).
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
 * The mutator every generated operation function calls. Adapts the
 * axios-shaped orval call to the @speed/api-client request function and
 * forwards it; the host-bound client then performs the whole request
 * lifecycle (authorization header, idempotent retry, timeout, ApiError
 * normalization).
 */
export function speedRequest<T>(call: OrvalCall): Promise<T> {
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
  return requestFn<T>(call.url, options)
}
