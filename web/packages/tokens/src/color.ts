/**
 * Color tokens: the semantic role palette plus a neutral ramp.
 *
 * Field naming mirrors the MUI theme palette surface on purpose (semantic
 * roles carry main/light/dark/contrastText, neutrals use the grey-style
 * numeric steps), so the future ui-kit theme adapter maps tokens onto the
 * MUI theme shape key by key. The hex values are the speed brand defaults;
 * projects override them through deepMerge (see README.md).
 */

/** A semantic role's tonal family: default, light and dark variants plus the text color that reads on them. */
export interface SemanticColor {
  readonly main: string
  readonly light: string
  readonly dark: string
  readonly contrastText: string
}

/** The UI roles every speed application can use. */
export type SemanticRole = 'primary' | 'secondary' | 'error' | 'warning' | 'info' | 'success'

/** Palette of the six semantic roles. */
export type SemanticPalette = { readonly [Role in SemanticRole]: SemanticColor }

/** Neutral ramp steps; the numeric names match the MUI grey scale so theme mapping stays 1:1. */
export type NeutralTone =
  | 50
  | 100
  | 200
  | 300
  | 400
  | 500
  | 600
  | 700
  | 800
  | 900
  | 950

/** The neutral ramp, darkest at 950. */
export type NeutralScale = { readonly [Tone in NeutralTone]: string }

/** The color section of the speed token tree (SpeedTokens["color"]). */
export interface ColorTokens {
  readonly semantic: SemanticPalette
  readonly neutral: NeutralScale
  readonly text: {
    readonly primary: string
    readonly secondary: string
    readonly disabled: string
  }
  readonly background: {
    readonly default: string
    readonly paper: string
  }
  readonly divider: string
}

export const colorTokens: ColorTokens = {
  semantic: {
    primary: { main: '#2563EB', light: '#60A5FA', dark: '#1D4ED8', contrastText: '#FFFFFF' },
    secondary: { main: '#7C3AED', light: '#A78BFA', dark: '#6D28D9', contrastText: '#FFFFFF' },
    error: { main: '#DC2626', light: '#F87171', dark: '#B91C1C', contrastText: '#FFFFFF' },
    warning: { main: '#D97706', light: '#FBBF24', dark: '#B45309', contrastText: '#FFFFFF' },
    info: { main: '#0284C7', light: '#38BDF8', dark: '#0369A1', contrastText: '#FFFFFF' },
    success: { main: '#16A34A', light: '#4ADE80', dark: '#15803D', contrastText: '#FFFFFF' },
  },
  neutral: {
    50: '#F8FAFC',
    100: '#F1F5F9',
    200: '#E2E8F0',
    300: '#CBD5E1',
    400: '#94A3B8',
    500: '#64748B',
    600: '#475569',
    700: '#334155',
    800: '#1E293B',
    900: '#0F172A',
    950: '#020617',
  },
  text: {
    primary: '#0F172A',
    secondary: '#475569',
    disabled: '#94A3B8',
  },
  background: {
    default: '#F8FAFC',
    paper: '#FFFFFF',
  },
  divider: '#E2E8F0',
}
