/**
 * Shape tokens.
 *
 * One radius in pixels for all corners. MUI's theme.shape.borderRadius is a
 * single number, so the mapping is direct.
 */

/** The shape section of the speed token tree (SpeedTokens["shape"]). */
export interface ShapeTokens {
  readonly borderRadius: number
}

export const shapeTokens: ShapeTokens = {
  borderRadius: 8,
}
