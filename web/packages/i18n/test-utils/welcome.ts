/**
 * Shared test fixtures and instance factory for the i18n tests: a
 * deterministic createI18n (injected memory storage, no navigator
 * dependence) with the bilingual "welcome" namespace registered. The
 * bilingual resources live under test-utils/locales/ -- the CJK-bearing
 * files stay in a directory the repo's CJK scanner exempts; .ts sources
 * assert against imported fixtures instead of embedding literals.
 */

import {
  createI18n,
  registerNamespace,
  SPEED_LOCALE_STORAGE_KEY,
  type I18nInstance,
  type MissingKeyDetails,
  type ResourceBundle,
} from '../src/index'
import { MemoryStorage } from './memory-storage'
import welcomeEn from './locales/welcome/en-US.json'
import welcomeZh from './locales/welcome/zh-CN.json'

/** The bilingual welcome namespace, as registerNamespace expects it. */
export const welcomeResources: Readonly<Record<string, ResourceBundle>> = {
  'zh-CN': welcomeZh as unknown as ResourceBundle,
  'en-US': welcomeEn as unknown as ResourceBundle,
}

/** Register the welcome namespace on an instance (throws on duplicate). */
export function registerWelcome(instance: I18nInstance): void {
  registerNamespace(instance, 'welcome', welcomeResources)
}

export interface CreateTestI18nOptions {
  /** Pre-seed the memory storage with a persisted choice. */
  readonly storedLanguage?: string | null
  /** Defaults to [] so no test depends on the host's navigator. */
  readonly navigatorLanguages?: readonly string[]
  readonly profileLanguage?: string | null
  readonly onMissingKey?: (details: MissingKeyDetails) => void
}

/**
 * An instance with the welcome namespace registered, backed by an
 * assertable MemoryStorage. Language sources not mentioned default to
 * absent (no URL, no profile, no navigator languages).
 */
export function createTestI18n(
  options: CreateTestI18nOptions = {},
): { instance: I18nInstance; storage: MemoryStorage } {
  const storage = new MemoryStorage()
  if (options.storedLanguage !== null && options.storedLanguage !== undefined) {
    storage.setItem(SPEED_LOCALE_STORAGE_KEY, options.storedLanguage)
  }
  const instance = createI18n({
    storage,
    navigatorLanguages: options.navigatorLanguages ?? [],
    profileLanguage: options.profileLanguage ?? null,
    onMissingKey: options.onMissingKey,
  })
  registerWelcome(instance)
  return { instance, storage }
}
