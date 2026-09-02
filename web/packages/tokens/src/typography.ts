/**
 * Typography tokens: font stacks, sizes, weights, line heights and letter
 * spacing.
 *
 * The font stacks are Latin-first with CJK-capable fallbacks appended, so a
 * single family token serves both writing systems; a ui-kit theme adapter
 * maps them onto the MUI theme's fontFamily surface unchanged. Font sizes are
 * pixel numbers; the adapter converts to its own units when needed.
 */

/** Named size steps; smaller than 2xl are spelled out, larger ones use the MUI-style numeric prefix. */
export type FontSizeStep = 'xs' | 'sm' | 'md' | 'lg' | 'xl' | '2xl' | '3xl' | '4xl' | '5xl'

export type FontSizeScale = { readonly [Step in FontSizeStep]: number }

export type FontWeightKey = 'regular' | 'medium' | 'semibold' | 'bold'

export type FontWeightScale = { readonly [Weight in FontWeightKey]: number }

/** The typography section of the speed token tree (SpeedTokens["typography"]). */
export interface TypographyTokens {
  readonly fontFamily: {
    /** Latin + CJK ("PingFang SC" et al.) humanist stack; maps to a MUI theme's fontFamily. */
    readonly sans: string
    readonly mono: string
  }
  readonly fontSize: FontSizeScale
  readonly fontWeight: FontWeightScale
  /** Unitless multipliers, as CSS line-height accepts. */
  readonly lineHeight: {
    readonly tight: number
    readonly normal: number
    readonly relaxed: number
  }
  readonly letterSpacing: {
    readonly normal: string
    readonly wide: string
  }
}

export const typographyTokens: TypographyTokens = {
  fontFamily: {
    sans: "ui-sans-serif, system-ui, -apple-system, 'Segoe UI', Roboto, 'Helvetica Neue', Arial, 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', sans-serif",
    mono: "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, 'Liberation Mono', 'Courier New', monospace",
  },
  fontSize: {
    xs: 12,
    sm: 14,
    md: 16,
    lg: 18,
    xl: 20,
    '2xl': 24,
    '3xl': 30,
    '4xl': 36,
    '5xl': 48,
  },
  fontWeight: {
    regular: 400,
    medium: 500,
    semibold: 600,
    bold: 700,
  },
  lineHeight: {
    tight: 1.25,
    normal: 1.5,
    relaxed: 1.75,
  },
  letterSpacing: {
    normal: '0em',
    wide: '0.05em',
  },
}
