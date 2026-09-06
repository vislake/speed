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

import type { DeepPartial } from './types.js'

/**
 * Merge `overrides` into a copy of `base`, recursively, and return the copy.
 *
 * Semantics, in contract form (each is pinned by a test):
 * - No argument is ever mutated. Copy-on-write: a branch that no override
 *   touches is shared with `base` by identity; every branch some override
 *   does touch is rebuilt.
 * - The result is a faithful copy of `base`: every own key of `base`
 *   survives, even one whose value is `undefined` -- an override that
 *   omits a key never causes that key to vanish from the result.
 * - `undefined` values in overrides are skipped, so a partial override never
 *   blanks a token. Every other value (including null) replaces wholesale.
 * - Two plain objects at the same key merge recursively; anything else at
 *   that key replaces.
 * - Overrides apply in argument order; later overrides win.
 * - Every key lands as an own data property, written through
 *   Object.defineProperty, so a hostile override key such as "__proto__"
 *   can neither mutate the result's prototype nor be dropped silently:
 *   "__proto__" is defined non-enumerably (deep-merged like any other key,
 *   still readable off the result), so no [[Set]]-based copy such as
 *   Object.assign can ever carry it onto another object's prototype.
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

/**
 * Shallow-copy every own key of `source` onto `target` as an own data
 * property, values verbatim (an `undefined` value is copied too: this side
 * copies `base` data, and the result must stay a faithful copy of it -- the
 * skip-undefined rule belongs to the override side in mergeInto only).
 */
function copyInto(target: Record<string, unknown>, source: Record<string, unknown>): void {
  for (const key of Object.keys(source)) {
    writeOwn(target, key, source[key])
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
    // "__proto__" must never be enumerable: an enumerable own "__proto__"
    // data property is picked up by downstream [[Set]]-based copies
    // (Object.assign) and would reassign the receiving object's prototype.
    // Non-enumerable, the payload stays on the result -- own, readable,
    // deep-merged like any other key -- but no copy operation can carry it.
    enumerable: key !== '__proto__',
    configurable: true,
  })
}
