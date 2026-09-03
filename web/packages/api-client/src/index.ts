/**
 * @speed/api-client -- the hand-written HTTP runtime under the speed
 * frontend (the counterpart of the api-contract discipline in
 * docs/internal/21-api-contract.md).
 *
 * createClient wires the seams (injectable fetch, memory-only access
 * token store, refresh hook, retry policy, timeout, reporter) into one
 * request function; every failure rejects an ApiError carrying the
 * envelope's code/traceId or a reserved client.* code. @speed/api-sdk
 * (the orval-generated surface) calls this layer; no other package
 * hand-writes HTTP -- enforced by the speed/no-direct-http ESLint
 * rule, whose single whitelist this package is.
 *
 * No i18n resources ship here: ApiError codes map to user-facing text
 * in the consuming package's own catalogs, and nothing in this package
 * emits user-facing text. No storage API exists anywhere in this
 * package -- tokens live in memory only (see access-token.ts).
 */

export { createClient } from './client.js'
export type {
  ClientOptions,
  HttpMethod,
  RequestFn,
  RequestOptions,
} from './client.js'
export {
  createMemoryAccessTokenStore,
} from './access-token.js'
export type { AccessTokenStore } from './access-token.js'
export {
  ApiError,
  ERROR_CODE_NETWORK,
  ERROR_CODE_PROTOCOL,
  ERROR_CODE_TIMEOUT,
  httpErrorCode,
  isApiError,
} from './errors.js'
export type { ApiErrorInit, FieldError } from './errors.js'
export {
  DEFAULT_RETRY_POLICY,
  retryAfterDelayMs,
  retryDelayMs,
} from './retry.js'
export type { RetryPolicy } from './retry.js'
export { createConsoleReporter } from './reporter.js'
export type { Reporter } from './reporter.js'
