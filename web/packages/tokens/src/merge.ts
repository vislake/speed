/**
 * The deep-merge helper for token trees.
 *
 * Home decision (why merge lives here, not in a factory package): the merge
 * is defined over SpeedTokens itself -- its type argument is DeepPartial of
 * the token tree, it never mutates, and it treats objects as the only
 * mergeable unit. A future theme factory consumes the *merged result* and
 * must not depend on a merging mechanism, so the merge ships with the tree
 * it merges.
 */

import type { DeepPartial } from './types'

/**
 * Merge `overrides` into a copy of `base`, recursively, and return the copy.
 *
 * Semantics, in contract form (each is pinned by a test):
 * - No argument is ever mutated. Copy-on-write: a branch that no override
 *   touches is shared with `base` by identity; every branch some override
 *   does touch is rebuilt.
 * - `undefined` values in overrides are skipped, so a partial override never
 *   blanks a token. Every other value (including null) replaces wholesale.
 * - Two plain objects at the same key merge recursively; anything else at
 *   that key replaces.
 * - Overrides apply in argument order; later overrides win.
 * - Every key lands as an own data property, written through
 *   Object.defineProperty, so a hostile override key such as "__proto__"
 *   can neither mutate the result's prototype nor be dropped silently.
 */
export function deepMerge<T extends object>(
  base: T,
  ...overrides: Array<DeepPartial<T>>
): T {
  const merged = {} as Record<string, unknown>
  copyInto(merged, base as unknown as Record<string, unknown>)
  for (const override of overrides) {
    mergeInto(merged, override as unknown as Record<string, unknown>)
  }
  return merged as T
}

function isPlainRecord(value: unknown): value is Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    return false
  }
  const prototype = Object.getPrototypeOf(value)
  return prototype === Object.prototype || prototype === null
}

/** Shallow-copy every own value of `source` onto `target` (own data props). */
function copyInto(target: Record<string, unknown>, source: Record<string, unknown>): void {
  for (const key of Object.keys(source)) {
    const value = source[key]
    if (value !== undefined) {
      writeOwn(target, key, value)
    }
  }
}

function mergeInto(target: Record<string, unknown>, source: Record<string, unknown>): void {
  for (const key of Object.keys(source)) {
    const value = source[key]
    if (value === undefined) {
      continue
    }
    if (isPlainRecord(value)) {
      const child: Record<string, unknown> = {}
      const existing = getOwn(target, key)
      if (isPlainRecord(existing)) {
        copyInto(child, existing)
      }
      writeOwn(target, key, child)
      mergeInto(child, value)
    } else {
      writeOwn(target, key, value)
    }
  }
}

function getOwn(target: Record<string, unknown>, key: string): unknown | undefined {
  return Object.prototype.hasOwnProperty.call(target, key) ? target[key] : undefined
}

function writeOwn(target: Record<string, unknown>, key: string, value: unknown): void {
  Object.defineProperty(target, key, {
    value,
    writable: true,
    enumerable: true,
    configurable: true,
  })
}
