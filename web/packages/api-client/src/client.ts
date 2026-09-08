/**
 * The request client: createClient wires every seam -- injectable
 * fetch, the access-token store, the refresh hook, retry timing, the
 * timeout and the reporter -- into one request function, which performs
 * the whole request lifecycle on each call:
 *
 *   1. attach `Authorization: Bearer <token>` from the store (per
 *      attempt, so a refreshed token is picked up on retry) -- unless
 *      the request declares `omitAccessToken`, in which case the store
 *      is never read and no store token is attached (a caller-supplied
 *      `authorization` header, when one is given, is the caller's own
 *      and travels untouched);
 *   2. send; a caller-supplied signal cancels the request raw (the
 *      AbortError reaches the caller, standard query-cancellation
 *      semantics -- nothing is wrapped or retried). Cancellation
 *      covers the whole attempt, the response body included: the
 *      per-attempt timeout keeps running and the caller's signal
 *      keeps aborting the request until the body has settled (read in
 *      full, failed, or released), so a server that answers headers
 *      and then stalls its body cannot outlive either the timeout or
 *      an abort;
 *   3. on HTTP 401 from a request that presented a bearer token, with
 *      a refreshAccessToken hook configured: run one refresh,
 *      coalescing concurrent 401s onto a single in-flight refresh, and
 *      retry the original request exactly once (any method, outside
 *      the retry budget -- the refresh round performs no transient
 *      retry and never consumes one). The refresh round is a separate
 *      exchange of its own (a host's refresh request travels through
 *      this same client and carries its own timeout): the refused
 *      attempt's timeout is suspended across it, so a refresh that
 *      outlives timeoutMs never fires the attempt timeout into the
 *      401 envelope read that follows a failed refresh -- the real
 *      envelope (code and trace id) survives. A 401 on a
 *      credential-less request
 *      means authentication is required -- refreshing cannot provide
 *      it -- so it surfaces untouched, which also keeps a session's
 *      own refresh request (sent credential-less) from re-entering
 *      the refresh path (and a host that forgot that declaration on
 *      its refresh request is refused the refresh path at runtime the
 *      same way). Caller cancellation is never muted by the pause: an
 *      abort landing during the refresh round races the round's wait
 *      and rejects the request raw -- the flight itself races no
 *      signal, so only the aborting request detaches, and the waiters
 *      already joined on the round still get the hook's real outcome
 *      when it settles. Refresh failure surfaces the original 401 as
 *      a distinguishable auth ApiError and is reported through the
 *      Reporter;
 *   4. on 429/502/503/504, network failures and timeouts: retry only
 *      idempotent methods (GET/HEAD/OPTIONS), exponential full-jitter
 *      backoff per RetryPolicy, Retry-After honoured on 429 and 503.
 *      The retry budget bounds transient retries, never the refresh
 *      round; a response being discarded for a retry (a retryable
 *      status, a refused 401 whose refresh succeeded) has its body
 *      cancelled first, so the connection is released instead of left
 *      held by an unread response;
 *   5. normalize the outcome: 2xx bodies parse as JSON (empty bodies
 *      resolve undefined -- unless the request declared
 *      `requireJsonBody`, which refuses an empty 2xx as client.protocol
 *      with the exchange's real status and attempts), and every
 *      failure rejects an ApiError -- envelope errors keep the
 *      envelope's code (plus its traceId, params, message and details
 *      when the backend sent them -- code is the only required wire
 *      field), everything else gets the reserved client.* vocabulary
 *      from errors.ts. A request body that cannot be JSON-serialized
 *      (a circular structure, a top-level function or symbol, a
 *      toJSON() that yields nothing) rejects as client.protocol before
 *      anything is sent.
 *
 * The tenant never appears here: no tenant header exists anywhere in
 * the package -- tenant context travels in the access token, and
 * nothing in this file reads or stores data beyond the current request.
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
   * Extra request headers. `accept` defaults to application/json and
   * `content-type` to application/json when a body is sent -- each
   * only when the caller did not already set one, so a caller-supplied
   * value is honoured. `authorization` is reserved whenever the store
   * is consulted: when the store holds a token it overwrites a
   * caller-supplied value, because the store is the session's single
   * source of truth. Declare `omitAccessToken` below to send the
   * request without the store's token instead -- the store is then
   * never read, so a caller-supplied `authorization` (when one is
   * given) is the caller's own and travels untouched.
   */
  headers?: Readonly<Record<string, string>>
  /**
   * Declares this request store-token-less: the token store is never
   * read, and no token from it is attached, no matter what it holds --
   * the request carries only the caller's own headers. Without a
   * caller-supplied `authorization` that means the request travels
   * credential-less. A session's own refresh operation is the
   * canonical user -- it authenticates with the refresh token in its
   * body, and its 401 must stay terminal under the bearer-only rule
   * (see `ClientOptions.refreshAccessToken`) -- and declaring this on
   * the request keeps the store untouched for every concurrent
   * request, which may still be presenting a perfectly valid token.
   */
  omitAccessToken?: boolean
  /**
   * Query parameters, URL-encoded and appended to the path; null and
   * undefined entries are skipped. Put parameter values here, never in
   * the path string. An array value is encoded as repeated parameters
   * -- the form/explode convention a Go `r.URL.Query()` handler parses
   * (`tag: ['a', 'b']` becomes `?tag=a&tag=b`), never comma-joined
   * into one parameter; array entries must be scalars, and null or
   * undefined entries are skipped inside arrays as at the top level.
   */
  query?: Readonly<
    Record<
      string,
      | string
      | number
      | boolean
      | ReadonlyArray<string | number | boolean>
      | null
      | undefined
    >
  >
  /** JSON request body; serialized with the JSON content type. */
  body?: unknown
  /**
   * Declares that this request needs a JSON document in its 2xx body:
   * an empty 2xx body (the client's own 204-style empty-success
   * shape) then refuses as a client.protocol ApiError carrying the
   * exchange's real status and attempt count -- never a resolved
   * `undefined` that passes for the typed document. The config
   * fetchers in config-fetcher.ts are the canonical users: both
   * go/config endpoints always write a JSON document, so an empty
   * answer there is a broken one.
   */
  requireJsonBody?: boolean
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
   * retried once. @speed/auth-core supplies it against the
   * session-refresh operation (`() => session.refresh()`); hosts
   * without a session leave it out and every 401 surfaces as an auth
   * ApiError. Never called more than once per request; concurrent 401s
   * share one in-flight refresh. A caller abort landing while the
   * round is in flight rejects the request with the raw AbortError on
   * the next microtask -- the round's wait races the caller's own
   * signal like every other wait in the request loop, so each joined
   * request is bounded by its own signal -- while the flight itself
   * races no signal: it settles when the hook settles, whatever
   * happened to the request that started it, so an initiator that gave
   * up mid round never settles a possibly-successful refresh as a
   * failure for the waiters already joined on it.
   *
   * The hook fires only for a refused request that itself presented a
   * bearer token. A 401 on a credential-less request means the
   * endpoint demands authentication that refreshing cannot provide,
   * so it surfaces untouched -- and this rule is load-bearing for the
   * session wiring: a session's own refresh operation declares the
   * per-request `omitAccessToken` flag, so its request travels
   * credential-less with the store never cleared, so when the refresh
   * endpoint refuses a stale token the refusal surfaces instead of
   * re-entering the refresh path, which would await the very refresh
   * it is part of and deadlock. Any other client must keep its
   * credential-less requests declared the same way for the same
   * reason: clearing the store instead would momentarily strip the
   * token from concurrent requests that still hold a valid one,
   * turning their 401s into spurious auth failures. A host that
   * forgets the declaration on its own refresh request is caught at
   * runtime instead: a request born inside the refresh round is
   * refused the refresh path (its 401 surfaces as the terminal auth
   * error), so the round ends as a finite failure -- never a
   * self-deadlock and never a second concurrent refresh.
   */
  refreshAccessToken?: () => Promise<boolean>
  /** Abort an HTTP exchange that exceeds this many milliseconds;
   * absent, no internal timeout is armed. The budget bounds one
   * exchange -- a send plus its body read -- and covers stalled body
   * reads the way it covers a stalled send. Time spent in the
   * silent-401-refresh round does not count against it: the refresh
   * is a separate exchange (a host's refresh request travels through
   * this same client and carries its own timeout). */
  timeoutMs?: number
  /** Transient-retry budget and timing; defaults to
   * DEFAULT_RETRY_POLICY. */
  retryPolicy?: RetryPolicy
  /** Structured diagnostics sink; defaults to createConsoleReporter(). */
  reporter?: Reporter
}

/** Statuses transient-retried on idempotent methods. */
const RETRYABLE_STATUSES = new Set([429, 502, 503, 504])

/** Methods safe to repeat automatically after a lost response. */
function isIdempotent(method: HttpMethod): boolean {
  return method === 'GET' || method === 'HEAD' || method === 'OPTIONS'
}

/** A response that arrived, with its status, before any body is read.
 * `attachedToken` records whether the attempt carried a bearer token:
 * the silent-401 refresh only applies to a refused attempt that
 * presented one. The body stays inside the attempt's abort scope --
 * the per-attempt timeout and the caller-abort forwarding keep
 * running until the body settles (see the header) -- so an http
 * outcome carries the three scope operations below. Callers must let
 * the body settle (a completed `readBody`, or `cancelBody` for a
 * response being discarded) and then call `dispose()` exactly once;
 * the request loop guarantees this with a finally. */
interface HttpOutcome {
  kind: 'http'
  status: number
  response: Response
  attachedToken: boolean
  /** Reads the body to the end, or settles early when this attempt is
   * aborted mid-read: the attempt's own timeout yields `timeout`, the
   * caller's signal `aborted`, any other read failure `failed`. The
   * release of an abandoned read is the controller abort itself --
   * real fetch ties the response body to the fetch signal -- so the
   * caller observes an early `timeout`/`aborted` result while the
   * platform tears the connection down. */
  readBody(): Promise<BodyReadResult>
  /** Cancels the response body of a response the caller decided not
   * to consume (a retryable status being retried, a discarded 401):
   * the connection is released deterministically instead of staying
   * held by an unread response until garbage collection. Resolves
   * when the body is gone; a body already errored or absent is a
   * no-op. */
  cancelBody(): Promise<void>
  /** Suspends the attempt's timeout timer across a gap that is not
   * part of this HTTP exchange (the silent-401-refresh round), and
   * re-arms it for the budget that was left. The caller-abort wiring
   * is untouched by either. Both are no-ops when no timeout is
   * configured or the timer is not paused. */
  pauseTimeout(): void
  resumeTimeout(): void
  /** Ends the attempt's timeout/abort wiring. Idempotent; safe to
   * call after the body settled by any path. */
  dispose(): void
}

/** How a body read inside an attempt's abort scope settled. */
type BodyReadResult =
  | { kind: 'text'; text: string }
  | { kind: 'timeout'; cause: unknown }
  | { kind: 'aborted' }
  | { kind: 'failed'; cause: unknown }

/** The request never reached a usable response. */
interface FailureOutcome {
  kind: 'network' | 'timeout'
  cause: unknown
}

type AttemptOutcome = HttpOutcome | FailureOutcome

/** A valid envelope, structurally checked. `code` is the one required
 * wire field; `traceId` is optional (undefined when the backend does
 * not send one -- the shape every module's handlers actually write is
 * `{code, params}`), as are `message`, `params` and `details`. */
interface Envelope {
  code: string
  traceId: string | undefined
  message: string | undefined
  params: Record<string, unknown> | undefined
  details: FieldError[] | undefined
}

/**
 * Parses a response body into the ApiError envelope, or undefined when
 * the body is empty, not JSON, not an object, lacks the required
 * `code` string, or carries a code in the reserved `client.` namespace
 * -- those stay bare and get a client.http.* code. The namespace rule
 * is what makes the client.* vocabulary's "reserved" claim a mechanism
 * instead of documentation: server codes are module-scoped (errors.ts's
 * header), and `client` is no module's domain, so a `client.*` code
 * inside an envelope is a misbehaving backend or an intermediary
 * borrowing the vocabulary -- an HTTP response can only exist with a
 * real status, so an accepted `client.network`/`client.timeout`
 * envelope would masquerade as this client's own transport diagnosis
 * (status 0, the one status no server can answer with) and mislead the
 * surfaces that render those codes. All other fields are optional: a
 * `{code, params}` body is a valid envelope even though it carries no
 * traceId.
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
  if (typeof candidate.code !== 'string' || candidate.code.startsWith('client.')) {
    return undefined
  }
  const envelope: Envelope = {
    code: candidate.code,
    traceId: undefined,
    message: undefined,
    params: undefined,
    details: undefined,
  }
  if (typeof candidate.traceId === 'string') {
    envelope.traceId = candidate.traceId
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
    const params =
      typeof candidate.params === 'object' &&
      candidate.params !== null &&
      !Array.isArray(candidate.params)
        ? (candidate.params as Record<string, unknown>)
        : undefined
    const fieldError: FieldError = {
      field: candidate.field,
      code: candidate.code,
      ...(params === undefined ? {} : { params }),
    }
    fieldErrors.push(fieldError)
  }
  return fieldErrors
}

/** Serializes a JSON body option into wire text. A body that does not
 * serialize to a string refuses with a TypeError -- symmetric with the
 * circular-structure case -- instead of letting the request go out
 * silently bodyless: JSON.stringify yields undefined (the value) for a
 * top-level function, symbol, or a toJSON() that returns undefined,
 * and none of those is a sendable JSON body. (An explicit `undefined`
 * body never reaches this function: the caller's `body !== undefined`
 * guard is the documented "no body" shape.) */
function serializeBody(body: unknown): string {
  const serialized = JSON.stringify(body)
  if (serialized === undefined) {
    throw new TypeError(
      'The request body does not serialize to a JSON string (a top-level undefined, function, or symbol).',
    )
  }
  return serialized
}

/** Builds the wire URL: baseUrl + path, query parameters appended. An
 * array value becomes one repeated parameter per element (URLSearchParams
 * append semantics -- the form/explode convention), so a multi-valued
 * filter survives as `?tag=a&tag=b` instead of collapsing into a
 * comma-joined `?tag=a,b` the backend would read as one literal value.
 * A path that already carries its own query string cannot take an
 * appended `?...` -- the result would be a malformed double-? URL -- so
 * combining one with a query option is refused as a programmer error:
 * request paths are spec paths, and every parameter belongs in the
 * query option. */
function buildUrl(
  baseUrl: string,
  path: string,
  query: RequestOptions['query'],
): string {
  const url = `${baseUrl}${path}`
  if (query === undefined) {
    return url
  }
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined || value === null) {
      continue
    }
    if (Array.isArray(value)) {
      for (const element of value) {
        if (element === undefined || element === null) {
          continue
        }
        params.append(key, String(element))
      }
      continue
    }
    params.append(key, String(value))
  }
  const encoded = params.toString()
  if (encoded === '') {
    return url
  }
  if (path.includes('?')) {
    throw programmerError(
      `request path must not carry a query string (${JSON.stringify(path)}): ` +
        'parameters appended through the query option would produce a ' +
        'malformed double-? URL; move those parameters into the query option.',
    )
  }
  return `${url}?${encoded}`
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
 * failure's body, within the attempt's abort scope; undefined when the
 * body is empty, unreadable, or stalled past the attempt's own
 * timeout. A caller abort during the read rejects the raw AbortError
 * -- cancellation wins over an envelope read. A response body is
 * single-read, so callers that need the envelope twice (a report and
 * the ApiError) read once and reuse it. */
async function readEnvelope(
  outcome: HttpOutcome,
): Promise<Envelope | undefined> {
  const read = await outcome.readBody()
  if (read.kind === 'text') {
    return parseEnvelope(read.text)
  }
  if (read.kind === 'aborted') {
    throw new DOMException('The operation was aborted.', 'AbortError')
  }
  // 'timeout' and 'failed': an unreadable error body has always meant
  // an envelope-less error -- the failure status still dominates.
  return undefined
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

/** The final ApiError for a transport-class failure. */
function failureError(outcome: FailureOutcome, attempts: number): ApiError {
  return new ApiError({
    status: 0,
    code: outcome.kind === 'timeout' ? ERROR_CODE_TIMEOUT : ERROR_CODE_NETWORK,
    attempts,
    cause: outcome.cause,
  })
}

/** Sleeps `ms` milliseconds -- or rejects with the raw AbortError the
 * moment `signal` aborts, so a cancelled request never sits out its
 * full backoff: the retry is already moot, and the caller's
 * cancellation should surface as promptly as it would mid-fetch. A
 * signal that is already aborted rejects without arming a timer. */
function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  if (signal?.aborted === true) {
    return Promise.reject(
      new DOMException('The operation was aborted.', 'AbortError'),
    )
  }
  return new Promise<void>((resolve, reject) => {
    const onAbort = (): void => {
      clearTimeout(timer)
      signal?.removeEventListener('abort', onAbort)
      reject(new DOMException('The operation was aborted.', 'AbortError'))
    }
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort)
      resolve()
    }, ms)
    signal?.addEventListener('abort', onAbort, { once: true })
  })
}

/** Awaits `promise` -- or rejects with the raw AbortError the moment
 * `signal` aborts, the same race `sleep` runs for a timed wait, for a
 * wait that owns no timer of its own (the silent-401-refresh round):
 * a cancelled caller must not stay stuck on a promise the caller's
 * abort cannot otherwise reach. A signal that is already aborted
 * rejects without attaching anything. The awaited promise itself is
 * never cancelled: when the abort wins, its eventual settlement is
 * simply discarded. */
function raceWithAbort<T>(
  promise: Promise<T>,
  signal?: AbortSignal,
): Promise<T> {
  if (signal?.aborted === true) {
    return Promise.reject(
      new DOMException('The operation was aborted.', 'AbortError'),
    )
  }
  return new Promise<T>((resolve, reject) => {
    const onAbort = (): void => {
      signal?.removeEventListener('abort', onAbort)
      reject(new DOMException('The operation was aborted.', 'AbortError'))
    }
    promise.then(
      (value) => {
        signal?.removeEventListener('abort', onAbort)
        resolve(value)
      },
      (cause: unknown) => {
        signal?.removeEventListener('abort', onAbort)
        reject(cause)
      },
    )
    signal?.addEventListener('abort', onAbort, { once: true })
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
  // CodeQL's js/polynomial-redos alert on this line is a false
  // positive. /\/+$/ is a single anchored character-class repetition
  // (one quantifier, no nesting, no alternation) matching a run of
  // trailing slashes at the end of the string -- there is exactly one
  // way to decompose a match, so this runs in O(n) even under a naive
  // backtracking engine; it has none of the nested/overlapping-quantifier
  // shape catastrophic backtracking needs. options.baseUrl reaches this
  // line directly from the caller with no intervening transformation,
  // and no other regex in this file is a plausible alternate target.
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
   * passthrough. Rejects raw only for caller cancellation. The
   * response body is part of the attempt, so for an http outcome the
   * timeout timer and the caller-abort forwarding stay armed past this
   * function -- the returned outcome's `readBody`/`cancelBody` run
   * inside that scope, and `dispose()` (idempotent) ends it. */
  const attemptOnce = async (
    method: HttpMethod,
    url: string,
    requestOptions: RequestOptions,
    bodyText: string | undefined,
    signal: AbortSignal | undefined,
  ): Promise<AttemptOutcome> => {
    const headers = new Headers(requestOptions.headers ?? {})
    // Both content defaults mirror the content-type guard below: set
    // only when the caller did not already supply the header, so a
    // caller-supplied accept is honoured rather than overwritten.
    if (!headers.has('accept')) {
      headers.set('accept', 'application/json')
    }
    if (bodyText !== undefined && !headers.has('content-type')) {
      headers.set('content-type', 'application/json')
    }
    // The token is read per attempt: a retry after a successful refresh
    // picks up the fresh token without extra plumbing. A request that
    // declares omitAccessToken skips the read entirely -- no store token
    // is attached no matter what the store holds (a session's own
    // refresh request authenticates with the refresh token in its body),
    // and its 401 therefore counts as a credential-less one. The
    // request's own headers are untouched by the omission: a
    // caller-supplied authorization header is the caller's, not the
    // store's, and travels as given.
    const omitToken = requestOptions.omitAccessToken === true
    let attachedToken = false
    if (!omitToken) {
      const token = tokenStore?.get() ?? null
      attachedToken = token !== null && token !== ''
      if (attachedToken) {
        headers.set('authorization', `Bearer ${token}`)
      }
    }

    const controller = new AbortController()
    let timedOut = false
    let disposed = false
    let timer: ReturnType<typeof setTimeout> | undefined
    let timeoutDeadline = 0
    let pausedRemaining: number | undefined
    let notifyAborted: (() => void) | undefined
    // Settles the moment this attempt is aborted while its body is
    // still being read -- by its own timeout or the caller's signal.
    // Real fetch ties the body to the controller's signal and kills
    // the read itself; this trigger covers environments that do not,
    // so a stalled body never outlives the abort either way.
    const abortTrigger = new Promise<void>((resolve) => {
      notifyAborted = resolve
    })
    const fireTimeout = (): void => {
      timedOut = true
      controller.abort()
      notifyAborted?.()
    }
    const armTimeout = (delay: number): void => {
      // Deadline bookkeeping on the clock the timers run on (the fake
      // timers tests use fake Date too), so a suspended timer can be
      // re-armed for exactly the budget that was left.
      timeoutDeadline = Date.now() + delay
      timer = setTimeout(fireTimeout, delay)
    }
    if (timeoutMs !== undefined) {
      armTimeout(timeoutMs)
    }
    // The timeout bounds one HTTP exchange, never the gaps around it:
    // pauseTimeout suspends the attempt timer (the caller-abort
    // forwarding stays live -- cancellation wins over everything), and
    // resumeTimeout re-arms it for exactly the budget that was left at
    // the pause, so time spent in a paused gap never counts against
    // the exchange. The silent-401-refresh round is the one paused
    // gap -- it is a separate exchange of its own (a host's refresh
    // request travels through this same client and carries its own
    // timeout), so a refresh that outlives timeoutMs must not fire
    // this attempt's timeout into the envelope read that follows a
    // failed refresh, degrading the real 401 envelope to a synthetic
    // client.http.401.
    const pauseTimeout = (): void => {
      if (timer === undefined) {
        return
      }
      clearTimeout(timer)
      timer = undefined
      pausedRemaining = Math.max(0, timeoutDeadline - Date.now())
    }
    const resumeTimeout = (): void => {
      if (pausedRemaining === undefined) {
        return
      }
      const delay = pausedRemaining
      pausedRemaining = undefined
      if (delay <= 0) {
        // A deadline that genuinely passed while paused fires on
        // resume: the exchange's budget is spent. Unreachable for a
        // delivered outcome -- the pause runs in the same microtask
        // chain as the headers' arrival, before any timer macrotask
        // can fire -- guarded for the bookkeeping's integrity.
        fireTimeout()
        return
      }
      armTimeout(delay)
    }
    const forwardAbort = (): void => {
      controller.abort()
      notifyAborted?.()
    }
    if (signal !== undefined) {
      signal.addEventListener('abort', forwardAbort, { once: true })
    }
    const dispose = (): void => {
      if (disposed) {
        return
      }
      disposed = true
      if (timer !== undefined) {
        clearTimeout(timer)
      }
      if (signal !== undefined) {
        signal.removeEventListener('abort', forwardAbort)
      }
    }
    const cancelBody = async (response: Response): Promise<void> => {
      try {
        await response.body?.cancel()
      } catch {
        // The body may already be gone (errored by an abort, a
        // null-body response): nothing left to release.
      }
    }

    try {
      const response = await fetchFn(url, {
        method,
        headers,
        body: bodyText,
        signal: controller.signal,
      })
      const outcome: HttpOutcome = {
        kind: 'http',
        status: response.status,
        response,
        attachedToken,
        readBody: async (): Promise<BodyReadResult> => {
          const reading = response.text()
          const result = await Promise.race<BodyReadResult>([
            reading.then(
              (text): BodyReadResult => ({ kind: 'text', text }),
              (cause: unknown): BodyReadResult => {
                // The read itself rejected. Real fetch rejects a body
                // read when the controller's signal aborts, so
                // classify by what aborted -- a platform-killed read
                // carries the same meaning as the trigger below.
                if (timedOut) {
                  return {
                    kind: 'timeout',
                    cause: new DOMException(
                      'The operation was aborted.',
                      'AbortError',
                    ),
                  }
                }
                if (signal?.aborted === true) {
                  return { kind: 'aborted' }
                }
                return { kind: 'failed', cause }
              },
            ),
            abortTrigger.then((): BodyReadResult =>
              timedOut
                ? {
                    kind: 'timeout',
                    cause: new DOMException(
                      'The operation was aborted.',
                      'AbortError',
                    ),
                  }
                : { kind: 'aborted' },
            ),
          ])
          // A read abandoned mid-stream ('timeout'/'aborted') releases
          // nothing further here by hand: response.text() holds the
          // body's lock, so a direct cancel would fail -- the release
          // is the controller abort itself, which conforming platforms
          // (real fetch) tie to the response body. The pending read is
          // simply dropped.
          return result
        },
        cancelBody: () => cancelBody(response),
        pauseTimeout,
        resumeTimeout,
        dispose,
      }
      return outcome
    } catch (error) {
      // The fetch itself failed: no body exists, so the wiring ends
      // here.
      dispose()
      if (timedOut) {
        return { kind: 'timeout', cause: error }
      }
      if (signal?.aborted === true) {
        // Caller cancellation: surface the raw AbortError, standard
        // query-layer semantics -- never wrapped, never retried.
        throw error
      }
      return { kind: 'network', cause: error }
    }
  }

  // Single-flight refresh: at most one refresh in flight across all
  // concurrent requests. A completed refresh (success or failure) is
  // not memoized -- the next 401 starts a fresh attempt, so a session
  // restored by another tab can be picked up.
  const refreshHook = options.refreshAccessToken
  let refreshInFlight: Promise<boolean> | null = null
  // True only while the refresh hook's own synchronous invocation is
  // on the stack -- the instant the refresh operation issues its
  // request through this same client (a host hook calls it straight
  // away, through however many synchronous wrappers). A request born
  // in that instant is the round's own request, and the 401 guard in
  // the request loop refuses it the refresh path exactly like the
  // bearer-only rule refuses a credential-less 401 -- a host that
  // forgot the `omitAccessToken` declaration on its refresh request
  // gets a finite failure, never a self-await. The marker is
  // deliberately dropped once the hook has returned its promise: a
  // request born later in the round's pending window is an
  // independent request and must still be able to join the flight.
  let inRefreshHookCall = false
  const refreshOnce =
    refreshHook === undefined
      ? undefined
      : (): Promise<boolean> => {
          if (refreshInFlight === null) {
            refreshInFlight = (async (): Promise<boolean> => {
              try {
                inRefreshHookCall = true
                let hookOutcome: Promise<boolean>
                try {
                  hookOutcome = (async (): Promise<boolean> => {
                    try {
                      return (await refreshHook()) === true
                    } catch {
                      // A throwing refresh hook is a failed refresh:
                      // the session is still gone and the request must
                      // not retry.
                      return false
                    }
                  })()
                } finally {
                  inRefreshHookCall = false
                }
                // The flight is shared by every request whose 401
                // joins it -- the initiator and each waiter alike --
                // and no signal races the flight itself: it settles
                // when the hook settles, and its outcome is the hook's
                // real one. Every joining request races the flight
                // against its OWN signal at its own call site (the
                // request loop below), so an abort detaches only the
                // request that aborted and every waiter with a live
                // signal is still bounded by that signal. Binding the
                // round to the request that started it would be a
                // conflation, not a trade-off: an initiator that gave
                // up mid round must not settle the round as a failed
                // refresh for the requests already joined on it -- the
                // hook may succeed and write a fresh token into the
                // store while the waiters were told it failed (an
                // auth:true answer -- the host's basis for judging the
                // session ended -- for a session the refresh just
                // saved). The slot is freed the moment the hook
                // settles; a 401 that arrives before then joins the
                // in-flight round rather than firing a sibling
                // invocation -- a second simultaneous presentation of
                // the same refresh token, which the authn server reads
                // as theft.
                return await hookOutcome
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
    // A request born inside the refresh hook's own synchronous
    // invocation (see refreshOnce's marker) is the refresh round's own
    // request: the refresh operation travelling through this client
    // without its credential-less declaration. Captured at entry for
    // the 401 guard below -- the live marker is gone long before this
    // request's own 401 can arrive.
    const bornInsideRefresh = inRefreshHookCall
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

    // The body does not change between attempts: serialize once, up
    // front, so a body that cannot be JSON-serialized (a circular
    // structure, a top-level function or symbol, a toJSON() that
    // yields nothing) fails as a coded client.protocol ApiError -- a
    // request the client can never send -- instead of surfacing a
    // bare TypeError out of the retry machinery or going out
    // silently bodyless.
    let bodyText: string | undefined
    if (requestOptions.body !== undefined) {
      try {
        bodyText = serializeBody(requestOptions.body)
      } catch (cause) {
        throw new ApiError({
          status: 0,
          code: ERROR_CODE_PROTOCOL,
          attempts: 0,
          message: 'The request body could not be serialized as JSON.',
          cause,
        })
      }
    }

    let attempts = 0
    // Transient retries performed so far: retryPolicy.maxAttempts
    // bounds these (maxAttempts - 1 retries from a fresh request). The
    // silent-401-refresh round -- a refused send plus its one
    // post-refresh retry -- is not a transient retry and never counts
    // toward the budget: it is bounded by its own exactly-once rule.
    let transientRetries = 0
    let refreshed = false
    for (;;) {
      attempts += 1
      const outcome = await attemptOnce(
        method,
        url,
        requestOptions,
        bodyText,
        signal,
      )
      if (outcome.kind === 'http') {
        try {
          // The caller may have aborted while the request was in
          // flight: cancellation wins over any outcome -- release the
          // response the caller no longer wants, then nothing below
          // may retry or deliver on behalf of a caller that gave up.
          if (signal?.aborted === true) {
            await outcome.cancelBody()
            throwIfAborted(signal)
          }

          if (isSuccessStatus(outcome.status)) {
            const read = await outcome.readBody()
            if (read.kind === 'aborted') {
              // The abort trigger only fires from the caller's signal;
              // cancellation surfaces raw, never as a delivery.
              throwIfAborted(signal)
              throw new DOMException('The operation was aborted.', 'AbortError')
            }
            if (read.kind !== 'text') {
              // The headers said 2xx but the body never settled: a
              // timeout or a dead read is a transport-class failure of
              // this attempt, retried like any other.
              const kind = read.kind === 'timeout' ? 'timeout' : 'network'
              if (idempotent && transientRetries < retryPolicy.maxAttempts - 1) {
                transientRetries += 1
                // The signal-aware sleep rejects the raw AbortError
                // the moment the caller cancels, so a cancelled
                // request never sits out its backoff and no retry
                // fires for it; the check after the sleep guards the
                // instant between it settling and this continuation.
                await sleep(retryDelayMs(transientRetries - 1, retryPolicy), signal)
                throwIfAborted(signal)
                continue
              }
              throw failureError({ kind, cause: read.cause }, attempts)
            }
            const body = read.text
            // The body arrived; an abort during the read cancels the
            // delivery instead of resolving a 2xx for a cancelled
            // caller.
            throwIfAborted(signal)
            if (body.trim() === '') {
              if (requestOptions.requireJsonBody === true) {
                // A declared need for a JSON document turns the
                // client's own 204-style empty-success shape into a
                // protocol violation -- refused here, inside the
                // exchange, so the error carries the exchange's real
                // status and attempt count (a wrapper around the
                // RequestFn could only synthesize both).
                throw new ApiError({
                  status: outcome.status,
                  code: ERROR_CODE_PROTOCOL,
                  attempts,
                  message:
                    'The endpoint answered an empty 2xx body; expected a JSON document.',
                })
              }
              // 204-style: no content is a valid, empty success. Blank
              // counts as empty (some servers pad the bodyless response
              // with whitespace), symmetric with parseEnvelope treating
              // a whitespace-only error body as envelope-less.
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
              // The branch refuses JSON that is not an object or an
              // array (a JSON string or number parses fine but is not a
              // usable response document) -- the cause names exactly
              // what is refused.
              throw new ApiError({
                status: outcome.status,
                code: ERROR_CODE_PROTOCOL,
                attempts,
                cause: new SyntaxError('2xx body is not a JSON object or array'),
              })
            }
            return data as T
          }

          if (outcome.status === 401) {
            // Bearer-only: a credential-less 401 means authentication
            // is required, which refreshing cannot provide -- and a
            // session's own refresh request (sent credential-less)
            // must never re-enter the refresh path or it awaits
            // itself. The born-inside-refresh marker enforces the same
            // rule at runtime: a refresh request whose host forgot the
            // credential-less declaration presents the stale token, is
            // refused with a token-bearing 401, and would otherwise
            // join -- and await -- the very flight it is part of.
            if (
              !refreshed &&
              refreshOnce !== undefined &&
              outcome.attachedToken &&
              !bornInsideRefresh
            ) {
              refreshed = true
              // The refresh round is not part of this attempt's HTTP
              // exchange: suspend the attempt timer across it so a
              // refresh that outlives timeoutMs cannot fire the
              // timeout into the envelope read below and degrade the
              // real 401 envelope to a synthetic client.http.401 (its
              // code and traceId lost). The caller-abort forwarding
              // stays live across the pause, but with the attempt's
              // fetch already settled and its timer suspended there is
              // nothing left for it to abort: an abort landing during
              // the round has to race the refresh wait itself. The
              // await below therefore runs the same signal race the
              // backoff sleeps run -- this request's cancellation
              // rejects it with the raw AbortError on the next
              // microtask, never wrapped, never retried, detaching
              // only this request: the flight itself races no signal,
              // so the requests already joined on it still get the
              // hook's real outcome when it settles (see refreshOnce).
              outcome.pauseTimeout()
              const refreshedOk = await raceWithAbort(refreshOnce(), signal)
              // The caller may have aborted while the refresh was in
              // flight: cancellation wins -- never send the
              // post-refresh retry, never deliver an auth error
              // either. (The race above already rejects for the
              // cancelling caller; this check guards the instant
              // between the flight settling and this continuation.)
              throwIfAborted(signal)
              if (refreshedOk) {
                // Retry once with whatever token the store holds now
                // (the token is re-read at send time). Orthogonal to
                // the retry budget and to method idempotency. The
                // refused 401's body is not wanted: release it
                // deterministically instead of leaving the connection
                // held by an unread response.
                await outcome.cancelBody()
                continue
              }
              // Refresh failed: read the 401 body once and reuse it
              // for the report and the error, so the warning carries
              // the envelope's code (and trace id, when the backend
              // sent one) -- correlating it to server logs -- instead
              // of firing blind. The read stays inside the attempt's
              // abort scope: the timer is re-armed for the budget that
              // was left at the pause, so a stalled body still cannot
              // outlive the attempt's own timeout (the error degrades
              // to an envelope-less one), and a caller abort mid-read
              // surfaces raw.
              outcome.resumeTimeout()
              const envelope = await readEnvelope(outcome)
              reporter.warn('access token refresh failed', {
                status: outcome.status,
                ...(envelope === undefined
                  ? {}
                  : envelope.traceId === undefined
                    ? { code: envelope.code }
                    : {
                        code: envelope.code,
                        trace_id: envelope.traceId,
                      }),
              })
              throw envelopeError(outcome, envelope, attempts)
            }
            // No hook, no bearer token, the retried request was
            // refused again, or a request born inside the refresh
            // round itself (see the marker guard above): the session
            // is over, surface the auth error.
            throw envelopeError(outcome, await readEnvelope(outcome), attempts)
          }

          if (
            idempotent &&
            transientRetries < retryPolicy.maxAttempts - 1 &&
            retryableOutcome(outcome)
          ) {
            // A retryable status (429/502/503/504) whose body is not
            // wanted: release the response body deterministically
            // before the backoff, instead of leaving the connection
            // held by an unread response.
            await outcome.cancelBody()
            const delay = retryDelayFor(outcome, transientRetries, retryPolicy)
            transientRetries += 1
            // The signal-aware sleep rejects the raw AbortError the
            // moment the caller cancels, so a cancelled request never
            // sits out its backoff and no retry fires for it; the
            // check after the sleep guards the instant between it
            // settling and this continuation.
            await sleep(delay, signal)
            throwIfAborted(signal)
            continue
          }

          throw envelopeError(outcome, await readEnvelope(outcome), attempts)
        } finally {
          // The body settled (read, released, or abandoned): end this
          // attempt's timeout and abort wiring. Idempotent, so a
          // branch that already ended it early stays safe.
          outcome.dispose()
        }
      }

      // Transport-class failure (network or timeout): no response
      // body exists, so there is nothing to release. The caller may
      // have aborted after the fetch rejected: cancellation wins.
      throwIfAborted(signal)
      if (idempotent && transientRetries < retryPolicy.maxAttempts - 1) {
        const delay = retryDelayMs(transientRetries, retryPolicy)
        transientRetries += 1
        // The signal-aware sleep rejects the raw AbortError the
        // moment the caller cancels, so a cancelled request never
        // sits out its backoff and no retry fires for it; the check
        // after the sleep guards the instant between it settling and
        // this continuation.
        await sleep(delay, signal)
        throwIfAborted(signal)
        continue
      }
      throw failureError(outcome, attempts)
    }
  }

  return request
}

function isSuccessStatus(status: number): boolean {
  return status >= 200 && status <= 299
}
