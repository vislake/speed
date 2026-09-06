/**
 * The speed token tree: one interface per section, one merged whole.
 *
 * Section shapes are documented domain by domain (see color.ts,
 * typography.ts, ... and README.md for the MUI mapping table). Projects do
 * not build a SpeedTokens from scratch: they start from defaultTokens and
 * hand a TokensOverride to deepMerge.
 */

import type { BreakpointTokens } from './breakpoints.js'
import type { ColorTokens } from './color.js'
import type { ShadowTokens } from './shadows.js'
import type { ShapeTokens } from './shape.js'
import type { SpacingTokens } from './spacing.js'
import type { TypographyTokens } from './typography.js'
import type { ZIndexTokens } from './z-index.js'

/** The complete token tree a theme is built from. */
export interface SpeedTokens {
  readonly color: ColorTokens
  readonly typography: TypographyTokens
  readonly spacing: SpacingTokens
  readonly shape: ShapeTokens
  readonly breakpoints: BreakpointTokens
  readonly zIndex: ZIndexTokens
  readonly shadows: ShadowTokens
}

/**
 * A recursive partial of an object type. Arrays and functions are leaves:
 * an override supplies the whole array or function, which replaces
 * wholesale. The token tree contains neither, but the helper is generic
 * and must not fabricate array members -- and a function-typed field must
 * only ever be overridden by a function, never by a value that would
 * replace a callable with non-callable data.
 */
export type DeepPartial<T> = {
  [K in keyof T]?: T[K] extends (...args: never[]) => unknown
    ? T[K]
    : T[K] extends readonly unknown[]
      ? T[K]
      : T[K] extends object
        ? DeepPartial<T[K]>
        : T[K]
}

/** What a project overrides of the speed defaults: any partial token tree. */
export type TokensOverride = DeepPartial<SpeedTokens>
