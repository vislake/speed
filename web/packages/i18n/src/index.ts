/**
 * @speed/i18n -- the platform i18n layer for web packages.
 *
 * One react-i18next-wrapped instance per host, negotiated start language,
 * per-package namespaces registered with parity and coverage validation,
 * and a missing-key discipline with no silent cross-language fallback
 * (mirroring go/pkgcore/i18n). See README.md for the design; the MUI
 * localization helper lives at @speed/i18n/mui-locale.
 *
 * The react bindings (I18nextProvider, useTranslation) are re-exported
 * here so component packages and hosts consume the whole i18n surface
 * through one @speed package; the module identity is pinned by lockstep
 * single-version shipping, so a consumer can never hold two react-i18next
 * instances from two copies of the library.
 */

export { createI18n, switchLanguage } from './create.js'
export { registerNamespace } from './register.js'
export {
  DEFAULT_LANGUAGE,
  DEFAULT_SUPPORTED_LANGUAGES,
  normalizeLanguageTag,
  matchSupportedLanguage,
} from './languages.js'
export { SPEED_LOCALE_STORAGE_KEY, type StorageLike } from './storage.js'
export {
  defaultMissingKeyHandler,
  type MissingKeyDetails,
} from './missing-key.js'
export type { CreateI18nOptions } from './create.js'
export type { ResourceBundle } from './register.js'
export type { i18n as I18nInstance } from 'i18next'
export { I18nextProvider, useTranslation } from 'react-i18next'
export type { UseTranslationResponse } from 'react-i18next'
