/**
 * Contract tests for createI18n and switchLanguage: negotiation inputs and
 * precedence, the discipline options init pins, the opt-in fallback
 * namespace (namespace fallback, never language fallback), persistence
 * semantics, and the end-to-end proof that a missing key in the loaded
 * language never renders another language's text (the no-silent-fallback
 * analogue of go/pkgcore/i18n, whose catalog never falls back across
 * languages).
 *
 * CJK assertions compare against the imported fixtures; no language
 * literals live in this file.
 */

import { afterEach, describe, expect, it, vi } from 'vitest'
import { createInstance } from 'i18next'
import {
  createI18n,
  registerNamespace,
  switchLanguage,
  SPEED_LOCALE_STORAGE_KEY,
  type MissingKeyDetails,
  type ResourceBundle,
  type StorageLike,
} from './index'
import { MemoryStorage } from '../test-utils/memory-storage'
import { createTestI18n, registerWelcome } from '../test-utils/welcome'
import welcomeZh from '../test-utils/locales/welcome/zh-CN.json'
import welcomeEn from '../test-utils/locales/welcome/en-US.json'
import fallbackZh from '../test-utils/locales/fallback/zh-CN.json'
import fallbackEn from '../test-utils/locales/fallback/en-US.json'

/** Storage whose write always throws: persistence must stay best-effort. */
class ThrowingWriteStorage implements StorageLike {
  getItem(key: string): string | null {
    void key
    return null
  }

  setItem(key: string, value: string): void {
    throw new Error(`storage write denied for "${key}" = "${value}"`)
  }
}

/** Storage whose read always throws: creation must fall back to no choice. */
class ThrowingReadStorage implements StorageLike {
  getItem(key: string): string | null {
    throw new Error(`storage read denied for "${key}"`)
  }

  setItem(key: string, value: string): void {
    void key
    void value
  }
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('createI18n negotiation', () => {
  it('starts with the default language when every source is absent', () => {
    const { instance } = createTestI18n()
    expect(instance.language).toBe('en-US')
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
    expect(instance.language).toBe('en-US')
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

  it('skips the URL source entirely when urlParameterName is null', () => {
    // urlParameterName: null is the documented opt-out; it must not be
    // coalesced onto the default parameter name, or a ?lang= override on
    // the real URL would still outrank storage and the navigator.
    const storage = new MemoryStorage()
    storage.setItem(SPEED_LOCALE_STORAGE_KEY, 'en-US')
    const instance = createI18n({
      storage,
      urlParameterName: null,
      navigatorLanguages: ['en-US'],
      searchParams: new URLSearchParams('lang=zh-CN'),
    })
    expect(instance.language).toBe('en-US')
  })

  it('never honors a "null"-named or empty-named parameter when the URL source is opted out', () => {
    // URLSearchParams.get() coerces its argument and an empty name is a
    // legal (if pathological) parameter, so ?null=en-US and ?=en-US must
    // both stay inert once detection is disabled -- neither may select a
    // language the negotiation chain would otherwise not pick.
    for (const search of ['?null=en-US', '?=en-US']) {
      const instance = createI18n({
        storage: new MemoryStorage(),
        urlParameterName: null,
        navigatorLanguages: [],
        searchParams: new URLSearchParams(search),
      })
      expect(instance.language, search).toBe('en-US')
    }
    const emptyName = createI18n({
      storage: new MemoryStorage(),
      urlParameterName: '',
      navigatorLanguages: [],
      searchParams: new URLSearchParams('?=en-US'),
    })
    expect(emptyName.language).toBe('en-US')
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
    expect(instance.language).toBe('en-US')
  })

  it('falls back to no stored choice when the storage read throws', () => {
    const instance = createI18n({
      storage: new ThrowingReadStorage(),
      navigatorLanguages: [],
      searchParams: new URLSearchParams(),
    })
    expect(instance.language).toBe('en-US')
  })

  it('throws when the supported set is empty or the default is outside it', () => {
    expect(() => createI18n({ supportedLanguages: [] })).toThrow(
      /at least one supported language/,
    )
    expect(() => createI18n({ defaultLanguage: 'fr-FR' })).toThrow(
      /defaultLanguage "fr-FR" is not among the supported languages/,
    )
  })

  it('refuses a non-canonical supported-language entry, naming its canonical form', () => {
    expect(() => createI18n({ supportedLanguages: ['zh-CN', 'en_US'] })).toThrow(
      /supported language "en_US" is not in canonical form; use "en-US"/,
    )
    expect(() => createI18n({ supportedLanguages: ['en-US ', 'zh-CN'] })).toThrow(
      /supported language "en-US " is not in canonical form; use "en-US"/,
    )
    expect(() => createI18n({ supportedLanguages: ['zh-CN', 'e!'] })).toThrow(
      /supported language "e!" is not a valid language tag/,
    )
  })

  it('negotiates a subtagged request onto a supported bare tag of the same language', () => {
    const fromNavigator = createI18n({
      supportedLanguages: ['zh-CN', 'ja'],
      defaultLanguage: 'zh-CN',
      storage: new MemoryStorage(),
      navigatorLanguages: ['ja-JP'],
    })
    expect(fromNavigator.language).toBe('ja')
    const fromUrl = createI18n({
      supportedLanguages: ['zh-CN', 'ja'],
      defaultLanguage: 'zh-CN',
      storage: new MemoryStorage(),
      navigatorLanguages: [],
      searchParams: new URLSearchParams('lang=ja-JP'),
    })
    expect(fromUrl.language).toBe('ja')
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

  // i18next's own default is no fallback namespace; an unset option must
  // stay indistinguishable from one that never existed.
  it('configures no fallback namespace when fallbackNamespaces is undefined', () => {
    const instance = createI18n({ storage: new MemoryStorage(), navigatorLanguages: [] })
    expect(instance.options.fallbackNS).toBe(false)
  })
})

describe('fallbackNamespaces (namespace fallback, never language fallback)', () => {
  const FALLBACK_NAMESPACE = 'fallback-fixture'
  const FALLBACK_ONLY_KEY = 'errors.demo.fallback_only'
  const fixtureTexts = {
    'zh-CN': (fallbackZh as { errors: Record<string, string> }).errors[
      'demo.fallback_only'
    ],
    'en-US': (fallbackEn as { errors: Record<string, string> }).errors[
      'demo.fallback_only'
    ],
  }
  const fallbackFixtureResources: Readonly<Record<string, ResourceBundle>> = {
    'zh-CN': fallbackZh as unknown as ResourceBundle,
    'en-US': fallbackEn as unknown as ResourceBundle,
  }

  function createFallbackI18n(
    options: { readonly storedLanguage?: string; readonly onMissingKey?: (details: MissingKeyDetails) => void } = {},
  ) {
    const storage = new MemoryStorage()
    if (options.storedLanguage !== undefined) {
      storage.setItem(SPEED_LOCALE_STORAGE_KEY, options.storedLanguage)
    }
    const instance = createI18n({
      storage,
      navigatorLanguages: [],
      onMissingKey: options.onMissingKey,
      fallbackNamespaces: [FALLBACK_NAMESPACE],
    })
    registerWelcome(instance)
    registerNamespace(instance, FALLBACK_NAMESPACE, fallbackFixtureResources)
    return instance
  }

  it('passes the namespaces through to i18next as fallbackNS', () => {
    expect(createFallbackI18n().options.fallbackNS).toEqual([FALLBACK_NAMESPACE])
  })

  it('resolves a key the called namespace lacks through the fallback namespace', () => {
    const instance = createFallbackI18n()
    expect(instance.t(`welcome:${FALLBACK_ONLY_KEY}`)).toBe(fixtureTexts['en-US'])
    expect(instance.exists(`welcome:${FALLBACK_ONLY_KEY}`)).toBe(true)
  })

  it('resolves the fallback in the current language, the same as any namespace', () => {
    const instance = createFallbackI18n({ storedLanguage: 'zh-CN' })
    expect(instance.language).toBe('zh-CN')
    expect(instance.t(`welcome:${FALLBACK_ONLY_KEY}`)).toBe(fixtureTexts['zh-CN'])
  })

  it('lets the called namespace win when both carry the key', () => {
    const instance = createFallbackI18n()
    const ownText = 'The called namespace keeps its own words.'
    registerNamespace(instance, 'own-holder', {
      'zh-CN': { errors: { 'demo.fallback_only': ownText } },
      'en-US': { errors: { 'demo.fallback_only': ownText } },
    })
    expect(instance.t(`own-holder:${FALLBACK_ONLY_KEY}`)).toBe(ownText)
  })

  it('does not fire the missing-key handler for a key the fallback namespace answers', () => {
    const onMissingKey = vi.fn()
    const instance = createFallbackI18n({ onMissingKey })
    expect(instance.t(`welcome:${FALLBACK_ONLY_KEY}`)).toBe(fixtureTexts['en-US'])
    expect(onMissingKey).not.toHaveBeenCalled()
  })

  it('still renders the key and fires the handler when no consulted namespace answers', () => {
    const onMissingKey = vi.fn()
    const instance = createFallbackI18n({ onMissingKey })
    const nowhereKey = 'errors.demo.nowhere'
    expect(instance.t(`welcome:${nowhereKey}`)).toBe(nowhereKey)
    expect(onMissingKey).toHaveBeenCalledTimes(1)
    expect(onMissingKey.mock.calls[0]?.[0]).toMatchObject({
      key: nowhereKey,
      namespace: 'welcome',
    })
  })
})

describe('switchLanguage', () => {
  it('switches and persists the canonical tag under the locale key', async () => {
    const { instance, storage } = createTestI18n({ storedLanguage: null })
    expect(instance.language).toBe('en-US')
    await switchLanguage(instance, 'zh_cn')
    expect(instance.language).toBe('zh-CN')
    expect(storage.getItem(SPEED_LOCALE_STORAGE_KEY)).toBe('zh-CN')
  })

  it('refuses a target outside the supported set without switching', async () => {
    const { instance, storage } = createTestI18n({ storedLanguage: null })
    await expect(switchLanguage(instance, 'fr-FR')).rejects.toThrow(
      /cannot switch to "fr-FR"/,
    )
    expect(instance.language).toBe('en-US')
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

  it('switches onto a supported bare tag from a subtagged request', async () => {
    const storage = new MemoryStorage()
    const instance = createI18n({
      supportedLanguages: ['zh-CN', 'ja'],
      defaultLanguage: 'zh-CN',
      storage,
      navigatorLanguages: [],
    })
    await switchLanguage(instance, 'ja-JP')
    expect(instance.language).toBe('ja')
    expect(storage.getItem(SPEED_LOCALE_STORAGE_KEY)).toBe('ja')
  })

  it('still completes the switch when persisting the choice fails', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const instance = createI18n({
      storage: new ThrowingWriteStorage(),
      navigatorLanguages: [],
    })
    await expect(switchLanguage(instance, 'en-US')).resolves.toBeUndefined()
    expect(instance.language).toBe('en-US')
    expect(warn).toHaveBeenCalledTimes(1)
    const message = warn.mock.calls[0]![0] as string
    expect(message).toContain('[speed-i18n]')
    expect(message).toContain('en-US')
    expect(message).toContain(SPEED_LOCALE_STORAGE_KEY)
  })
})

describe('profile-language tier under late resolution', () => {
  it('applies a late-resolved profile without persisting, so a later profile change still wins on the next visit', async () => {
    // The host resolves the profile locale only after creation (it comes
    // from /me), so it applies it through switchLanguage -- and must use
    // the non-persisting form, because the persisted slot is the
    // manual-choice tier, which outranks the profile tier on every later
    // visit.
    const storage = new MemoryStorage()
    const visitOne = createI18n({ storage, navigatorLanguages: [] })
    expect(visitOne.language).toBe('en-US')
    await switchLanguage(visitOne, 'zh-CN', null)
    expect(visitOne.language).toBe('zh-CN')
    // The manual-choice slot stays untouched: no stale trace to shadow the
    // tier when the profile changes.
    expect(storage.getItem(SPEED_LOCALE_STORAGE_KEY)).toBeNull()

    // The profile changes on the server; the next visit feeds the current
    // profile at creation and the profile tier speaks.
    const visitTwo = createI18n({
      storage,
      profileLanguage: 'zh-CN',
      navigatorLanguages: [],
    })
    expect(visitTwo.language).toBe('zh-CN')
  })

  it('persisting a profile application writes the manual slot, which outranks the profile tier on later visits', async () => {
    // The trap the non-persisting recipe exists to prevent: applying the
    // profile through switchLanguage's default writes the manual-choice
    // slot, and the stored choice outranks the profile tier by design -- so
    // a later profile change is shadowed in this browser until the manual
    // slot is overwritten or cleared.
    const storage = new MemoryStorage()
    const visitOne = createI18n({ storage, navigatorLanguages: [] })
    await switchLanguage(visitOne, 'en-US')
    expect(storage.getItem(SPEED_LOCALE_STORAGE_KEY)).toBe('en-US')

    // The profile changes to zh-CN, but the stored manual choice wins at
    // the next visit's negotiation.
    const visitTwo = createI18n({
      storage,
      profileLanguage: 'zh-CN',
      navigatorLanguages: [],
    })
    expect(visitTwo.language).toBe('en-US')
  })
})

describe('missing-key discipline (no silent fallback across languages)', () => {
  it('never renders another language text for a key missing in the loaded language', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const { instance, storage } = createTestI18n({ storedLanguage: null })
    // The loaded language starts with the key present...
    expect(instance.t('greeting.hello', { ns: 'welcome' })).toBe(
      welcomeEn.greeting.hello,
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
