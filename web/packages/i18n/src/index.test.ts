/**
 * Entry-point contract: exactly the documented runtime exports, nothing
 * more -- in particular no MUI surface leaks into the main entry (that
 * lives at @speed/i18n/mui-locale, isolated so non-MUI consumers never
 * pull the MUI tree). React bindings are part of the surface by design
 * (see index.ts), kept so component packages and hosts consume the whole
 * i18n layer through one @speed package.
 */

import { describe, expect, it } from 'vitest'
import * as i18nModule from './index'

describe('@speed/i18n main entry', () => {
  it('exports exactly the documented runtime surface', () => {
    expect(Object.keys(i18nModule).sort()).toEqual([
      'CLIENT_TRANSPORT_ERROR_CODES',
      'DEFAULT_LANGUAGE',
      'DEFAULT_SUPPORTED_LANGUAGES',
      'I18nextProvider',
      'SESSION_LIFECYCLE_ERROR_CODES',
      'SPEED_LOCALE_STORAGE_KEY',
      'createErrorTextResolver',
      'createI18n',
      'defaultMissingKeyHandler',
      'matchSupportedLanguage',
      'normalizeLanguageTag',
      'readSupportedLanguages',
      'registerNamespace',
      'switchLanguage',
      'useTranslation',
    ])
  })

  it('exports the canonical constants and callables', () => {
    expect(i18nModule.DEFAULT_SUPPORTED_LANGUAGES).toEqual(['zh-CN', 'en-US'])
    expect(i18nModule.DEFAULT_LANGUAGE).toBe('en-US')
    expect(typeof i18nModule.readSupportedLanguages).toBe('function')
    expect(typeof i18nModule.createI18n).toBe('function')
    expect(typeof i18nModule.switchLanguage).toBe('function')
    expect(typeof i18nModule.registerNamespace).toBe('function')
    expect(typeof i18nModule.normalizeLanguageTag).toBe('function')
    expect(typeof i18nModule.matchSupportedLanguage).toBe('function')
    expect(typeof i18nModule.defaultMissingKeyHandler).toBe('function')
    expect(typeof i18nModule.useTranslation).toBe('function')
    expect(typeof i18nModule.createErrorTextResolver).toBe('function')
    expect(i18nModule.SESSION_LIFECYCLE_ERROR_CODES).toEqual([
      'authn.session_not_found',
      'authn.session_revoked',
      'authn.token_expired',
      'authn.refresh_token_invalid',
      'authn.refresh_token_reused',
    ])
    expect(i18nModule.CLIENT_TRANSPORT_ERROR_CODES).toEqual([
      'client.network',
      'client.timeout',
      'client.protocol',
    ])
  })

  it('exposes the I18nextProvider react binding', () => {
    expect(typeof i18nModule.I18nextProvider).toBe('function')
  })

  it('keeps the MUI locale helper out of the main entry', () => {
    expect(Object.keys(i18nModule)).not.toContain('muiLocaleFor')
  })
})
