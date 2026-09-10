/**
 * time-format.ts behaviour: the three-tier display-timezone chain
 * (stored preference -> device -> UTC) and its Intl guards. The guard
 * cases are the point: a stored value the engine rejects must fall
 * through the chain, never throw into a render, and an environment whose
 * Intl cannot answer must degrade to the terminal tier.
 */

import { describe, expect, it, vi } from 'vitest'
import {
  deviceTimeZone,
  FALLBACK_TIME_ZONE,
  isSupportedTimeZone,
  resolveTimeZone,
} from './time-format.js'

/** Runs fn with Intl.DateTimeFormat stubbed to throw on every
 * construction (the environment-cannot-answer shape), restoring after. */
function withBrokenDateTimeFormat(
  fn: () => void,
  { onlyWithoutArgs = false }: { onlyWithoutArgs?: boolean } = {},
): void {
  const original = Intl.DateTimeFormat
  const broken = function (
    this: unknown,
    ...args: unknown[]
  ): Intl.DateTimeFormat {
    if (!onlyWithoutArgs || args.length === 0) {
      throw new RangeError('test: Intl.DateTimeFormat unavailable')
    }
    return new original(...(args as ConstructorParameters<typeof Intl.DateTimeFormat>))
  } as unknown as typeof Intl.DateTimeFormat
  const spy = vi.spyOn(Intl, 'DateTimeFormat').mockImplementation(broken)
  try {
    fn()
  } finally {
    spy.mockRestore()
  }
}

describe('isSupportedTimeZone', () => {
  it('accepts a real IANA zone and rejects an unknown one', () => {
    expect(isSupportedTimeZone('Asia/Shanghai')).toBe(true)
    expect(isSupportedTimeZone('UTC')).toBe(true)
    expect(isSupportedTimeZone('Mars/Olympus')).toBe(false)
    expect(isSupportedTimeZone('')).toBe(false)
  })

  it('answers false rather than throwing when the engine refuses to construct', () => {
    withBrokenDateTimeFormat(() => {
      expect(isSupportedTimeZone('Asia/Shanghai')).toBe(false)
    })
  })
})

describe('deviceTimeZone', () => {
  it('answers the device zone when the environment can resolve one', () => {
    const zone = deviceTimeZone()
    expect(typeof zone).toBe('string')
    expect(zone).not.toBe('')
  })

  it('answers null rather than throwing when the environment cannot', () => {
    withBrokenDateTimeFormat(
      () => {
        expect(deviceTimeZone()).toBeNull()
      },
      { onlyWithoutArgs: true },
    )
  })
})

describe('resolveTimeZone', () => {
  const device = deviceTimeZone()

  it('prefers a stored value the engine accepts', () => {
    expect(resolveTimeZone('Asia/Tokyo')).toBe('Asia/Tokyo')
  })

  it('trims a stored value before accepting it', () => {
    expect(resolveTimeZone('  Asia/Tokyo ')).toBe('Asia/Tokyo')
  })

  it('falls through an empty or absent stored value to the device zone', () => {
    expect(resolveTimeZone('')).toBe(device)
    expect(resolveTimeZone('   ')).toBe(device)
    expect(resolveTimeZone(null)).toBe(device)
    expect(resolveTimeZone(undefined)).toBe(device)
  })

  it('skips a stored value the engine rejects instead of crashing the render', () => {
    expect(resolveTimeZone('Mars/Olympus')).toBe(device)
  })

  it('lands on UTC when neither the stored value nor the device resolves', () => {
    withBrokenDateTimeFormat(() => {
      expect(resolveTimeZone('Mars/Olympus')).toBe(FALLBACK_TIME_ZONE)
      expect(resolveTimeZone('')).toBe(FALLBACK_TIME_ZONE)
      expect(resolveTimeZone(null)).toBe(FALLBACK_TIME_ZONE)
    })
  })

  it('answers a zone Intl.DateTimeFormat accepts, for every outcome', () => {
    for (const zone of [
      resolveTimeZone('Asia/Tokyo'),
      resolveTimeZone(''),
      resolveTimeZone('Mars/Olympus'),
    ]) {
      expect(() => new Intl.DateTimeFormat('en-US', { timeZone: zone })).not.toThrow()
    }
  })
})
