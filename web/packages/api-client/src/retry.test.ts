/**
 * Contract tests for the retry backoff maths: exponential full-jitter
 * delay formula (injected random source), the Retry-After header parser
 * (delta-seconds and HTTP-date, explicit now), and the frozen default
 * policy. These are the pure functions the client's transient-retry
 * timing is built on, so every branch is pinned here rather than
 * indirectly through client tests.
 */

import { describe, expect, it, vi } from 'vitest'
import {
  DEFAULT_RETRY_POLICY,
  retryAfterDelayMs,
  retryDelayMs,
  type RetryPolicy,
} from './index'

/** A random source that always draws the same value, for determinism. */
const almostOne: () => number = () => 0.999

describe('DEFAULT_RETRY_POLICY', () => {
  it('ships the documented budget and timing', () => {
    expect(DEFAULT_RETRY_POLICY).toEqual({
      maxAttempts: 3,
      initialDelayMs: 200,
      maxDelayMs: 4000,
    })
  })

  it('is frozen, so accidental mutation fails fast', () => {
    expect(Object.isFrozen(DEFAULT_RETRY_POLICY)).toBe(true)
  })
})

describe('retryDelayMs', () => {
  const policy: RetryPolicy = {
    maxAttempts: 3,
    initialDelayMs: 100,
    maxDelayMs: 1000,
  }

  it('draws uniformly from [0, cap): zero for a zero random value', () => {
    const zero: () => number = () => 0
    expect(retryDelayMs(0, policy, zero)).toBe(0)
    expect(retryDelayMs(3, policy, zero)).toBe(0)
  })

  it('draws near the cap for a random value near one', () => {
    // Math.floor(0.999 * cap) === cap - 1 for integer caps.
    expect(retryDelayMs(0, policy, almostOne)).toBe(99)
  })

  it('doubles the delay ceiling per retry, capped by maxDelayMs', () => {
    // Caps: 100, 200, 400, 800, then the 1000ms ceiling holds.
    const caps = [99, 199, 399, 799, 999, 999]
    for (let attempt = 0; attempt < caps.length; attempt += 1) {
      expect(retryDelayMs(attempt, policy, almostOne)).toBe(caps[attempt])
    }
  })

  it('clamps negative and fractional attempts to the retry index', () => {
    const policyWithCeiling: RetryPolicy = {
      maxAttempts: 3,
      initialDelayMs: 100,
      maxDelayMs: 1000,
    }
    // -2 floors to 0: first-retry range [0, 100).
    expect(retryDelayMs(-2, policyWithCeiling, almostOne)).toBe(99)
    // 2.7 floors to 2: third-retry range [0, 400).
    expect(retryDelayMs(2.7, policyWithCeiling, almostOne)).toBe(399)
  })

  it('honours a zero delay ceiling (retries as fast as timers allow)', () => {
    const immediate: RetryPolicy = {
      maxAttempts: 2,
      initialDelayMs: 0,
      maxDelayMs: 0,
    }
    expect(retryDelayMs(0, immediate, almostOne)).toBe(0)
    expect(retryDelayMs(5, immediate, almostOne)).toBe(0)
  })

  it('defaults the random source to Math.random', () => {
    const spy = vi.spyOn(Math, 'random').mockReturnValue(0.5)
    try {
      // 0.5 * 200 -> 100, deterministic only via the stub.
      expect(retryDelayMs(0, DEFAULT_RETRY_POLICY)).toBe(100)
    } finally {
      spy.mockRestore()
    }
  })
})

describe('retryAfterDelayMs', () => {
  const NOW = Date.parse('Wed, 21 Oct 2015 07:27:00 GMT')

  it('returns null when no header is present', () => {
    expect(retryAfterDelayMs(null, NOW)).toBeNull()
    expect(retryAfterDelayMs(undefined, NOW)).toBeNull()
  })

  it('returns null for empty or whitespace-only headers', () => {
    expect(retryAfterDelayMs('', NOW)).toBeNull()
    expect(retryAfterDelayMs('   ', NOW)).toBeNull()
  })

  it('parses delta-seconds as milliseconds', () => {
    expect(retryAfterDelayMs('120', NOW)).toBe(120000)
    expect(retryAfterDelayMs('0', NOW)).toBe(0)
    // Surrounding whitespace is tolerated per RFC 9110 field parsing.
    expect(retryAfterDelayMs(' 45 ', NOW)).toBe(45000)
  })

  it('parses an HTTP-date header as the seconds until that instant', () => {
    expect(retryAfterDelayMs('Wed, 21 Oct 2015 07:28:00 GMT', NOW)).toBe(60000)
  })

  it('clamps an HTTP-date already in the past to zero', () => {
    expect(retryAfterDelayMs('Wed, 21 Oct 2015 07:26:00 GMT', NOW)).toBe(0)
  })

  it('returns null for anything unparseable', () => {
    // The fixture strings must be NaN for Date.parse in every engine --
    // V8 legacy-parses surprising inputs (partial dates, '12.5'), so
    // keep them clearly date-free words.
    expect(retryAfterDelayMs('later', NOW)).toBeNull()
    expect(retryAfterDelayMs('not a date', NOW)).toBeNull()
    expect(retryAfterDelayMs('garbage', NOW)).toBeNull()
  })
})
