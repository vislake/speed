/**
 * Public entry of @speed/tokens.
 *
 * The runtime surface is deliberately two symbols: defaultTokens (the tree
 * every project starts from) and deepMerge (the only sanctioned way to
 * override it). Everything else is types, so token authors and the future
 * ui-kit theme adapter get full compile-time shape without extra runtime.
 */

export { defaultTokens } from './defaultTokens.js'
export { deepMerge } from './merge.js'

export type { BreakpointKey, BreakpointTokens, BreakpointValues } from './breakpoints.js'
export type {
  ColorTokens,
  NeutralScale,
  NeutralTone,
  SemanticColor,
  SemanticPalette,
  SemanticRole,
} from './color.js'
export type { ElevationSlot, ShadowTokens } from './shadows.js'
export type { ShapeTokens } from './shape.js'
export type { SpacingTokens } from './spacing.js'
export type { DeepPartial, SpeedTokens, TokensOverride } from './types.js'
export type {
  FontSizeScale,
  FontSizeStep,
  FontWeightKey,
  FontWeightScale,
  TypographyTokens,
} from './typography.js'
export type { ZIndexKey, ZIndexTokens, ZIndexValues } from './z-index.js'
