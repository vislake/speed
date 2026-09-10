/**
 * The platform error-copy bundle and its registration.
 *
 * The bundle is the client-side text for every backend apperr code whose
 * module publishes a catalog entry, generated mechanically from the same
 * locales/*.toml catalogs the backend renders from
 * (tools/gen_platform_error_bundle.py); it is not hand-maintained, and
 * backend-only content (invitation emails, notification templates, SMS
 * bodies, seed copy) has no code to be looked up with and is therefore
 * absent by construction. Keys are the `errors` section the packages'
 * resolvers already navigate, one flat dotted apperr code per leaf
 * (`errors.authn.invalid_credentials`), so `t('errors.' + code)` resolves
 * through i18next's own deepFind without a lookup helper.
 *
 * Isolated in its own subpath so the main entry's export table stays
 * exactly as documented (the ./mui-locale precedent: a consumer that
 * never resolves a backend code never pulls this bundle). A host
 * registers it once at bootstrap and names it as the instance's fallback
 * namespace, which is what lets a code a package's own namespace does
 * not cover resolve to the module catalog's words instead of the raw
 * key:
 *
 *   const i18n = createI18n({ fallbackNamespaces: [PLATFORM_ERRORS_NAMESPACE] })
 *   registerPlatformErrors(i18n)
 */

import type { i18n as I18nInstance } from 'i18next'
import { readSupportedLanguages } from './languages.js'
import { registerNamespace, type ResourceBundle } from './register.js'
import enUS from './platform-errors/locales/en-US.json' with { type: 'json' }
import zhCN from './platform-errors/locales/zh-CN.json' with { type: 'json' }

/** The namespace the platform error bundle registers under. */
export const PLATFORM_ERRORS_NAMESPACE = 'platform-errors' as const

/** The generated bundles, one per language the generator ships. */
const PLATFORM_ERROR_RESOURCES: Readonly<Record<string, ResourceBundle>> = {
  'zh-CN': zhCN as ResourceBundle,
  'en-US': enUS as ResourceBundle,
}

/**
 * Register the platform error bundle on the instance.
 *
 * Registration goes through registerNamespace, so the bundle's
 * validation is the package's own: both languages, identical leaf sets,
 * non-empty strings. A supported language the bundle has no file for is
 * reported with the fix inline instead of registering a bundle that
 * would force the render-as-key path on that language's users -- the
 * generator's language pair is fixed, so a new platform language is a
 * generator (and catalog) change, not a silent partial registration.
 */
export function registerPlatformErrors(instance: I18nInstance): void {
  const supported = readSupportedLanguages(instance)
  if (supported.length === 0) {
    throw new Error(
      '[speed-i18n] registerPlatformErrors needs the instance to pin a ' +
        'supported-language set; create the instance with createI18n (bare ' +
        'i18next instances are refused).',
    )
  }
  const missing = supported.filter(
    (language) => !(language in PLATFORM_ERROR_RESOURCES),
  )
  if (missing.length > 0) {
    throw new Error(
      `[speed-i18n] the platform error bundle has no resources for ` +
        `[${missing.join(', ')}]; the bundle ships ` +
        `[${Object.keys(PLATFORM_ERROR_RESOURCES).join(', ')}]. Extend ` +
        'tools/gen_platform_error_bundle.py and the backend modules\' locale ' +
        'catalogs before adding the language, then regenerate the bundle.',
    )
  }
  const resources: Record<string, ResourceBundle> = {}
  for (const language of supported) {
    resources[language] = PLATFORM_ERROR_RESOURCES[language]!
  }
  registerNamespace(instance, PLATFORM_ERRORS_NAMESPACE, resources)
}
