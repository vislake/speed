/**
 * The theme factory: speed tokens into an MUI v9 theme.
 *
 * Layering is the token tree's own discipline applied three deep:
 *
 *   defaultTokens  <-  projectTokens  <-  tenantOverrides
 *
 * Each layer is a diff against the one below it, merged through the
 * tokens package's deepMerge (copy-on-write: untouched branches stay
 * shared with the defaults by identity, no input is ever mutated, hostile
 * "__proto__" override keys land as inert own properties). The factory
 * then maps the merged tree onto MUI's createTheme surface:
 *
 * - palette roles: each semantic role's main/light/dark/contrastText maps
 *   key for key; the neutral ramp aliases the MUI grey scale (steps 50-900
 *   by number; A100/A200/A400/A700 alias the same-number steps, matching
 *   MUI's own grey aliasing). Tone 950 has no MUI slot and stays a token
 *   only. text/background/divider map directly.
 * - spacing: the token unit IS the MUI spacing unit (probe-verified
 *   against MUI 9.4.0: a numeric spacing option sets the base unit, so
 *   theme.spacing(n) === n x unit).
 * - shape.borderRadius maps directly; breakpoints.values and
 *   zIndex.values map directly (the slot names are MUI's own).
 * - typography: fontFamily.sans becomes the theme font family; the named
 *   size steps feed the variant role table, converted to rem on a 16px
 *   base (MUI's default htmlFontSize); fontWeight.regular/medium/bold
 *   fill fontWeightRegular/Medium/Bold (light has no token and keeps
 *   MUI's 300). Role table (all documented in the README):
 *   h1-h6 <- 5xl..lg, tight line height, semibold;
 *   subtitle1/2 <- md/sm, normal line height, medium;
 *   body1/2 <- md/sm, normal line height, regular;
 *   button <- sm, medium; caption <- xs, regular;
 *   overline <- xs, semibold, wide letter spacing.
 * - shadows: MUI themes carry 25 elevations. The six token slots are
 *   floored onto the ramp -- elevation i uses the nearest token slot at
 *   or below i (monotonic, never exceeding the design scale) -- with
 *   'none' at 0. MUI 9 accepts any array length, so the 25-entry ramp is
 *   deliberate: consumers can index theme.shadows like any stock MUI
 *   theme.
 *
 * The returned theme is locale-free on purpose: AppThemeProvider merges
 * the MUI locale matching the active language on top at render time, and
 * hosts composing their own theme do the same via
 * createTheme(appTheme.theme, muiLocaleFor(i18n.language)).
 *
 * MUI 9 notes verified against the installed 9.4.0 at implementation
 * time: css variables are off by default (theme.vars is absent), so no
 * cssVariables setup is needed for a plain palette theme; createTheme
 * partial options merge with the defaults (keys left unmapped keep MUI's
 * defaults); the typography option surface is TypographyVariantsOptions
 * with 13 variants (no inherit).
 */

import { createTheme } from '@mui/material/styles'
import type {
  PaletteColorOptions,
  PaletteOptions,
  Theme,
  ThemeOptions,
} from '@mui/material/styles'
import {
  deepMerge,
  defaultTokens,
  type ColorTokens,
  type NeutralScale,
  type SemanticRole,
  type ShadowTokens,
  type SpeedTokens,
  type TokensOverride,
} from '@speed/tokens'

/** Typography rem base; MUI's default htmlFontSize is 16, keep aligned. */
const REM_BASE = 16

/** The token elevation slots in ascending order (their numeric key order). */
const ELEVATION_SLOTS = [1, 2, 4, 8, 16, 24] as const

/** What the factory hands back: the merged tokens and the theme built from them. */
export interface AppTheme {
  /**
   * The merged token tree (defaults <- project <- tenant layers). Each
   * untouched branch is shared by identity with defaultTokens; treat the
   * tree as immutable and override through the factory's own arguments.
   */
  readonly tokens: SpeedTokens
  /**
   * The MUI theme assembled from the merged tokens. Locale-free: compose
   * with createTheme(appTheme.theme, muiLocaleFor(language)) -- or let
   * AppThemeProvider do it -- for MUI built-in texts to follow the
   * active language.
   */
  readonly theme: Theme
}

/**
 * Build the theme for merged speed tokens.
 *
 * `projectTokens` and `tenantOverrides` are the two override layers on
 * top of the built-in defaults, applied in that order (each diff-only).
 * Both are optional; a layer that is undefined is skipped entirely.
 */
export function createAppTheme(
  projectTokens?: TokensOverride,
  tenantOverrides?: TokensOverride,
): AppTheme {
  // deepMerge skips undefined overrides by contract; drop empty layers
  // before the call so an omitted layer never reaches the type signature.
  const layers = [projectTokens, tenantOverrides].filter(
    (layer): layer is TokensOverride => layer !== undefined,
  )
  const tokens = deepMerge(defaultTokens, ...layers)
  return { tokens, theme: createTheme(tokensToThemeOptions(tokens)) }
}

function rem(px: number): string {
  return `${px / REM_BASE}rem`
}

function roleOptions(color: ColorTokens['semantic'][SemanticRole]): PaletteColorOptions {
  return {
    main: color.main,
    light: color.light,
    dark: color.dark,
    contrastText: color.contrastText,
  }
}

function neutralAsGrey(neutral: NeutralScale): PaletteOptions['grey'] {
  // A-tone aliases repeat the same-number step, mirroring MUI's own grey
  // scale where A100 === 100, A200 === 200, A400 === 400, A700 === 700.
  // Tone 950 is not part of MUI's grey and stays token-only.
  return {
    50: neutral[50],
    100: neutral[100],
    200: neutral[200],
    300: neutral[300],
    400: neutral[400],
    500: neutral[500],
    600: neutral[600],
    700: neutral[700],
    800: neutral[800],
    900: neutral[900],
    A100: neutral[100],
    A200: neutral[200],
    A400: neutral[400],
    A700: neutral[700],
  }
}

/**
 * MUI's theme.shadows is a 25-entry ramp indexed by elevation. The token
 * set only defines six slots; floor every other index onto the nearest
 * token slot at or below it so elevation never exceeds the design scale.
 *
 * MUI 9 types the ramp as a fixed 25-tuple; the loop below provably
 * produces 25 entries ('none' plus one push per elevation 1..24), which
 * the cast asserts on the runtime guarantee, not on faith.
 */
function shadowRamp(shadows: ShadowTokens): ThemeOptions['shadows'] {
  const ramp: string[] = ['none']
  let slotIndex = 0
  for (let elevation = 1; elevation <= 24; elevation += 1) {
    const next = ELEVATION_SLOTS[slotIndex + 1]
    if (next !== undefined && next <= elevation) {
      slotIndex += 1
    }
    ramp.push(shadows[ELEVATION_SLOTS[slotIndex] as (typeof ELEVATION_SLOTS)[number]])
  }
  return ramp as unknown as ThemeOptions['shadows']
}

/** Map merged speed tokens onto the MUI theme options surface. */
function tokensToThemeOptions(tokens: SpeedTokens): ThemeOptions {
  const t = tokens.typography
  const heading = (px: number) => ({
    fontSize: rem(px),
    lineHeight: t.lineHeight.tight,
    fontWeight: t.fontWeight.semibold,
    letterSpacing: t.letterSpacing.normal,
  })
  const bodyText = (px: number, weight: number, letterSpacing?: string) => ({
    fontSize: rem(px),
    lineHeight: t.lineHeight.normal,
    fontWeight: weight,
    letterSpacing: letterSpacing ?? t.letterSpacing.normal,
  })
  return {
    palette: {
      mode: 'light',
      primary: roleOptions(tokens.color.semantic.primary),
      secondary: roleOptions(tokens.color.semantic.secondary),
      error: roleOptions(tokens.color.semantic.error),
      warning: roleOptions(tokens.color.semantic.warning),
      info: roleOptions(tokens.color.semantic.info),
      success: roleOptions(tokens.color.semantic.success),
      grey: neutralAsGrey(tokens.color.neutral),
      text: {
        primary: tokens.color.text.primary,
        secondary: tokens.color.text.secondary,
        disabled: tokens.color.text.disabled,
      },
      background: {
        default: tokens.color.background.default,
        paper: tokens.color.background.paper,
      },
      divider: tokens.color.divider,
    },
    typography: {
      fontFamily: t.fontFamily.sans,
      fontWeightRegular: t.fontWeight.regular,
      fontWeightMedium: t.fontWeight.medium,
      fontWeightBold: t.fontWeight.bold,
      h1: heading(t.fontSize['5xl']),
      h2: heading(t.fontSize['4xl']),
      h3: heading(t.fontSize['3xl']),
      h4: heading(t.fontSize['2xl']),
      h5: heading(t.fontSize.xl),
      h6: heading(t.fontSize.lg),
      subtitle1: bodyText(t.fontSize.md, t.fontWeight.medium),
      subtitle2: bodyText(t.fontSize.sm, t.fontWeight.medium),
      body1: bodyText(t.fontSize.md, t.fontWeight.regular),
      body2: bodyText(t.fontSize.sm, t.fontWeight.regular),
      button: bodyText(t.fontSize.sm, t.fontWeight.medium),
      caption: bodyText(t.fontSize.xs, t.fontWeight.regular),
      overline: bodyText(t.fontSize.xs, t.fontWeight.semibold, t.letterSpacing.wide),
    },
    spacing: tokens.spacing.unit,
    shape: { borderRadius: tokens.shape.borderRadius },
    breakpoints: { values: { ...tokens.breakpoints.values } },
    zIndex: { ...tokens.zIndex.values },
    shadows: shadowRamp(tokens.shadows),
  }
}
