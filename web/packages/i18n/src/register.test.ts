/**
 * Contract tests for registerNamespace: validation happens before any
 * mutation (failed registrations leave the instance untouched), parity and
 * coverage are enforced across the supported languages, and a namespace
 * registers exactly once per instance.
 */

import { describe, expect, it } from 'vitest'
import { createInstance } from 'i18next'
import { createI18n, registerNamespace, type ResourceBundle } from './index'
import { MemoryStorage } from '../test-utils/memory-storage'
import {
  registerWelcome,
  welcomeResources,
} from '../test-utils/welcome'
import welcomeZh from '../test-utils/locales/welcome/zh-CN.json'
import welcomeEn from '../test-utils/locales/welcome/en-US.json'

function bareInstance(): ReturnType<typeof createInstance> {
  return createInstance()
}

function instanceWithSupported(
  supported: readonly string[],
): ReturnType<typeof createInstance> {
  const instance = bareInstance()
  void instance.init({ supportedLngs: [...supported], lng: supported[0] })
  return instance
}

/** The welcome resources minus one en-US leaf (parity must reject it). */
function enUsMissingProfile(): Record<string, ResourceBundle> {
  const enUs = JSON.parse(JSON.stringify(welcomeResources['en-US'])) as Record<
    string,
    unknown
  >
  delete (enUs.greeting as Record<string, unknown>).profile
  return {
    'zh-CN': welcomeResources['zh-CN']!,
    'en-US': enUs as ResourceBundle,
  }
}

describe('registerNamespace', () => {
  it('registers a bilingual namespace and makes it translatable in both languages', async () => {
    const instance = createI18n({ storage: new MemoryStorage(), navigatorLanguages: [] })
    registerWelcome(instance)
    expect(instance.t('greeting.hello', { ns: 'welcome', lng: 'zh-CN' })).toBe(
      welcomeZh.greeting.hello,
    )
    expect(instance.t('greeting.hello', { ns: 'welcome', lng: 'en-US' })).toBe(
      welcomeEn.greeting.hello,
    )
    expect(instance.t('common.save', { ns: 'welcome', lng: 'zh-CN' })).toBe(
      welcomeZh.common.save,
    )
  })

  it('registers several namespaces on one instance, each under its own name', () => {
    const instance = createI18n({ storage: new MemoryStorage(), navigatorLanguages: [] })
    registerNamespace(instance, 'welcome', welcomeResources)
    registerNamespace(instance, 'goodbye', {
      'zh-CN': { bye: welcomeZh.greeting.hello },
      'en-US': { bye: welcomeEn.greeting.hello },
    })
    expect(instance.t('bye', { ns: 'goodbye', lng: 'en-US' })).toBe(
      welcomeEn.greeting.hello,
    )
    expect(instance.t('greeting.hello', { ns: 'welcome', lng: 'zh-CN' })).toBe(
      welcomeZh.greeting.hello,
    )
  })

  it('refuses to register the same namespace twice on the same instance', () => {
    const instance = createI18n({ storage: new MemoryStorage(), navigatorLanguages: [] })
    registerWelcome(instance)
    expect(() => registerNamespace(instance, 'welcome', welcomeResources)).toThrow(
      /already registered on this instance/,
    )
  })

  it('refuses a bare i18next instance that pins no supported set', () => {
    expect(() => registerNamespace(bareInstance(), 'welcome', welcomeResources)).toThrow(
      /createI18n/,
    )
  })

  it('refuses invalid namespace names', () => {
    const instance = instanceWithSupported(['zh-CN'])
    // Scoped npm names ('@speed/tokens') are not namespaces: a package
    // registers under its base name, and the pattern admits no '@' or '/'.
    for (const name of ['1welcome', 'welcome space', 'weiß', '@speed/tokens']) {
      expect(
        () => registerNamespace(instance, name, { 'zh-CN': { a: 'x' } }),
        name,
      ).toThrow(/not a valid namespace/)
    }
  })

  it('refuses resources without any language', () => {
    const instance = instanceWithSupported(['zh-CN'])
    expect(() => registerNamespace(instance, 'welcome', {})).toThrow(
      /at least one language/,
    )
  })

  it('refuses languages outside the instance supported set', () => {
    const instance = instanceWithSupported(['zh-CN', 'en-US'])
    const withFr = {
      ...welcomeResources,
      'fr-FR': welcomeResources['en-US']!,
    }
    expect(() => registerNamespace(instance, 'welcome', withFr)).toThrow(
      /\[fr-FR\] are not among the instance's supported languages \[zh-CN, en-US\]/,
    )
  })

  it('requires coverage of every supported language', () => {
    const instance = instanceWithSupported(['zh-CN', 'en-US'])
    expect(() =>
      registerNamespace(instance, 'welcome', { 'zh-CN': welcomeResources['zh-CN']! }),
    ).toThrow(/missing bundle\(s\) for \[en-US\]/)
  })

  it('rejects a language bundle whose key set differs from the reference language', () => {
    const instance = instanceWithSupported(['zh-CN', 'en-US'])
    expect(() => registerNamespace(instance, 'welcome', enUsMissingProfile())).toThrow(
      /missing in "en-US" relative to "zh-CN": "greeting\.profile"/,
    )
  })

  it('rejects an extra key in one language as loudly as a missing one', () => {
    const instance = instanceWithSupported(['zh-CN', 'en-US'])
    const enUs = JSON.parse(JSON.stringify(welcomeResources['en-US'])) as Record<
      string,
      unknown
    >
    ;(enUs.greeting as Record<string, unknown>).extra = 'Extra'
    expect(() =>
      registerNamespace(instance, 'welcome', {
        'zh-CN': welcomeResources['zh-CN']!,
        'en-US': enUs as ResourceBundle,
      }),
    ).toThrow(/present in "en-US" but not in "zh-CN": "greeting\.extra"/)
  })

  it('rejects non-string leaves by naming their path', () => {
    const instance = instanceWithSupported(['zh-CN'])
    expect(() =>
      registerNamespace(instance, 'welcome', {
        'zh-CN': { greeting: { hello: 42 } as unknown as ResourceBundle },
      }),
    ).toThrow(/"greeting\.hello" is a number/)
  })

  it('rejects a bundle that ships no keys at all', () => {
    const instance = instanceWithSupported(['zh-CN'])
    expect(() =>
      registerNamespace(instance, 'welcome', {
        'zh-CN': { greeting: {} },
      }),
    ).toThrow(/"zh-CN" bundle ships no translation keys/)
  })

  it('leaves the instance untouched when validation fails (atomicity)', () => {
    const instance = createI18n({ storage: new MemoryStorage(), navigatorLanguages: [] })
    expect(() => registerNamespace(instance, 'welcome', enUsMissingProfile())).toThrow()
    expect(instance.hasResourceBundle('zh-CN', 'welcome')).toBe(false)
    expect(instance.hasResourceBundle('en-US', 'welcome')).toBe(false)
    expect(instance.t('greeting.hello', { ns: 'welcome', lng: 'zh-CN' })).toBe(
      'greeting.hello',
    )
  })
})
