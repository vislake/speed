/**
 * The ApiError envelope and the reserved client.* code vocabulary.
 *
 * Every speed API answers non-2xx responses with one shared envelope
 * (docs/internal/21-api-contract.md):
 *
 *   { code: string, traceId: string,
 *     params?: object, message?: string, details?: FieldError[] }
 *
 * `code` and `traceId` are required; `message` is an English log-triage
 * fallback that must never be rendered to users (frontends resolve
 * `code` through the i18n catalog instead). Responses that do not carry
 * a valid envelope are still errors, but they cannot be mapped onto
 * module error codes -- they get a synthesized code in the reserved
 * `client.` namespace instead, so callers can tell "the API answered a
 * real error" from "the API layer itself broke":
 *
 *   client.network   -- the request never completed (DNS, refused,
 *                       TLS, CORS, mid-body read failure...).
 *   client.timeout   -- the client's own timeout fired and aborted it.
 *   client.http.<n>  -- a non-2xx response without a parseable envelope
 *                       (non-JSON body, or JSON without code+traceId).
 *   client.protocol  -- a 2xx whose body is not a JSON object/array.
 *
 * Codes from a valid envelope always win over the client.* vocabulary:
 * a 401 carrying `authn.session_expired` surfaces as that code, with
 * `auth: true` marking it as a session/credential problem hosts treat
 * specially (silent refresh, forced sign-out).
 */

/** A single field-level validation failure listed in the envelope. */
export interface FieldError {
  /** The field the error attaches to (JSON path when nested). */
  readonly field: string
  /** Module-scoped machine code, e.g. billing.insufficient_credits. */
  readonly code: string
  /** Code interpolation parameters, when the message needs them. */
  readonly params?: Readonly<Record<string, unknown>>
}

/** Reserved code: the request never completed for transport reasons. */
export const ERROR_CODE_NETWORK = 'client.network' as const

/** Reserved code: the request exceeded ClientOptions.timeoutMs. */
export const ERROR_CODE_TIMEOUT = 'client.timeout' as const

/** Reserved code: a 2xx response that is not a JSON value. */
export const ERROR_CODE_PROTOCOL = 'client.protocol' as const

/** Reserved code for a bare non-2xx response (no valid envelope). */
export function httpErrorCode(status: number): string {
  return `client.http.${status}`
}

/** Everything the ApiError constructor needs. */
export interface ApiErrorInit {
  /** HTTP status of the response; 0 when no response arrived. */
  readonly status: number
  /** Module error code from the envelope, or a reserved client.* code. */
  readonly code: string
  /** Server trace id, for reconnecting user reports to server logs. */
  readonly traceId?: string
  /** Envelope message; overrides the synthesized English default. */
  readonly message?: string
  /** Envelope parameters, for hosts that interpolate their own text. */
  readonly params?: Readonly<Record<string, unknown>>
  /** Field-level validation failures, when the envelope carried them. */
  readonly details?: readonly FieldError[]
  /** HTTP attempts made for this request (1 = no retry happened). */
  readonly attempts: number
  /** The underlying error for network/timeout/protocol failures. */
  readonly cause?: unknown
}

/** The English diagnostic default for a code with no envelope message. */
function defaultMessage(code: string, status: number): string {
  if (code === ERROR_CODE_NETWORK) {
    return 'The request failed before a response arrived.'
  }
  if (code === ERROR_CODE_TIMEOUT) {
    return 'The request timed out before a response arrived.'
  }
  if (code === ERROR_CODE_PROTOCOL) {
    return 'The response did not satisfy the API JSON contract.'
  }
  if (code.startsWith('client.http.')) {
    return `The server answered HTTP ${status} without a valid ApiError envelope.`
  }
  return `The API rejected the request (${code}).`
}

/**
 * The normalized error every failed request rejects with. Thrown by the
 * request function for envelope errors, bare HTTP errors and transport
 * failures alike, so callers handle exactly one error type -- inspect
 * `code` for a module code (map it through i18n) and the reserved
 * `client.` codes for API-layer failures. `auth` is true exactly for
 * HTTP 401, whatever the envelope said; hosts that configured a
 * refreshAccessToken hook see it only when refresh failed or the
 * retried request was refused again.
 */
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly traceId: string | undefined
  readonly params: Readonly<Record<string, unknown>> | undefined
  readonly details: readonly FieldError[] | undefined
  readonly attempts: number
  readonly auth: boolean

  constructor(init: ApiErrorInit) {
    super(init.message ?? defaultMessage(init.code, init.status), {
      cause: init.cause,
    })
    this.name = 'ApiError'
    this.status = init.status
    this.code = init.code
    this.traceId = init.traceId
    this.params = init.params
    this.details = init.details
    this.attempts = init.attempts
    this.auth = init.status === 401
  }
}

/**
 * Type guard for ApiError. `instanceof` alone would miss errors thrown
 * by a second copy of the library (dev-time aliasing); the structural
 * check additionally accepts any error-shaped object carrying the four
 * fields hosts actually branch on, and is what a `catch (error)` site
 * should use before reading `.code` or `.traceId`.
 */
export function isApiError(value: unknown): value is ApiError {
  if (value instanceof ApiError) {
    return true
  }
  if (typeof value !== 'object' || value === null) {
    return false
  }
  const candidate = value as Partial<ApiError>
  return (
    typeof candidate.status === 'number' &&
    typeof candidate.code === 'string' &&
    typeof candidate.message === 'string' &&
    typeof candidate.auth === 'boolean'
  )
}
