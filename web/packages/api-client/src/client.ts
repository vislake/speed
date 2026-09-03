/**
 * The request client: createClient wires every seam -- injectable
 * fetch, the access-token store, the refresh hook, retry timing, the
 * timeout and the reporter -- into one request function, which performs
 * the whole request lifecycle on each call:
 *
 *   1. attach `Authorization: Bearer <token>` from the store (per
 *      attempt, so a refreshed token is picked up on retry);
 *   2. send; a caller-supplied signal cancels the request raw (the
 *      AbortError reaches the caller, standard query-cancellation
 *      semantics -- nothing is wrapped or retried);
 *   3. on HTTP 401 with a refreshAccessToken hook configured: run one
 *      refresh, coalescing concurrent 401s onto a single in-flight
 *      refresh, and retry the original request exactly once (any
 *      method, outside the retry budget). Refresh failure surfaces the
 *      original 401 as a distinguishable auth ApiError and is reported
 *      through the Reporter;
 *   4. on 429/502/503/504, network failures and timeouts: retry only
 *      idempotent methods (GET/HEAD/OPTIONS), exponential full-jitter
 *      backoff per RetryPolicy, Retry-After honoured on 429 and 503;
 *   5. normalize the outcome: 2xx bodies parse as JSON (empty bodies
 *      resolve undefined), and every failure rejects an ApiError --
 *      envelope errors keep code/traceId/params/details, everything
 *      else gets the reserved client.* vocabulary from errors.ts.
 *
 * The tenant never appears here: no tenant header exists anywhere in
 * the package (docs/internal/12-frontend.md) -- tenant context travels
 * in the access token, and nothing in this file reads or stores data
 * beyond the current request.
 */

import {
  ApiError,
  ERROR_CODE_NETWORK,
  ERROR_CODE_PROTOCOL,
  ERROR_CODE_TIMEOUT,
  httpErrorCode,
  type FieldError,
} from './errors.js'
import type { AccessTokenStore } from './access-token.js'
import {
  DEFAULT_RETRY_POLICY,
  retryAfterDelayMs,
  retryDelayMs,
  type RetryPolicy,
} from './retry.js'
import { createConsoleReporter, type Reporter } from './reporter.js'

/** HTTP methods the request function accepts. */
export type HttpMethod =
  | 'GET'
  | 'HEAD'
  | 'OPTIONS'
  | 'POST'
  | 'PUT'
  | 'PATCH'
  | 'DELETE'

/**
 * Per-request options. Paths and response types come from the spec
 * (`task api:gen` -> @speed/api-sdk); this layer stays spec-agnostic.
 */
export interface RequestOptions {
  /** HTTP method; GET when omitted. */
  method?: HttpMethod
  /**
   * Extra request headers. `accept` defaults to application/json;
   * `content-type` defaults to application/json when a body is sent;
   * and `authorization` is reserved -- when the store holds a token it
   * overwrites a caller-supplied value, because the store is the
   * session's single source of truth.
   */
  headers?: Readonly<Record<string, string>>
  /**
   * Query parameters, URL-encoded and appended to the path; null and
   * undefined entries are skipped. Put parameter values here, never in
   * the path string.
   */
  query?: Readonly<
    Record<string, string | number | boolean | null | undefined>
  >
  /** JSON request body; serialized with the JSON content type. */
  body?: unknown
  /**
   * Caller cancellation: aborting the signal rejects with the raw
   * AbortError (no ApiError, no retry) so query layers (TanStack Query)
   * keep standard cancellation semantics.
   */
  signal?: AbortSignal
}

/**
 * The request function createClient returns. Generic over the parsed
 * response type: a 2xx JSON body resolves as `T`, an empty 2xx body
 * (204-style) resolves undefined, and every failure rejects an
 * ApiError (see errors.ts). In TSX files annotate calls as
 * `api<T,>(path)` -- the trailing comma disambiguates from JSX.
 */
export type RequestFn = <T>(
  path: string,
  options?: RequestOptions,
) => Promise<T>

/** Everything createClient needs. */
export interface ClientOptions {
  /**
   * Scheme + host + optional prefix, e.g. 'https://api.example.com' or
   * '/api/v1' for the same-origin dev host. Trailing slashes are
   * stripped; request paths must start with '/' and are appended
   * verbatim.
   */
  baseUrl: string
  /**
   * The fetch implementation. Defaults to globalThis.fetch captured at
   * construction -- and construction throws when neither is available,
   * so an environment without fetch fails fast instead of failing per
   * request. Tests always inject a deterministic stand-in.
   */
  fetch?: typeof fetch
  /**
   * The bearer-token store; absent, requests go out without
   * Authorization (public-config style endpoints).
   */
  accessTokenStore?: AccessTokenStore
  /**
   * Silent-401-refresh hook: resolves true when a fresh access token
   * was stored (via accessTokenStore) and the original request may be
   * retried once. The M1 authn round supplies it against the
   * session-refresh endpoint; hosts without a session leave it out and
   * every 401 surfaces as an auth ApiError. Never called more than
   * once per request; concurrent 401s share one in-flight refresh.
   */
  refreshAccessToken?: () => Promise<boolean>
  /** Abort requests that exceed this many milliseconds; absent, no
   * internal timeout is armed. */
  timeoutMs?: number
  /** Transient-retry budget and timing; defaults to
   * DEFAULT_RETRY_POLICY. */
  retryPolicy?: RetryPolicy
  /** Structured diagnostics sink; defaults to createConsoleReporter()
   * until the M1 round wires the real pipeline. */
  reporter?: Reporter
}

/** Statuses transient-retried on idempotent methods. */
const RETRYABLE_STATUSES = new Set([429, 502, 503, 504])

/** Methods safe to repeat automatically after a lost response. */
function isIdempotent(method: HttpMethod): boolean {
  return method === 'GET' || method === 'HEAD' || method === 'OPTIONS'
}

/** A response that arrived, with its status, before any body is read. */
interface HttpOutcome {
  kind: 'http'
  status: number
  response: Response
}

/** The request never reached a usable response. */
interface FailureOutcome {
  kind: 'network' | 'timeout'
  cause: unknown
}

type AttemptOutcome = HttpOutcome | FailureOutcome

/** A valid envelope, structurally checked. */
interface Envelope {
  code: string
  traceId: string
  message: string | undefined
  params: Record<string, unknown> | undefined
  details: FieldError[] | undefined
}

/**
 * Parses a response body into the ApiError envelope, or undefined when
 * the body is empty, not JSON, not an object, or lacks the required
 * code/traceId strings -- those stay bare and get a client.http.* code.
 */
function parseEnvelope(text: string | null): Envelope | undefined {
  if (text === null || text.trim() === '') {
    return undefined
  }
  let parsed: unknown
  try {
    parsed = JSON.parse(text)
  } catch {
    return undefined
  }
  if (typeof parsed !== 'object' || parsed === null) {
    return undefined
  }
  const candidate = parsed as Record<string, unknown>
  if (
    typeof candidate.code !== 'string' ||
    typeof candidate.traceId !== 'string'
  ) {
    return undefined
  }
  const envelope: Envelope = {
    code: candidate.code,
    traceId: candidate.traceId,
    message: undefined,
    params: undefined,
    details: undefined,
  }
  if (typeof candidate.message === 'string') {
    envelope.message = candidate.message
  }
  if (
    typeof candidate.params === 'object' &&
    candidate.params !== null &&
    !Array.isArray(candidate.params)
  ) {
    envelope.params = candidate.params as Record<string, unknown>
  }
  if (Array.isArray(candidate.details)) {
    envelope.details = parseFieldErrors(candidate.details)
  }
  return envelope
}

/**
 * Keeps only the entries of an envelope `details` array that are
 * structurally valid FieldErrors (string `field` and `code`; `params`,
 * when present, a plain object) -- the same strict normalization the
 * envelope applies to its own fields, so consumers never render a
 * typed-but-wrong entry.
 */
function parseFieldErrors(entries: unknown[]): FieldError[] {
  const fieldErrors: FieldError[] = []
  for (const entry of entries) {
    if (typeof entry !== 'object' || entry === null) {
      continue
    }
    const candidate = entry as Record<string, unknown>
    if (
      typeof candidate.field !== 'string' ||
      typeof candidate.code !== 'string'
    ) {
      continue
    }
    const fieldError: FieldError = {
      field: candidate.field,
      code: candidate.code,
    }
    if (
      typeof candidate.params === 'object' &&
      candidate.params !== null &&
      !Array.isArray(candidate.params)
    ) {
      fieldError.params = candidate.params as Record<string, unknown>
    }
    fieldErrors.push(fieldError)
  }
  return fieldErrors
}

/** Serializes the JSON body option; undefined means no body. */
function serializeBody(body: unknown): string | undefined {
  if (body === undefined) {
    return undefined
  }
  const serialized = JSON.stringify(body)
  return serialized === undefined ? undefined : serialized
}

/** Builds the wire URL: baseUrl + path, query parameters appended. */
function buildUrl(
  baseUrl: string,
  path: string,
  query: RequestOptions['query'],
): string {
  let url = `${baseUrl}${path}`
  if (query !== undefined) {
    const params = new URLSearchParams()
    for (const [key, value] of Object.entries(query)) {
      if (value === undefined || value === null) {
        continue
      }
      params.append(key, String(value))
    }
    const encoded = params.toString()
    if (encoded !== '') {
      url += `?${encoded}`
    }
  }
  return url
}

/** Programmer-error guard with the package's error prefix. */
function programmerError(message: string): Error {
  return new Error(`[speed-api-client] ${message}`)
}

/** Whether a failed outcome qualifies for a transient retry. */
function retryableOutcome(outcome: AttemptOutcome): boolean {
  if (outcome.kind === 'http') {
    return RETRYABLE_STATUSES.has(outcome.status)
  }
  return true
}

/** The delay before a retry of an HTTP failure: Retry-After honoured
 * on 429 and 503 (RFC 9110 carries the header on those), capped by the
 * policy; exponential full-jitter otherwise. */
function retryDelayFor(
  outcome: HttpOutcome,
  retryIndex: number,
  policy: RetryPolicy,
): number {
  if (outcome.status === 429 || outcome.status === 503) {
    const header = outcome.response.headers.get('retry-after')
    if (header !== null) {
      const parsed = retryAfterDelayMs(header)
      if (parsed !== null) {
        return Math.min(parsed, policy.maxDelayMs)
      }
    }
  }
  return retryDelayMs(retryIndex, policy)
}

/** Reads and structurally checks the error envelope from an HTTP
 * failure's body; undefined when the body is empty, unreadable or
 * carries no valid envelope. A response body is single-read, so callers
 * that need the envelope twice (a report and the ApiError) read once
 * and reuse it. */
async function readEnvelope(
  outcome: HttpOutcome,
): Promise<Envelope | undefined> {
  let text: string | null
  try {
    text = await outcome.response.text()
  } catch {
    text = null
  }
  return parseEnvelope(text)
}

/** The final ApiError for an HTTP failure from an envelope that was
 * already read: the envelope error when one is present,
 * client.http.<status> otherwise. */
function envelopeError(
  outcome: HttpOutcome,
  envelope: Envelope | undefined,
  attempts: number,
): ApiError {
  if (envelope !== undefined) {
    return new ApiError({
      status: outcome.status,
      code: envelope.code,
      traceId: envelope.traceId,
      message: envelope.message,
      params: envelope.params,
      details: envelope.details,
      attempts,
    })
  }
  return new ApiError({
    status: outcome.status,
    code: httpErrorCode(outcome.status),
    attempts,
  })
}

/** The final ApiError for an HTTP failure (reads the body once). */
async function httpError(
  outcome: HttpOutcome,
  attempts: number,
): Promise<ApiError> {
  return envelopeError(outcome, await readEnvelope(outcome), attempts)
}

/** The final ApiError for a transport-class failure. */
function failureError(outcome: FailureOutcome, attempts: number): ApiError {
  return new ApiError({
    status: 0,
    code: outcome.kind === 'timeout' ? ERROR_CODE_TIMEOUT : ERROR_CODE_NETWORK,
    attempts,
    cause: outcome.cause,
  })
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => {
    setTimeout(resolve, ms)
  })
}

/** Throws the raw AbortError the moment a cancelled signal is observed
 * after an await, so caller cancellation is never retried, never
 * wrapped, and never delivered as a result (RequestOptions.signal). */
function throwIfAborted(signal: AbortSignal | undefined): void {
  if (signal?.aborted === true) {
    throw new DOMException('The operation was aborted.', 'AbortError')
  }
}

/**
 * Creates the request function. Construction is where configuration
 * mistakes surface (bad baseUrl, unusable fetch, malformed policy);
 * every request afterwards rejects ApiError or the caller's own
 * AbortError, never a configuration error.
 */
export function createClient(options: ClientOptions): RequestFn {
  if (typeof options.baseUrl !== 'string' || options.baseUrl === '') {
    throw programmerError(
      'createClient requires a non-empty baseUrl (host + optional prefix; see the README).',
    )
  }
  const baseUrl = options.baseUrl.replace(/\/+$/, '')
  const fetchFn =
    options.fetch ??
    (typeof globalThis.fetch === 'function' ? globalThis.fetch : undefined)
  if (fetchFn === undefined) {
    throw programmerError(
      'no fetch implementation available: pass ClientOptions.fetch (this environment has no global fetch).',
    )
  }
  const tokenStore = options.accessTokenStore
  const retryPolicy = options.retryPolicy ?? DEFAULT_RETRY_POLICY
  if (
    !Number.isInteger(retryPolicy.maxAttempts) ||
    retryPolicy.maxAttempts < 1
  ) {
    throw programmerError(
      'retryPolicy.maxAttempts must be an integer >= 1 (1 disables transient retries).',
    )
  }
  if (
    !Number.isFinite(retryPolicy.initialDelayMs) ||
    retryPolicy.initialDelayMs < 0 ||
    !Number.isFinite(retryPolicy.maxDelayMs) ||
    retryPolicy.maxDelayMs < 0
  ) {
    throw programmerError(
      'retryPolicy delays must be finite, non-negative milliseconds.',
    )
  }
  const timeoutMs = options.timeoutMs
  if (timeoutMs !== undefined && (!Number.isFinite(timeoutMs) || timeoutMs <= 0)) {
    throw programmerError(
      'timeoutMs must be a positive number of milliseconds.',
    )
  }
  const reporter = options.reporter ?? createConsoleReporter()

  /** One HTTP attempt: token attach, timeout abort, caller-abort
   * passthrough. Rejects raw only for caller cancellation. */
  const attemptOnce = async (
    method: HttpMethod,
    url: string,
    requestOptions: RequestOptions,
    signal: AbortSignal | undefined,
  ): Promise<AttemptOutcome> => {
    const headers = new Headers(requestOptions.headers ?? {})
    headers.set('accept', 'application/json')
    const body = serializeBody(requestOptions.body)
    if (body !== undefined && !headers.has('content-type')) {
      headers.set('content-type', 'application/json')
    }
    // The token is read per attempt: a retry after a successful refresh
    // picks up the fresh token without extra plumbing.
    const token = tokenStore?.get() ?? null
    if (token !== null && token !== '') {
      headers.set('authorization', `Bearer ${token}`)
    }

    const controller = new AbortController()
    let timedOut = false
    let timer: ReturnType<typeof setTimeout> | undefined
    if (timeoutMs !== undefined) {
      timer = setTimeout(() => {
        timedOut = true
        controller.abort()
      }, timeoutMs)
    }
    const forwardAbort = (): void => {
      controller.abort()
    }
    if (signal !== undefined) {
      signal.addEventListener('abort', forwardAbort, { once: true })
    }

    try {
      const response = await fetchFn(url, {
        method,
        headers,
        body,
        signal: controller.signal,
      })
      return { kind: 'http', status: response.status, response }
    } catch (error) {
      if (timedOut) {
        return { kind: 'timeout', cause: error }
      }
      if (signal?.aborted === true) {
        // Caller cancellation: surface the raw AbortError, standard
        // query-layer semantics -- never wrapped, never retried.
        throw error
      }
      return { kind: 'network', cause: error }
    } finally {
      if (timer !== undefined) {
        clearTimeout(timer)
      }
      if (signal !== undefined) {
        signal.removeEventListener('abort', forwardAbort)
      }
    }
  }

  // Single-flight refresh: at most one refresh in flight across all
  // concurrent requests. A completed refresh (success or failure) is
  // not memoized -- the next 401 starts a fresh attempt, so a session
  // restored by another tab can be picked up.
  const refreshHook = options.refreshAccessToken
  let refreshInFlight: Promise<boolean> | null = null
  const refreshOnce =
    refreshHook === undefined
      ? undefined
      : (): Promise<boolean> => {
          if (refreshInFlight === null) {
            refreshInFlight = (async () => {
              try {
                return (await refreshHook()) === true
              } catch {
                // A throwing refresh hook is a failed refresh: the
                // session is still gone and the request must not retry.
                return false
              } finally {
                refreshInFlight = null
              }
            })()
          }
          return refreshInFlight
        }

  const request: RequestFn = async <T>(
    path: string,
    requestOptions: RequestOptions = {},
  ): Promise<T> => {
    if (typeof path !== 'string' || !path.startsWith('/')) {
      throw programmerError(
        `request path must be an absolute path starting with "/" (baseUrl carries host and prefix); got ${JSON.stringify(path)}.`,
      )
    }
    const method = (requestOptions.method ?? 'GET').toUpperCase() as HttpMethod
    const idempotent = isIdempotent(method)
    const url = buildUrl(baseUrl, path, requestOptions.query)
    const signal = requestOptions.signal
    // Real fetch rejects an already-aborted signal; stand-ins in
    // tests may not, so normalize before sending.
    throwIfAborted(signal)

    let attempts = 0
    let refreshed = false
    for (;;) {
      attempts += 1
      const outcome = await attemptOnce(method, url, requestOptions, signal)
      // The caller may have aborted while the request was in flight:
      // cancellation wins over any outcome, so nothing below may retry
      // or deliver on behalf of a caller that gave up.
      throwIfAborted(signal)

      if (outcome.kind === 'http' && isSuccessStatus(outcome.status)) {
        let body: string
        try {
          body = await outcome.response.text()
        } catch (error) {
          // The headers said 2xx but the body never arrived: a
          // network-class failure, retryable like any other -- except
          // when the caller aborted during the read, in which case the
          // failure IS the cancellation and surfaces raw.
          throwIfAborted(signal)
          if (idempotent && attempts < retryPolicy.maxAttempts) {
            await sleep(retryDelayMs(attempts - 1, retryPolicy))
            // An abort during the backoff cancels the retry: the next
            // attempt must not fire after the caller cancelled.
            throwIfAborted(signal)
            continue
          }
          throw failureError({ kind: 'network', cause: error }, attempts)
        }
        // The body arrived; an abort during the read cancels the
        // delivery instead of resolving a 2xx for a cancelled caller.
        throwIfAborted(signal)
        if (body === '') {
          // 204-style: no content is a valid, empty success.
          return undefined as T
        }
        let data: unknown
        try {
          data = JSON.parse(body)
        } catch {
          throw new ApiError({
            status: outcome.status,
            code: ERROR_CODE_PROTOCOL,
            attempts,
            cause: new SyntaxError('2xx body is not valid JSON'),
          })
        }
        if (typeof data !== 'object' || data === null) {
          throw new ApiError({
            status: outcome.status,
            code: ERROR_CODE_PROTOCOL,
            attempts,
            cause: new SyntaxError('2xx body is not a JSON value'),
          })
        }
        return data as T
      }

      if (outcome.kind === 'http' && outcome.status === 401) {
        if (!refreshed && refreshOnce !== undefined) {
          refreshed = true
          const refreshedOk = await refreshOnce()
          // The caller may have aborted while the refresh was in
          // flight: cancellation wins -- never send the post-refresh
          // retry, never deliver an auth error either.
          throwIfAborted(signal)
          if (refreshedOk) {
            // Retry once with whatever token the store holds now (the
            // token is re-read at send time). Orthogonal to the retry
            // budget and to method idempotency.
            continue
          }
          // Refresh failed: read the 401 body once and reuse it for
          // the report and the error, so the warning carries the
          // envelope's code and traceId -- correlating it to server
          // logs -- instead of firing blind.
          const envelope = await readEnvelope(outcome)
          // An abort during that read wins over the auth error.
          throwIfAborted(signal)
          reporter.warn('access token refresh failed', {
            status: outcome.status,
            ...(envelope === undefined
              ? {}
              : { code: envelope.code, traceId: envelope.traceId }),
          })
          throw envelopeError(outcome, envelope, attempts)
        }
        // No hook, or the retried request was refused again: the
        // session is over, surface the auth error.
        throw await httpError(outcome, attempts)
      }

      if (
        idempotent &&
        attempts < retryPolicy.maxAttempts &&
        retryableOutcome(outcome)
      ) {
        const delay =
          outcome.kind === 'http'
            ? retryDelayFor(outcome, attempts - 1, retryPolicy)
            : retryDelayMs(attempts - 1, retryPolicy)
        await sleep(delay)
        // An abort during the backoff cancels the retry: the next
        // attempt must not fire after the caller cancelled.
        throwIfAborted(signal)
        continue
      }

      if (outcome.kind === 'http') {
        throw await httpError(outcome, attempts)
      }
      throw failureError(outcome, attempts)
    }
  }

  return request
}

function isSuccessStatus(status: number): boolean {
  return status >= 200 && status <= 299
}
