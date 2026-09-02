/**
 * Contract tests for the MUI locale linkage: canonical language tag to the
 * MUI localization object, identity-stable, throwing on unknown tags.
 */

import { describe, expect, it } from 'vitest'
import { zhCN, enUS } from '@mui/material/locale'
import { muiLocaleFor } from './mui-locale'

describe('muiLocaleFor', () => {
  it('returns the MUI localization object for each shipped language', () => {
    expect(muiLocaleFor('zh-CN')).toBe(zhCN)
    expect(muiLocaleFor('en-US')).toBe(enUS)
  })

  it('is identity-stable across calls', () => {
    expect(muiLocaleFor('zh-CN')).toBe(muiLocaleFor('zh-CN'))
  })

  it('throws on unknown or non-canonical tags instead of silently mismatching locale text', () => {
    expect(() => muiLocaleFor('fr-FR')).toThrow(/no MUI localization for language "fr-FR"/)
    expect(() => muiLocaleFor('zh')).toThrow(/no MUI localization/)
    expect(() => muiLocaleFor('')).toThrow(/no MUI localization/)
  })
})
