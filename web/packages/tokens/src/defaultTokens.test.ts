/**
 * Contract tests for the default token tree.
 *
 * These pin the *shape* guarantees a theme factory can rely on: every section
 * the SpeedTokens interface declares is populated, every color is a full hex
 * literal, scales are monotone where order matters, and the spots that
 * deliberately mirror MUI defaults (breakpoint values, z-index slots,
 * spacing unit, borderRadius single number) equal those defaults -- that
 * parity is what keeps the ui-kit theme mapping free of contortions.
 */

import { describe, expect, it } from 'vitest'
import { defaultTokens, deepMerge, type TokensOverride } from './index'

const HEX_COLOR = /^#[0-9A-F]{6}$/i

describe('defaultTokens', () => {
  it('ships every semantic role with main, light, dark and contrastText hex colors', () => {
    const roles = [
      'primary',
      'secondary',
      'error',
      'warning',
      'info',
      'success',
    ] as const
    for (const role of roles) {
      const color = defaultTokens.color.semantic[role]
      for (const key of ['main', 'light', 'dark', 'contrastText'] as const) {
        expect(HEX_COLOR.test(color[key]), `${role}.${key}`).toBe(true)
      }
    }
  })

  it('ships the full neutral ramp as hex colors, darkest at 950', () => {
    const tones = [50, 100, 200, 300, 400, 500, 600, 700, 800, 900, 950] as const
    for (const tone of tones) {
      const value = defaultTokens.color.neutral[tone]
      expect(HEX_COLOR.test(value), `neutral.${tone}`).toBe(true)
    }
    expect(defaultTokens.color.neutral[50]).toBe('#F8FAFC')
    expect(defaultTokens.color.neutral[950]).toBe('#020617')
  })

  it('ships the text, background and divider colors as hex literals', () => {
    for (const value of Object.values(defaultTokens.color.text)) {
      expect(HEX_COLOR.test(value)).toBe(true)
    }
    for (const value of Object.values(defaultTokens.color.background)) {
      expect(HEX_COLOR.test(value)).toBe(true)
    }
    expect(HEX_COLOR.test(defaultTokens.color.divider)).toBe(true)
  })

  it('ships Latin-first font stacks that end in a CJK-capable fallback and a generic family', () => {
    const { sans, mono } = defaultTokens.typography.fontFamily
    expect(sans.startsWith('ui-sans-serif, system-ui')).toBe(true)
    expect(sans).toContain("'PingFang SC'")
    expect(sans).toContain("'Microsoft YaHei'")
    expect(sans.endsWith('sans-serif')).toBe(true)
    expect(mono).toContain('monospace')
    expect(mono.endsWith('monospace')).toBe(true)
  })

  it('ships a strictly increasing font-size scale of positive pixel numbers', () => {
    const steps = ['xs', 'sm', 'md', 'lg', 'xl', '2xl', '3xl', '4xl', '5xl'] as const
    const sizes = steps.map((step) => defaultTokens.typography.fontSize[step])
    for (let i = 0; i < sizes.length - 1; i += 1) {
      expect(sizes[i]).toBeGreaterThan(0)
      expect(sizes[i + 1]).toBeGreaterThan(sizes[i]!)
    }
    expect(defaultTokens.typography.fontSize.xs).toBe(12)
    expect(defaultTokens.typography.fontSize['5xl']).toBe(48)
  })

  it('ships the standard font weights and unitless line heights above 1', () => {
    expect(defaultTokens.typography.fontWeight).toEqual({
      regular: 400,
      medium: 500,
      semibold: 600,
      bold: 700,
    })
    for (const value of Object.values(defaultTokens.typography.lineHeight)) {
      expect(value).toBeGreaterThan(1)
    }
    for (const value of Object.values(defaultTokens.typography.letterSpacing)) {
      expect(value.length).toBeGreaterThan(0)
    }
  })

  it('ships an 8px spacing unit and a single 8px border radius', () => {
    expect(defaultTokens.spacing.unit).toBe(8)
    expect(defaultTokens.shape.borderRadius).toBe(8)
  })

  it('ships the MUI breakpoint keys and values unchanged', () => {
    // Parity spot checks with the MUI default theme: the keys and values the
    // MUI theme uses, which the ui-kit adapter assigns 1:1.
    expect(defaultTokens.breakpoints.values).toEqual({
      xs: 0,
      sm: 600,
      md: 900,
      lg: 1200,
      xl: 1536,
    })
  })

  it('ships the MUI z-index slots with the MUI default ordering', () => {
    // Parity spot checks with the MUI theme zIndex defaults (1000..1500).
    expect(defaultTokens.zIndex.values).toEqual({
      mobileStepper: 1000,
      fab: 1050,
      speedDial: 1050,
      appBar: 1100,
      drawer: 1200,
      modal: 1300,
      snackbar: 1400,
      tooltip: 1500,
    })
  })

  it('ships a layered rgba box-shadow for every elevation slot', () => {
    const elevations = [1, 2, 4, 8, 16, 24] as const
    for (const elevation of elevations) {
      const shadow = defaultTokens.shadows[elevation]
      expect(shadow.length).toBeGreaterThan(0)
      expect(shadow).toContain('rgba(')
      expect(shadow).toContain('px')
    }
    expect(Object.keys(defaultTokens.shadows).map(Number).sort((a, b) => a - b)).toEqual([
      1, 2, 4, 8, 16, 24,
    ])
  })

  it('accepts partial project overrides and rejects shape drift at compile time', () => {
    const override: TokensOverride = {
      color: { semantic: { primary: { main: '#000000' } } },
      zIndex: { values: { drawer: 1300 } },
    }
    const merged = deepMerge(defaultTokens, override)
    expect(merged.color.semantic.primary.main).toBe('#000000')
    expect(merged.color.semantic.primary.light).toBe(
      defaultTokens.color.semantic.primary.light,
    )
    expect(merged.zIndex.values.drawer).toBe(1300)

    // @ts-expect-error unknown token sections are not overridable
    deepMerge(defaultTokens, { palette: { primary: { main: '#000000' } } })
    // @ts-expect-error token field types are enforced
    deepMerge(defaultTokens, { shape: { borderRadius: '8px' } })
    // @ts-expect-error overriding with an object where a string belongs is a shape error
    deepMerge(defaultTokens, { color: { divider: { main: '#000000' } } })
  })
})
