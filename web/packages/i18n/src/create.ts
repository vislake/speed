/**
 * Instance creation and the language switch.
 *
 * createI18n is the single way to obtain an i18n instance in this package.
 * It runs the negotiation chain from languages.ts against the supported
 * set, then configures the i18next instance so that the missing-key
 * discipline holds by construction:
 *
 *  - supportedLngs pins the language set; fallbackLng: false and load:
 *    "currentOnly" make cross-language fallback impossible; a missing key
 *    resolves to the key itself.
 *  - saveMissing: true is required for i18next v26 to dispatch its
 *    missingKeyHandler at all; the handler is always installed (the host's
 *    onMissingKey, or the package's visible default warning), so a missing
 *    key is never silent.
 *  - initAsync: false keeps init synchronous (v26 defers by default): the
 *    negotiated language is decided and the instance is ready by the time
 *    createI18n returns, which tests and SSR-rendered first paint rely on.
 *
 * Every negotiable input is injectable (searchParams, storage,
 * navigatorLanguages) and the DOM is only touched through guarded reads, so
 * tests and non-browser hosts stay deterministic.
 */

import { createInstance, type i18n as I18nInstance } from 'i18next'
import { initReactI18next } from 'react-i18next'
import {
  DEFAULT_LANGUAGE,
  DEFAULT_SUPPORTED_LANGUAGES,
  detectLanguage,
  matchSupportedLanguage,
  readSupportedLanguages,
} from './languages'
import {
  SPEED_LOCALE_STORAGE_KEY,
  bindInstanceStorage,
  boundInstanceStorage,
  type StorageLike,
} from './storage'
import { missingKeyHandlerFactory, type MissingKeyDetails } from './missing-key'

/** Customize the language the instance starts with, or how it is decided. */
export interface CreateI18nOptions {
  /**
   * Canonical tags the instance may speak. Defaults to
   * DEFAULT_SUPPORTED_LANGUAGES (zh-CN, en-US). Every later registration
   * must cover this whole set, so keep it in lockstep with the language
   * resources the platform actually ships.
   */
  readonly supportedLanguages?: readonly string[]
  /**
   * Negotiation fallback when every source misses; must be a member of
   * supportedLanguages. Defaults to DEFAULT_LANGUAGE (zh-CN). An unknown
   * browser language resolves here deliberately -- see README's "Language
   * negotiation" -- but a missing translation key never falls back across
   * languages (see "Missing keys").
   */
  readonly defaultLanguage?: string
  /**
   * URL parameter carrying an explicit language override. Defaults to
   * "lang". Pass null or an empty string (not undefined) to disable
   * reading the URL. When searchParams is not injected, the current
   * location's query string is read through a guarded access, so absence
   * of a DOM never throws.
   */
  readonly urlParameterName?: string | null
  /** Injected URL parameters (tests, non-browser hosts). */
  readonly searchParams?: URLSearchParams | null
  /**
   * Storage the chosen language persists to and is read from. Defaults to
   * the browser's localStorage through a guarded access (null in Node).
   * Pass null explicitly to run without persistence.
   */
  readonly storage?: StorageLike | null
  /** Override the key the choice is persisted under. Defaults to SPEED_LOCALE_STORAGE_KEY. */
  readonly storageKey?: string
  /**
   * Signed-in user's profile locale, resolved by the host before instance
   * creation. M0 has no profile feature yet: this is the documented
   * extension point the M1 user-profile step feeds -- hosts that can
   * resolve a profile locale pass it here and it outranks the browser.
   */
  readonly profileLanguage?: string | null
  /**
   * Browser language preferences. Defaults to the global navigator
   * languages through a guarded read. Pass an explicit list (or []) in
   * tests and other deterministic contexts.
   */
  readonly navigatorLanguages?: readonly string[]
  /**
   * Called with details whenever a translation key is missing. Defaults to
   * the package's visible console warning (defaultMissingKeyHandler).
   */
  readonly onMissingKey?: (details: MissingKeyDetails) => void
}

function readUrlLanguage(
  parameterName: string,
  injected: URLSearchParams | null | undefined,
): string | null {
  let params = injected
  if (params === undefined) {
    const globalWithLocation = globalThis as { location?: { search?: string } }
    if (typeof globalWithLocation.location?.search === 'string') {
      try {
        params = new URLSearchParams(globalWithLocation.location.search)
      } catch {
        params = null
      }
    } else {
      params = null
    }
  }
  if (params === null) {
    return null
  }
  const value = params.get(parameterName)
  return value === null || value === '' ? null : value
}

function readGlobalLocalStorage(): StorageLike | null {
  const globalWithStorage = globalThis as { localStorage?: unknown }
  let candidate: unknown
  try {
    candidate = globalWithStorage.localStorage
  } catch {
    return null
  }
  if (typeof candidate !== 'object' || candidate === null) {
    return null
  }
  const storage = candidate as Partial<StorageLike>
  if (typeof storage.getItem === 'function' && typeof storage.setItem === 'function') {
    return storage as StorageLike
  }
  return null
}

function readGlobalNavigatorLanguages(): readonly string[] {
  const globalWithNavigator = globalThis as { navigator?: { languages?: unknown } }
  const raw = globalWithNavigator.navigator?.languages
  if (!Array.isArray(raw)) {
    return []
  }
  return raw.filter((entry): entry is string => typeof entry === 'string')
}

/**
 * Create the host i18n instance: negotiate the start language, pin the
 * supported set with the discipline options above, bind persistence, and
 * return a react-i18next-ready instance (initReactI18next is installed, so
 * the returned instance is what I18nextProvider and useTranslation expect).
 */
export function createI18n(options: CreateI18nOptions = {}): I18nInstance {
  const supportedLanguages = options.supportedLanguages ?? DEFAULT_SUPPORTED_LANGUAGES
  if (supportedLanguages.length === 0) {
    throw new Error(
      '[speed-i18n] createI18n requires at least one supported language; ' +
        'supportedLanguages must not be empty.',
    )
  }
  const defaultLanguage = options.defaultLanguage ?? DEFAULT_LANGUAGE
  const defaultMatched = matchSupportedLanguage(defaultLanguage, supportedLanguages)
  if (defaultMatched === null) {
    throw new Error(
      `[speed-i18n] defaultLanguage "${defaultLanguage}" is not among the supported ` +
        `languages [${supportedLanguages.join(', ')}]; the negotiation fallback must be a ` +
        'member of the supported set.',
    )
  }
  const storage =
    options.storage === undefined ? readGlobalLocalStorage() : options.storage
  const storageKey = options.storageKey ?? SPEED_LOCALE_STORAGE_KEY
  const storedLanguage =
    storage === null ? null : storage.getItem(storageKey)

  // undefined means the default parameter name "lang"; null or an empty
  // string opts the URL source out of the negotiation chain entirely. The
  // opt-out must not be coalesced onto "lang" (the ?? would turn
  // urlParameterName: null into an active ?lang= reader), and it must not
  // reach readUrlLanguage either -- URLSearchParams.get() coerces its
  // argument, so a raw null would honor a literal ?null=... parameter.
  const urlLanguage =
    options.urlParameterName === null || options.urlParameterName === ''
      ? null
      : readUrlLanguage(options.urlParameterName ?? 'lang', options.searchParams)

  const detected = detectLanguage({
    supportedLanguages,
    defaultLanguage: defaultMatched,
    urlLanguage,
    storedLanguage,
    profileLanguage: options.profileLanguage ?? null,
    navigatorLanguages: options.navigatorLanguages ?? readGlobalNavigatorLanguages(),
  })

  const instance = createInstance()
  instance.use(initReactI18next)
  // The missing-key discipline must hold without the host asking for it:
  // saveMissing gates i18next v26's missingKeyHandler dispatch, so it stays
  // on; with our handler always installed, no backend saveMissing path can
  // engage (this package registers in-memory resources only).
  void instance.init({
    lng: detected,
    supportedLngs: [...supportedLanguages],
    fallbackLng: false,
    load: 'currentOnly',
    saveMissing: true,
    missingKeyHandler: missingKeyHandlerFactory(options.onMissingKey),
    initAsync: false,
  })
  bindInstanceStorage(instance, storage, storageKey)
  return instance
}

/**
 * Persist the choice when it came from the manual override UI. The target
 * language must be a member of the instance's supported set; anything else
 * throws instead of silently switching somewhere.
 *
 * The optional storage argument is three-state: undefined uses the storage
 * bound at creation, null persists nothing, and a StorageLike persists to
 * that store instead. Reads and writes always go through the binding's key.
 */
export async function switchLanguage(
  instance: I18nInstance,
  language: string,
  storage?: StorageLike | null,
): Promise<void> {
  const supported = readSupportedLanguages(instance)
  if (supported.length === 0) {
    throw new Error(
      '[speed-i18n] switchLanguage needs the instance to pin a supported-language set; ' +
        'create the instance with createI18n (bare i18next instances are refused).',
    )
  }
  const target = matchSupportedLanguage(language, supported)
  if (target === null) {
    throw new Error(
      `[speed-i18n] cannot switch to "${language}": it is not among the supported ` +
        `languages [${supported.join(', ')}].`,
    )
  }
  await instance.changeLanguage(target)
  const binding = boundInstanceStorage(instance)
  const effectiveStorage =
    storage === undefined ? (binding?.storage ?? null) : storage
  if (effectiveStorage !== null) {
    effectiveStorage.setItem(binding?.key ?? SPEED_LOCALE_STORAGE_KEY, target)
  }
}
