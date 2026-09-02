/**
 * Contract tests for deepMerge, the token override mechanism.
 *
 * Each paragraph of the semantics documented on deepMerge is pinned here:
 * no input mutation, undefined overrides skipped, recursive object merging,
 * argument-order precedence, and the prototype-safety of hostile keys. The
 * "__proto__" case is constructed through JSON.parse so it really is an own
 * property, not a source-literal artifact.
 */

import { describe, expect, it } from 'vitest'
import { deepMerge } from './merge'
import { defaultTokens } from './defaultTokens'
import type { DeepPartial } from './types'

/** Recursively freeze an object so any mutation attempt throws (tests run under strict mode). */
function deepFreeze<T>(value: T): T {
  if (typeof value === 'object' && value !== null) {
    for (const child of Object.values(value as Record<string, unknown>)) {
      deepFreeze(child)
    }
    Object.freeze(value)
  }
  return value
}

/**
 * Escape hatch for exercising defensive runtime branches that the override
 * type deliberately makes unreachable (null and primitives cannot stand in
 * for an object-typed token field). The merge must still behave correctly
 * under untyped data such as JSON parsed at runtime.
 */
function uncheckedPartial<T extends object>(value: unknown): DeepPartial<T> {
  return value as DeepPartial<T>
}

describe('deepMerge', () => {
  it('returns a copy with every override applied at full depth', () => {
    const merged = deepMerge(
      { a: { b: { c: 1, d: 2 } }, e: 'x' },
      { a: { b: { c: 9 } } },
    )
    expect(merged).toEqual({ a: { b: { c: 9, d: 2 } }, e: 'x' })
  })

  it('applies overrides in argument order, later wins', () => {
    const merged = deepMerge(
      { a: 1, b: { c: 1 } },
      { a: 2, b: { c: 2 } },
      { a: 3, b: { c: 3 } },
    )
    expect(merged).toEqual({ a: 3, b: { c: 3 } })
  })

  it('merges a later deep partial onto an earlier override, not onto the base alone', () => {
    const merged = deepMerge(
      { a: { x: 1, y: 1 } },
      { a: { x: 2 } },
      { a: { y: 3 } },
    )
    expect(merged).toEqual({ a: { x: 2, y: 3 } })
  })

  it('skips undefined override values instead of blanking tokens', () => {
    const merged = deepMerge({ a: { b: 1 }, c: 2 }, { a: { b: undefined }, c: undefined })
    expect(merged).toEqual({ a: { b: 1 }, c: 2 })
  })

  it('replaces object fields wholesale when the override value is not a plain record', () => {
    // Typed path: arrays are leaves, so an array override replaces wholesale.
    expect(deepMerge({ a: [1, 2] }, { a: [3] })).toEqual({ a: [3] })
    // Defensive paths (only reachable through untyped data such as JSON):
    // null and primitives replace, they never merge.
    interface Sample {
      a: { b: number }
    }
    expect(deepMerge({ a: { b: 1 } }, uncheckedPartial<Sample>({ a: null }))).toEqual({
      a: null,
    })
    expect(deepMerge({ a: { b: 1 } }, uncheckedPartial<Sample>({ a: 0 }))).toEqual({ a: 0 })
    expect(deepMerge({ a: { b: 1 } }, uncheckedPartial<Sample>({ a: 'x' }))).toEqual({
      a: 'x',
    })
    expect(deepMerge({ a: { b: 1 } }, uncheckedPartial<Sample>({ a: false }))).toEqual({
      a: false,
    })
  })

  it('never mutates any input, even when inputs are deeply frozen', () => {
    const base = deepFreeze({ color: { main: '#111111', light: '#222222' }, ok: true })
    const override = deepFreeze({ color: { main: '#333333' } })
    const merged = deepMerge(base, override)
    expect(merged).toEqual({ color: { main: '#333333', light: '#222222' }, ok: true })
    expect(base).toEqual({ color: { main: '#111111', light: '#222222' }, ok: true })
  })

  it('shares untouched branches with the base by identity and rebuilds touched ones', () => {
    const merged = deepMerge(defaultTokens, { color: { divider: '#EEEEEE' } })
    expect(merged.typography).toBe(defaultTokens.typography)
    expect(merged.color).not.toBe(defaultTokens.color)
    expect(merged.color.semantic).toBe(defaultTokens.color.semantic)
    expect(merged.color.divider).toBe('#EEEEEE')
  })

  it('keeps base objects whose path no override enters untouched (no deep clone)', () => {
    const base = { a: { b: 1 }, deep: { x: { y: 1 } } }
    const merged = deepMerge(base, { a: { b: 2 } })
    expect(merged.deep).toBe(base.deep)
    expect(merged).toEqual({ a: { b: 2 }, deep: { x: { y: 1 } } })
  })

  it('stores a hostile "__proto__" override key as an own data property', () => {
    const hostile = JSON.parse('{"__proto__": {"polluted": true}, "a": {"b": 1}}') as {
      a: { b: number }
    }
    const merged = deepMerge({ a: { b: 0 } }, hostile as { a: { b: number } }) as Record<
      string,
      unknown
    >
    expect(Object.getPrototypeOf(merged)).toBe(Object.prototype)
    expect(Object.prototype.hasOwnProperty.call(merged, '__proto__')).toBe(true)
    expect(merged['__proto__']).toEqual({ polluted: true })
    expect((Object.prototype as { polluted?: boolean }).polluted).toBeUndefined()
    expect(merged.a).toEqual({ b: 1 })
  })

  it('never leaves a hostile "__proto__" key in the shared prototype, even when merging over it', () => {
    const first = JSON.parse('{"__proto__": {"a": 1}}') as Record<string, unknown>
    const second = JSON.parse('{"__proto__": {"b": 2}}') as Record<string, unknown>
    const merged = deepMerge(first, second)
    expect(Object.keys(merged)).toEqual(['__proto__'])
    expect(merged['__proto__']).toEqual({ a: 1, b: 2 })
    expect((Object.prototype as { a?: unknown; b?: unknown }).a).toBeUndefined()
    expect((Object.prototype as { a?: unknown; b?: unknown }).b).toBeUndefined()
  })
})
