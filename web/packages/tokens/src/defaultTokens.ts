/**
 * The speed design-token defaults, assembled from the per-domain constants.
 *
 * This is the single default tree every project starts from, and it is
 * deep-frozen at assembly: immutability is enforced at runtime, not just
 * documented. Because deepMerge shares untouched branches with its base by
 * identity (copy-on-write), every branch of a merge result that no override
 * touched IS a node of this frozen tree -- so a write attempt through such
 * a branch throws in strict mode instead of silently polluting the default
 * tree every override layer starts from. Branches an override rebuilt are
 * fresh objects owned by that result alone; a write there is local and can
 * never reach this tree. Override through deepMerge, which never mutates
 * its inputs: deepMerge(defaultTokens, myOverride) is the canonical
 * override path; see README.md.
 */

import type { SpeedTokens } from './types.js'
import { breakpointTokens } from './breakpoints.js'
import { colorTokens } from './color.js'
import { shadowTokens } from './shadows.js'
import { shapeTokens } from './shape.js'
import { spacingTokens } from './spacing.js'
import { typographyTokens } from './typography.js'
import { zIndexTokens } from './z-index.js'

/** Recursively freeze an object in place, idempotently, and return it. */
function deepFreeze<T>(value: T): T {
  if (typeof value === 'object' && value !== null) {
    for (const child of Object.values(value as Record<string, unknown>)) {
      deepFreeze(child)
    }
    Object.freeze(value)
  }
  return value
}

export const defaultTokens: SpeedTokens = deepFreeze({
  color: colorTokens,
  typography: typographyTokens,
  spacing: spacingTokens,
  shape: shapeTokens,
  breakpoints: breakpointTokens,
  zIndex: zIndexTokens,
  shadows: shadowTokens,
})
