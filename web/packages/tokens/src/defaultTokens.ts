/**
 * The speed design-token defaults, assembled from the per-domain constants.
 *
 * This is the single default tree every project starts from; treat it as
 * immutable (never mutate it in place -- override through deepMerge, which
 * never mutates its inputs). deepMerge(defaultTokens, myOverride) is the
 * canonical override path; see README.md.
 */

import type { SpeedTokens } from './types'
import { breakpointTokens } from './breakpoints'
import { colorTokens } from './color'
import { shadowTokens } from './shadows'
import { shapeTokens } from './shape'
import { spacingTokens } from './spacing'
import { typographyTokens } from './typography'
import { zIndexTokens } from './z-index'

export const defaultTokens: SpeedTokens = {
  color: colorTokens,
  typography: typographyTokens,
  spacing: spacingTokens,
  shape: shapeTokens,
  breakpoints: breakpointTokens,
  zIndex: zIndexTokens,
  shadows: shadowTokens,
}
