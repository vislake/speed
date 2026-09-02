/**
 * Contract tests for language-tag normalization, supported-set matching and
 * the negotiation chain precedence.
 */

import { describe, expect, it } from 'vitest'
import {
  DEFAULT_LANGUAGE,
  DEFAULT_SUPPORTED_LANGUAGES,
  detectLanguage,
  matchSupportedLanguage,
  normalizeLanguageTag,
  readSupportedLanguages,
} from './languages'

describe('normalizeLanguageTag', () => {
  it('passes canonical tags through unchanged', () => {
    expect(normalizeLanguageTag('zh-CN')).toBe('zh-CN')
    expect(normalizeLanguageTag('en-US')).toBe('en-US')
    expect(normalizeLanguageTag('en')).toBe('en')
  })

  it('maps underscores to hyphens and tolerates casing and whitespace', () => {
    expect(normalizeLanguageTag('zh_CN')).toBe('zh-CN')
    expect(normalizeLanguageTag('  EN-us ')).toBe('EN-us')
    expect(normalizeLanguageTag('zh-Hans-CN')).toBe('zh-Hans-CN')
  })

  it('rejects strings that are not language tags', () => {
    expect(normalizeLanguageTag('')).toBeNull()
    expect(normalizeLanguageTag('123')).toBeNull()
    expect(normalizeLanguageTag('en-!x')).toBeNull()
    expect(normalizeLanguageTag('-en')).toBeNull()
    expect(normalizeLanguageTag('e')).toBeNull()
  })
})

describe('matchSupportedLanguage', () => {
  const supported = [...DEFAULT_SUPPORTED_LANGUAGES]

  it('matches canonical tags exactly, case-insensitively', () => {
    expect(matchSupportedLanguage('zh-CN', supported)).toBe('zh-CN')
    expect(matchSupportedLanguage('EN-us', supported)).toBe('en-US')
  })

  it('selects a supported tag from a bare primary subtag when unique', () => {
    expect(matchSupportedLanguage('en', supported)).toBe('en-US')
    expect(matchSupportedLanguage('zh', supported)).toBe('zh-CN')
  })

  it('refuses a bare primary subtag that is ambiguous', () => {
    expect(matchSupportedLanguage('en', ['en-US', 'en-GB'])).toBeNull()
  })

  it('returns null for tags no supported language matches', () => {
    expect(matchSupportedLanguage('fr', supported)).toBeNull()
    expect(matchSupportedLanguage('de-DE', supported)).toBeNull()
    expect(matchSupportedLanguage('', supported)).toBeNull()
  })
})

describe('detectLanguage', () => {
  const base = {
    supportedLanguages: [...DEFAULT_SUPPORTED_LANGUAGES],
    defaultLanguage: DEFAULT_LANGUAGE,
  }

  it('prefers the URL parameter over every other source', () => {
    expect(
      detectLanguage({
        ...base,
        urlLanguage: 'en-US',
        storedLanguage: 'zh-CN',
        profileLanguage: 'zh-CN',
        navigatorLanguages: ['zh-CN'],
      }),
    ).toBe('en-US')
  })

  it('prefers the stored choice over profile and navigator', () => {
    expect(
      detectLanguage({
        ...base,
        storedLanguage: 'en-US',
        profileLanguage: 'zh-CN',
        navigatorLanguages: ['zh-CN'],
      }),
    ).toBe('en-US')
  })

  it('prefers the profile locale over the navigator', () => {
    expect(
      detectLanguage({
        ...base,
        profileLanguage: 'zh-CN',
        navigatorLanguages: ['en-US'],
      }),
    ).toBe('zh-CN')
  })

  it('falls back to the first navigator language that matches', () => {
    expect(
      detectLanguage({
        ...base,
        navigatorLanguages: ['fr', 'en-US'],
      }),
    ).toBe('en-US')
  })

  it('skips sources that match nothing and returns the default when all miss', () => {
    expect(
      detectLanguage({
        ...base,
        urlLanguage: 'fr-FR',
        storedLanguage: 'de',
        profileLanguage: '',
        navigatorLanguages: ['ja-JP'],
      }),
    ).toBe('zh-CN')
  })

  it('treats empty sources as absent', () => {
    expect(detectLanguage({ ...base, urlLanguage: '' })).toBe('zh-CN')
  })

  it('can still pick a language outside the default pair when supported', () => {
    expect(
      detectLanguage({
        supportedLanguages: ['zh-CN', 'en-US', 'fr-FR'],
        defaultLanguage: 'zh-CN',
        navigatorLanguages: ['fr-FR'],
      }),
    ).toBe('fr-FR')
  })
})

describe('readSupportedLanguages', () => {
  it('passes an array through and wraps a single string', () => {
    expect(
      readSupportedLanguages({ options: { supportedLngs: ['zh-CN', 'en-US'] } }),
    ).toEqual(['zh-CN', 'en-US'])
    expect(readSupportedLanguages({ options: { supportedLngs: 'zh-CN' } })).toEqual([
      'zh-CN',
    ])
  })

  it('returns [] when the instance pins no supported set', () => {
    expect(readSupportedLanguages({ options: {} })).toEqual([])
  })
})
