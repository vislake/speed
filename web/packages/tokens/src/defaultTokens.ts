/**
 * The speed design-token defaults, assembled from the per-domain constants.
 *
 * This is the single default tree every project starts from; treat it as
 * immutable (never mutate it in place -- override through deepMerge, which
 * never mutates its inputs). deepMerge(defaultTokens, myOverride) is the
 * canonical override path; see README.md.
 */

import type { SpeedTokens } from './types.js'
import { breakpointTokens } from './breakpoints.js'
import { colorTokens } from './color.js'
import { shadowTokens } from './shadows.js'
import { shapeTokens } from './shape.js'
import { spacingTokens } from './spacing.js'
import { typographyTokens } from './typography.js'
import { zIndexTokens } from './z-index.js'

export const defaultTokens: SpeedTokens = {
  color: colorTokens,
  typography: typographyTokens,
  spacing: spacingTokens,
  shape: shapeTokens,
  breakpoints: breakpointTokens,
  zIndex: zIndexTokens,
  shadows: shadowTokens,
}
