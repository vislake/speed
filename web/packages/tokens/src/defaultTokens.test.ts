/**
 * Contract tests for the default token tree.
 *
 * These pin the *shape* guarantees a theme factory can rely on: every section
 * the SpeedTokens interface declares is populated, every color is a full hex
 * literal, scales are monotone where order matters, and the spots that
 * deliberately mirror MUI defaults -- breakpoint values, z-index slots and
 * the spacing unit -- equal those defaults, which is what keeps the ui-kit
 * theme mapping free of contortions. That parity is anchored on the real
 * installed MUI theme (a dev-only @mui/material import; the package itself
 * ships zero runtime dependencies), never on in-tree copies of the expected
 * values, so an MUI default change fails here instead of passing a
 * self-comparison. shape.borderRadius is deliberately NOT a parity spot --
 * tokens ship 8 where MUI defaults to 4 -- and the deviation is pinned as
 * one, so it is re-decided loudly rather than drifting silently.
 */

import { createTheme } from '@mui/material/styles'
import { describe, expect, it } from 'vitest'
import { defaultTokens, deepMerge, type TokensOverride } from './index'

const HEX_COLOR = /^#[0-9A-F]{6}$/i

// Pristine snapshots captured at module load, before any test can write
// through a merged result: the isolation tests below prove the assembled
// defaults stay byte-identical to these no matter what a merge result's
// consumer tries.
const DEFAULT_BEFORE = JSON.stringify(defaultTokens)
const PRISTINE_ERROR_MAIN = defaultTokens.color.semantic.error.main

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
    // Parity against the real installed MUI theme -- the values the ui-kit
    // adapter assigns 1:1. The reference is the live library, never an
    // in-tree copy of the expectation: a copied expectation could only
    // agree with the tokens it was copied from.
    const { values } = createTheme({}).breakpoints
    expect(defaultTokens.breakpoints.values).toEqual(values)
  })

  it('ships the MUI z-index slots with the MUI default values', () => {
    const mui = createTheme({})
    // Comparing the whole zIndex object also pins the slot-name set: an
    // MUI slot added, renamed or revalued fails here.
    expect(defaultTokens.zIndex.values).toEqual(mui.zIndex)
  })

  it('anchors the spacing and border-radius claims on the installed MUI theme', () => {
    const mui = createTheme({})
    // The spacing unit mirrors MUI's default spacing base: the token unit
    // IS the MUI unit, so the adapter needs no rescaling.
    expect(mui.spacing(1)).toBe(`${defaultTokens.spacing.unit}px`)
    // Border radius is the recorded deviation: MUI defaults to 4, tokens
    // ship 8 (the README parity row documents both sides). Assert both
    // against the live library so an MUI default change re-decides the
    // deviation instead of letting it drift.
    expect(mui.shape.borderRadius).toBe(4)
    expect(defaultTokens.shape.borderRadius).not.toBe(mui.shape.borderRadius)
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

describe('defaultTokens shared-branch write enforcement', () => {
  it('throws when a write would reach the frozen defaults through a shared branch', () => {
    const merged = deepMerge(defaultTokens, {
      color: { semantic: { primary: { main: '#000000' } } },
    })
    // The type layer seals the tree at every depth (readonly modifiers), so
    // a real write needs the trigger channel the P1 finding names -- a JS
    // consumer, an `any`, or a cast -- which is what the writable view
    // below stands in for. The override rebuilt only the primary branch;
    // untouched branches of the result ARE the defaultTokens nodes
    // (copy-on-write sharing), so a write through one must throw in strict
    // mode instead of silently polluting the module singleton. The default
    // tree is deep-frozen at assembly (see defaultTokens.ts); these two
    // writes sit at different sharing depths -- one branch the override
    // never entered, one whole top-level section -- and both must refuse.
    const writable = merged as unknown as {
      color: { semantic: { error: { main: string } } }
      typography: { fontFamily: { sans: string } }
    }
    const writeDeepSharedBranch = (): void => {
      writable.color.semantic.error.main = '#DEADBEEF'
    }
    const writeTopLevelSharedBranch = (): void => {
      writable.typography.fontFamily.sans = '"Helvetica Neue"'
    }
    expect(writeDeepSharedBranch).toThrow(TypeError)
    expect(writeTopLevelSharedBranch).toThrow(TypeError)
  })

  it('keeps the defaults byte-identical after a write attempt through a shared branch', () => {
    const merged = deepMerge(defaultTokens, {
      color: { semantic: { primary: { main: '#000000' } } },
    })
    // Writable view: the cast stands in for the JS-consumer trigger channel
    // that bypasses the type layer's readonly seal (see the throws-when
    // test above).
    const writable = merged as unknown as {
      color: { semantic: { error: { main: string } } }
    }
    let attemptThrew = false
    try {
      // The branch this override never touched: the write must not land
      // anywhere shared. Refusing loudly is the deep freeze's job (pinned
      // by the throws-when test above); this test pins the outcome.
      writable.color.semantic.error.main = '#DEADBEEF'
    } catch {
      attemptThrew = true
    }
    // No pollution: the branch the write aimed at still holds the pristine
    // value, and the assembled defaults are byte-identical to the snapshot
    // taken at module load.
    expect(defaultTokens.color.semantic.error.main).toBe(PRISTINE_ERROR_MAIN)
    expect(JSON.stringify(defaultTokens)).toBe(DEFAULT_BEFORE)
    expect(attemptThrew).toBe(true)
  })

  it('reads only its own values in a later independent merge after a write attempt', () => {
    const merged = deepMerge(defaultTokens, {
      color: { semantic: { primary: { main: '#000000' } } },
    })
    const writable = merged as unknown as {
      color: { semantic: { error: { main: string } } }
    }
    let attemptThrew = false
    try {
      writable.color.semantic.error.main = '#DEADBEEF'
    } catch {
      attemptThrew = true
    }
    // No leak: an independent merge after the attempt reads its own
    // override and pristine defaults -- never the attempted value.
    const tenantB = deepMerge(defaultTokens, { zIndex: { values: { modal: 1600 } } })
    expect(tenantB.zIndex.values.modal).toBe(1600)
    expect(tenantB.color.semantic.error.main).toBe(PRISTINE_ERROR_MAIN)
    expect(tenantB.typography.fontFamily.sans).toBe(
      defaultTokens.typography.fontFamily.sans,
    )
    expect(attemptThrew).toBe(true)
  })
})
