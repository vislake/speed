/**
 * @speed/i18n -- the platform i18n layer for web packages.
 *
 * One react-i18next-wrapped instance per host, negotiated start language,
 * per-package namespaces registered with parity and coverage validation,
 * and a missing-key discipline with no silent cross-language fallback
 * (mirroring go/pkgcore/i18n). See README.md for the design; the MUI
 * localization helper lives at @speed/i18n/mui-locale.
 */

export { createI18n, switchLanguage } from './create'
export { registerNamespace } from './register'
export {
  DEFAULT_LANGUAGE,
  DEFAULT_SUPPORTED_LANGUAGES,
  normalizeLanguageTag,
  matchSupportedLanguage,
} from './languages'
export { SPEED_LOCALE_STORAGE_KEY, type StorageLike } from './storage'
export {
  defaultMissingKeyHandler,
  type MissingKeyDetails,
} from './missing-key'
export type { CreateI18nOptions } from './create'
export type { ResourceBundle } from './register'
export type { i18n as I18nInstance } from 'i18next'
