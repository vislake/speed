/**
 * Breakpoint tokens.
 *
 * The five named breakpoints and their values match the MUI theme
 * breakpoints surface exactly (same keys, same values), so a ui-kit theme
 * adapter assigns theme.breakpoints.values from these unchanged. A screen
 * width is "at or above" a breakpoint's value.
 */

export type BreakpointKey = 'xs' | 'sm' | 'md' | 'lg' | 'xl'

export type BreakpointValues = { readonly [Key in BreakpointKey]: number }

/** The breakpoints section of the speed token tree (SpeedTokens["breakpoints"]). */
export interface BreakpointTokens {
  readonly values: BreakpointValues
}

export const breakpointTokens: BreakpointTokens = {
  values: {
    xs: 0,
    sm: 600,
    md: 900,
    lg: 1200,
    xl: 1536,
  },
}
