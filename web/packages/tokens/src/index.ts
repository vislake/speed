/**
 * Public entry of @speed/tokens.
 *
 * The runtime surface is deliberately two symbols: defaultTokens (the tree
 * every project starts from) and deepMerge (the only sanctioned way to
 * override it). Everything else is types, so token authors and the future
 * ui-kit theme adapter get full compile-time shape without extra runtime.
 */

export { defaultTokens } from './defaultTokens'
export { deepMerge } from './merge'

export type { BreakpointKey, BreakpointTokens, BreakpointValues } from './breakpoints'
export type {
  ColorTokens,
  NeutralScale,
  NeutralTone,
  SemanticColor,
  SemanticPalette,
  SemanticRole,
} from './color'
export type { ElevationSlot, ShadowTokens } from './shadows'
export type { ShapeTokens } from './shape'
export type { SpacingTokens } from './spacing'
export type { DeepPartial, SpeedTokens, TokensOverride } from './types'
export type {
  FontSizeScale,
  FontSizeStep,
  FontWeightKey,
  FontWeightScale,
  TypographyTokens,
} from './typography'
export type { ZIndexKey, ZIndexTokens, ZIndexValues } from './z-index'
