/**
 * Retry policy and the pure backoff maths behind it.
 *
 * Transient retries are conservative by contract: only idempotent
 * methods (GET/HEAD/OPTIONS) are ever retried, and only for 429 (with
 * Retry-After honoured), 502/503/504, network failures and timeouts.
 * 401 handling is orthogonal and lives in client.ts. The policy shapes
 * the attempt budget and the delays, nothing else.
 */

/** Timing and budget of transient retries. */
export interface RetryPolicy {
  /** Total HTTP attempts before giving up; 1 disables transient retries. */
  readonly maxAttempts: number
  /**
   * Delay before the first retry. Doubles per retry, full-jittered
   * (uniform over [0, cap)) and never above maxDelayMs.
   */
  readonly initialDelayMs: number
  /** Absolute ceiling for any single retry delay. */
  readonly maxDelayMs: number
}

/**
 * The built-in budget: up to 3 HTTP attempts with 200ms doubling
 * jittered delays capped at 4s. Hosts override through
 * ClientOptions.retryPolicy -- notably `{ maxAttempts: 1, ... }` to
 * disable transient retries. Frozen so accidental mutation fails fast.
 */
export const DEFAULT_RETRY_POLICY: RetryPolicy = Object.freeze({
  maxAttempts: 3,
  initialDelayMs: 200,
  maxDelayMs: 4000,
})

/**
 * The full-jitter backoff delay before retry number `attempt` (0-based:
 * 0 = the first retry): uniform over [0, min(maxDelayMs,
 * initialDelayMs * 2**attempt)). Full jitter avoids the thundering herd
 * of synchronized retries while keeping the exponential ceiling. Pure:
 * the random source is injected so tests are deterministic; callers
 * that do not care let it default to Math.random.
 */
export function retryDelayMs(
  attempt: number,
  policy: RetryPolicy,
  random: () => number = Math.random,
): number {
  const retry = Math.max(0, Math.floor(attempt))
  const cap = Math.min(
    policy.maxDelayMs,
    policy.initialDelayMs * 2 ** retry,
  )
  return Math.floor(random() * cap)
}

/**
 * Parses a Retry-After header into a delay in milliseconds, or null
 * when the header is absent or unparseable. Accepts the two RFC 9110
 * forms: delta-seconds ("120") and HTTP-date ("Wed, 21 Oct 2015
 * 07:28:00 GMT"). The client honours the result for 429 and 503
 * responses, capped by the policy's maxDelayMs.
 */
export function retryAfterDelayMs(
  header: string | null | undefined,
  now: number = Date.now(),
): number | null {
  if (header === null || header === undefined) {
    return null
  }
  const trimmed = header.trim()
  if (trimmed === '') {
    return null
  }
  if (/^[0-9]+$/.test(trimmed)) {
    return Number(trimmed) * 1000
  }
  const at = Date.parse(trimmed)
  if (Number.isNaN(at)) {
    return null
  }
  return Math.max(0, at - now)
}
