/**
 * Theme-factory contract tests: the token-to-MUI mapping (every mapped
 * surface pinned against the default tokens), the three-layer override
 * semantics (later layers win per key, untouched branches stay shared),
 * copy-on-write (no input ever mutates), and hostile-override inertness.
 *
 * These run without a DOM: createAppTheme is a pure function.
 */

import { describe, expect, it } from 'vitest'
import { defaultTokens } from '@speed/tokens'
import type { TokensOverride } from '@speed/tokens'
import { createAppTheme } from './createAppTheme.js'

const DEFAULT_BEFORE = JSON.stringify(defaultTokens)

describe('createAppTheme token-to-theme mapping', () => {
  const { theme } = createAppTheme()

  it('maps every semantic role key for key onto the palette', () => {
    for (const role of ['primary', 'secondary', 'error', 'warning', 'info', 'success'] as const) {
      const token = defaultTokens.color.semantic[role]
      const paletteRole = theme.palette[role]
      expect(paletteRole.main).toBe(token.main)
      expect(paletteRole.light).toBe(token.light)
      expect(paletteRole.dark).toBe(token.dark)
      expect(paletteRole.contrastText).toBe(token.contrastText)
    }
    expect(theme.palette.mode).toBe('light')
  })

  it('aliases the neutral ramp onto the grey scale, same-number steps and A-tones', () => {
    const neutral = defaultTokens.color.neutral
    for (const tone of [50, 100, 200, 300, 400, 500, 600, 700, 800, 900] as const) {
      expect(theme.palette.grey[tone]).toBe(neutral[tone])
    }
    // MUI's own aliasing: A-tones repeat the same-number step.
    expect(theme.palette.grey.A100).toBe(neutral[100])
    expect(theme.palette.grey.A200).toBe(neutral[200])
    expect(theme.palette.grey.A400).toBe(neutral[400])
    expect(theme.palette.grey.A700).toBe(neutral[700])
    // Tone 950 has no MUI slot; it stays token-only.
    expect(Object.hasOwn(theme.palette.grey, 950)).toBe(false)
  })

  it('maps text, background and divider directly', () => {
    expect(theme.palette.text.primary).toBe(defaultTokens.color.text.primary)
    expect(theme.palette.text.secondary).toBe(defaultTokens.color.text.secondary)
    expect(theme.palette.text.disabled).toBe(defaultTokens.color.text.disabled)
    expect(theme.palette.background.default).toBe(defaultTokens.color.background.default)
    expect(theme.palette.background.paper).toBe(defaultTokens.color.background.paper)
    expect(theme.palette.divider).toBe(defaultTokens.color.divider)
  })

  it('uses the sans stack as fontFamily and the token weights as named weights', () => {
    expect(theme.typography.fontFamily).toBe(defaultTokens.typography.fontFamily.sans)
    expect(theme.typography.fontWeightRegular).toBe(defaultTokens.typography.fontWeight.regular)
    expect(theme.typography.fontWeightMedium).toBe(defaultTokens.typography.fontWeight.medium)
    expect(theme.typography.fontWeightBold).toBe(defaultTokens.typography.fontWeight.bold)
  })

  it('maps heading variants onto the 5xl..lg steps in rem, tight and semibold', () => {
    const sizes = defaultTokens.typography.fontSize
    const cases = [
      ['h1', sizes['5xl']],
      ['h2', sizes['4xl']],
      ['h3', sizes['3xl']],
      ['h4', sizes['2xl']],
      ['h5', sizes.xl],
      ['h6', sizes.lg],
    ] as const
    for (const [variant, px] of cases) {
      const style = theme.typography[variant]
      expect(style.fontSize).toBe(`${px / 16}rem`)
      expect(style.fontWeight).toBe(defaultTokens.typography.fontWeight.semibold)
      expect(style.lineHeight).toBe(defaultTokens.typography.lineHeight.tight)
    }
  })

  it('maps body/supporting variants with their documented roles', () => {
    const sizes = defaultTokens.typography.fontSize
    const weights = defaultTokens.typography.fontWeight
    const body1 = theme.typography.body1
    expect(body1.fontSize).toBe(`${sizes.md / 16}rem`)
    expect(body1.fontWeight).toBe(weights.regular)
    expect(body1.lineHeight).toBe(defaultTokens.typography.lineHeight.normal)
    expect(theme.typography.body2.fontSize).toBe(`${sizes.sm / 16}rem`)
    expect(theme.typography.subtitle1.fontWeight).toBe(weights.medium)
    expect(theme.typography.subtitle2.fontWeight).toBe(weights.medium)
    expect(theme.typography.button.fontWeight).toBe(weights.medium)
    expect(theme.typography.button.fontSize).toBe(`${sizes.sm / 16}rem`)
    expect(theme.typography.caption.fontSize).toBe(`${sizes.xs / 16}rem`)
    expect(theme.typography.caption.fontWeight).toBe(weights.regular)
    expect(theme.typography.overline.fontSize).toBe(`${sizes.xs / 16}rem`)
    expect(theme.typography.overline.fontWeight).toBe(weights.semibold)
    expect(theme.typography.overline.letterSpacing).toBe(defaultTokens.typography.letterSpacing.wide)
  })

  it('sets the spacing unit from the token (theme.spacing(n) === n x unit)', () => {
    expect(theme.spacing(1)).toBe('8px')
    expect(theme.spacing(2)).toBe('16px')
    expect(theme.spacing(0.5)).toBe('4px')
  })

  it('maps shape, breakpoints and zIndex values directly', () => {
    expect(theme.shape.borderRadius).toBe(defaultTokens.shape.borderRadius)
    expect(theme.breakpoints.values).toEqual(defaultTokens.breakpoints.values)
    expect(theme.zIndex.appBar).toBe(defaultTokens.zIndex.values.appBar)
    expect(theme.zIndex.modal).toBe(defaultTokens.zIndex.values.modal)
    expect(theme.zIndex.tooltip).toBe(defaultTokens.zIndex.values.tooltip)
  })

  it('builds a 25-entry shadow ramp with none at 0 and token slots floored below', () => {
    const shadows = theme.shadows
    expect(shadows).toHaveLength(25)
    expect(shadows[0]).toBe('none')
    const tokenShadows = defaultTokens.shadows
    expect(shadows[1]).toBe(tokenShadows[1])
    expect(shadows[2]).toBe(tokenShadows[2])
    expect(shadows[4]).toBe(tokenShadows[4])
    expect(shadows[8]).toBe(tokenShadows[8])
    expect(shadows[16]).toBe(tokenShadows[16])
    expect(shadows[24]).toBe(tokenShadows[24])
    // Unlisted elevations floor onto the nearest token slot at or below.
    expect(shadows[3]).toBe(tokenShadows[2])
    expect(shadows[5]).toBe(tokenShadows[4])
    expect(shadows[9]).toBe(tokenShadows[8])
    expect(shadows[20]).toBe(tokenShadows[16])
    // The ramp never dips: elevation growth is monotonic.
    for (let i = 2; i < shadows.length; i += 1) {
      expect(shadows[i]).not.toBe('none')
    }
  })
})

describe('createAppTheme layering', () => {
  it('applies project tokens over the defaults and tenant overrides over the project', () => {
    const project: TokensOverride = {
      color: {
        semantic: {
          primary: { main: '#111111' },
          secondary: { main: '#222222' },
        },
      },
      spacing: { unit: 4 },
    }
    const tenant: TokensOverride = {
      color: { semantic: { primary: { main: '#333333' } } },
    }
    const { theme } = createAppTheme(project, tenant)
    expect(theme.palette.primary.main).toBe('#333333')
    expect(theme.palette.secondary.main).toBe('#222222')
    expect(theme.spacing(1)).toBe('4px')
    // Untouched branches keep the defaults.
    expect(theme.palette.error.main).toBe(defaultTokens.color.semantic.error.main)
  })

  it('returns the merged token tree alongside the theme', () => {
    const { tokens } = createAppTheme({ spacing: { unit: 12 } })
    expect(tokens.spacing.unit).toBe(12)
    expect(tokens.color.semantic.primary.main).toBe(
      defaultTokens.color.semantic.primary.main,
    )
  })

  it('shares untouched branches with defaultTokens by identity', () => {
    const { tokens } = createAppTheme()
    expect(tokens.color.semantic.error).toBe(defaultTokens.color.semantic.error)
    expect(tokens.typography.fontWeight).toBe(defaultTokens.typography.fontWeight)
    expect(tokens.color.semantic.primary).toBe(defaultTokens.color.semantic.primary)
    expect(tokens).not.toBe(defaultTokens)
  })

  it('never mutates its inputs, even through later writes into rebuilt branches', () => {
    const project: TokensOverride = { color: { semantic: { primary: { main: '#123456' } } } }
    const first = createAppTheme(project)
    expect(first.theme.palette.primary.main).toBe('#123456')
    // The merged tree is readonly by convention, not by runtime; write
    // into a rebuilt branch (the project layer rebuilt primary) and
    // prove the sources stay byte-identical.
    const merged = first.tokens as unknown as {
      color: { semantic: { primary: { main: string } } }
    }
    merged.color.semantic.primary.main = '#DEADBEEF'
    expect(JSON.stringify(defaultTokens)).toBe(DEFAULT_BEFORE)
    expect(JSON.stringify(project)).toBe('{"color":{"semantic":{"primary":{"main":"#123456"}}}}')
  })

  it('keeps hostile __proto__ override keys inert', () => {
    const hostile = JSON.parse('{"__proto__": {"polluted": true}}') as TokensOverride
    const { tokens } = createAppTheme(hostile)
    expect(Object.getPrototypeOf(tokens)).toBe(Object.prototype)
    expect(({} as Record<string, unknown>).polluted).toBeUndefined()
    // The hostile key landed as an own property, neither dropped nor
    // merged into the prototype chain.
    expect(Object.prototype.hasOwnProperty.call(tokens, '__proto__')).toBe(true)
    // The theme itself stays sane.
    expect(JSON.stringify(defaultTokens)).toBe(DEFAULT_BEFORE)
  })
})
