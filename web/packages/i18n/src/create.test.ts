/**
 * Contract tests for createI18n and switchLanguage: negotiation inputs and
 * precedence, the discipline options init pins, persistence semantics, and
 * the end-to-end proof that a missing key in the loaded language never
 * renders another language's text (the no-silent-fallback analogue of
 * go/pkgcore/i18n, whose catalog never falls back across languages).
 *
 * CJK assertions compare against the imported fixtures; no language
 * literals live in this file.
 */

import { afterEach, describe, expect, it, vi } from 'vitest'
import { createInstance } from 'i18next'
import { createI18n, switchLanguage, SPEED_LOCALE_STORAGE_KEY } from './index'
import { MemoryStorage } from '../test-utils/memory-storage'
import { createTestI18n } from '../test-utils/welcome'
import welcomeZh from '../test-utils/locales/welcome/zh-CN.json'
import welcomeEn from '../test-utils/locales/welcome/en-US.json'

afterEach(() => {
  vi.restoreAllMocks()
})

describe('createI18n negotiation', () => {
  it('starts with the default language when every source is absent', () => {
    const { instance } = createTestI18n()
    expect(instance.language).toBe('zh-CN')
  })

  it('starts from the first matching navigator language', () => {
    const { instance } = createTestI18n({ navigatorLanguages: ['fr-FR', 'en-US'] })
    expect(instance.language).toBe('en-US')
  })

  it('lets the URL parameter outrank storage, profile and navigator', () => {
    const instance = createI18n({
      storage: new MemoryStorage(),
      navigatorLanguages: ['en-US'],
      profileLanguage: 'en-US',
      searchParams: new URLSearchParams('lang=zh-CN'),
    })
    expect(instance.language).toBe('zh-CN')
  })

  it('honors a custom URL parameter name and treats an empty value as absent', () => {
    const instance = createI18n({
      storage: new MemoryStorage(),
      navigatorLanguages: [],
      urlParameterName: 'locale',
      searchParams: new URLSearchParams('lang=en-US&locale='),
    })
    expect(instance.language).toBe('zh-CN')
    const other = createI18n({
      storage: new MemoryStorage(),
      navigatorLanguages: [],
      urlParameterName: 'locale',
      searchParams: new URLSearchParams('locale=en-US'),
    })
    expect(other.language).toBe('en-US')
  })

  it('starts from the persisted choice stored under the locale key', () => {
    const { instance } = createTestI18n({ storedLanguage: 'en-US' })
    expect(instance.language).toBe('en-US')
  })

  it('lets the profile language outrank the navigator (M1 extension point)', () => {
    const { instance } = createTestI18n({
      profileLanguage: 'zh-CN',
      navigatorLanguages: ['en-US'],
    })
    expect(instance.language).toBe('zh-CN')
  })

  it('matches tolerant spellings: underscores, casing, bare primary subtags', () => {
    const cases = [
      ['en_us', 'en-US'],
      ['EN-US', 'en-US'],
      ['zh', 'zh-CN'],
    ] as const
    for (const [raw, expected] of cases) {
      const instance = createI18n({
        storage: new MemoryStorage(),
        navigatorLanguages: [],
        searchParams: new URLSearchParams(`lang=${raw}`),
      })
      expect(instance.language, raw).toBe(expected)
    }
  })

  it('skips an unknown URL override instead of switching somewhere', () => {
    const { instance } = createTestI18n()
    const withUrl = createI18n({
      storage: new MemoryStorage(),
      navigatorLanguages: [],
      searchParams: new URLSearchParams('lang=fr-FR'),
    })
    expect(withUrl.language).toBe(instance.language)
  })

  it('supports a custom supported-language set (defaultLanguage stays a member)', () => {
    const instance = createI18n({
      supportedLanguages: ['zh-CN', 'en-US', 'fr-FR'],
      navigatorLanguages: ['fr-FR'],
      storage: new MemoryStorage(),
    })
    expect(instance.language).toBe('fr-FR')
  })

  it('runs without any storage when storage is explicitly null', () => {
    const instance = createI18n({ storage: null, navigatorLanguages: [] })
    expect(instance.language).toBe('zh-CN')
  })

  it('throws when the supported set is empty or the default is outside it', () => {
    expect(() => createI18n({ supportedLanguages: [] })).toThrow(
      /at least one supported language/,
    )
    expect(() => createI18n({ defaultLanguage: 'fr-FR' })).toThrow(
      /defaultLanguage "fr-FR" is not among the supported languages/,
    )
  })

  it('pins the discipline options on the underlying instance', () => {
    const instance = createI18n({ storage: new MemoryStorage(), navigatorLanguages: [] })
    expect(instance.options.fallbackLng).toBe(false)
    expect(instance.options.load).toBe('currentOnly')
    expect(instance.options.saveMissing).toBe(true)
    // i18next extends a pinned supportedLngs with its internal "cimode"
    // meta language at init; readSupportedLanguages filters it back out,
    // and the underlying option array reflects the runtime extension.
    expect(instance.options.supportedLngs).toEqual(['zh-CN', 'en-US', 'cimode'])
    expect(typeof instance.options.missingKeyHandler).toBe('function')
  })
})

describe('switchLanguage', () => {
  it('switches and persists the canonical tag under the locale key', async () => {
    const { instance, storage } = createTestI18n({ storedLanguage: null })
    expect(instance.language).toBe('zh-CN')
    await switchLanguage(instance, 'en_us')
    expect(instance.language).toBe('en-US')
    expect(storage.getItem(SPEED_LOCALE_STORAGE_KEY)).toBe('en-US')
  })

  it('refuses a target outside the supported set without switching', async () => {
    const { instance, storage } = createTestI18n({ storedLanguage: null })
    await expect(switchLanguage(instance, 'fr-FR')).rejects.toThrow(
      /cannot switch to "fr-FR"/,
    )
    expect(instance.language).toBe('zh-CN')
    expect(storage.getItem(SPEED_LOCALE_STORAGE_KEY)).toBeNull()
  })

  it('persists nothing when the switch is given an explicit null storage', async () => {
    const { instance, storage } = createTestI18n({ storedLanguage: null })
    await switchLanguage(instance, 'en-US', null)
    expect(instance.language).toBe('en-US')
    expect(storage.getItem(SPEED_LOCALE_STORAGE_KEY)).toBeNull()
  })

  it('persists to an explicit alternate storage instead of the bound one', async () => {
    const { instance, storage } = createTestI18n({ storedLanguage: null })
    const alternate = new MemoryStorage()
    await switchLanguage(instance, 'en-US', alternate)
    expect(alternate.getItem(SPEED_LOCALE_STORAGE_KEY)).toBe('en-US')
    expect(storage.getItem(SPEED_LOCALE_STORAGE_KEY)).toBeNull()
  })

  it('refuses a bare i18next instance that pins no supported set', async () => {
    const bare = createInstance()
    await expect(switchLanguage(bare, 'en-US')).rejects.toThrow(/createI18n/)
  })
})

describe('missing-key discipline (no silent fallback across languages)', () => {
  it('never renders another language text for a key missing in the loaded language', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const { instance, storage } = createTestI18n({ storedLanguage: null })
    // The loaded language starts with the key present...
    expect(instance.t('greeting.hello', { ns: 'welcome' })).toBe(
      welcomeZh.greeting.hello,
    )
    // ...then it goes missing in en-US only (zh-CN still has it, which is
    // exactly the trap a cross-language fallback would silently fall into).
    instance.removeResourceBundle('en-US', 'welcome')
    await switchLanguage(instance, 'en-US')
    expect(warn).not.toHaveBeenCalled()

    const rendered = instance.t('greeting.hello', { ns: 'welcome' })

    expect(rendered).toBe('greeting.hello')
    expect(rendered).not.toBe(welcomeZh.greeting.hello)
    expect(rendered).not.toBe(welcomeEn.greeting.hello)
    expect(warn).toHaveBeenCalledTimes(1)
    const message = warn.mock.calls[0]![0] as string
    expect(message).toContain('[speed-i18n]')
    expect(message).toContain('greeting.hello')
    expect(message).toContain('welcome')
    expect(message).toContain('en-US')
    expect(storage.getItem(SPEED_LOCALE_STORAGE_KEY)).toBe('en-US')
  })

  it('reports structured details to the host onMissingKey handler', async () => {
    const onMissingKey = vi.fn()
    const { instance } = createTestI18n({ onMissingKey })
    instance.removeResourceBundle('en-US', 'welcome')
    await switchLanguage(instance, 'en-US')
    instance.t('greeting.hello', { ns: 'welcome' })
    expect(onMissingKey).toHaveBeenCalledTimes(1)
    expect(onMissingKey.mock.calls[0]![0]).toEqual({
      languages: ['en-US'],
      namespace: 'welcome',
      key: 'greeting.hello',
    })
  })
})
