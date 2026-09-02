/**
 * Spacing tokens.
 *
 * A single base unit: every layout gap is unit x an integer or half-step
 * multiplier. The unit equals MUI's default theme.spacing base (8), so a
 * ui-kit theme adapter can set theme.spacing to a function over this unit
 * without rescaling.
 */

/** The spacing section of the speed token tree (SpeedTokens["spacing"]). */
export interface SpacingTokens {
  /** Base spacing unit in pixels; layout gaps are multiples of it. */
  readonly unit: number
}

export const spacingTokens: SpacingTokens = {
  unit: 8,
}
