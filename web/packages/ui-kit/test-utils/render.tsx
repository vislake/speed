/**
 * Shared DOM-test harness for ui-kit packages' tests.
 *
 * renderWithProviders mounts a unit under the tree a real host builds:
 * I18nextProvider (the app-level provider components' translations need)
 * around AppThemeProvider (theme + MUI locale linkage + CssBaseline).
 * The i18n instance is created per call with a deterministic
 * configuration (no storage, no URL, no navigator) and the ui-kit
 * namespace registered -- a fresh instance per call keeps
 * registerNamespace's double-registration guard from firing across
 * tests.
 *
 * Tests that exercise a language switch keep the returned instance and
 * act on it (await switchLanguage(i18n, 'en-US')); the provider's
 * 'languageChanged' subscription re-renders the tree.
 */

import { render } from '@testing-library/react'
import type { RenderResult } from '@testing-library/react'
import type { ReactElement } from 'react'
import {
  createI18n,
  I18nextProvider,
  registerNamespace,
  type I18nInstance,
} from '@speed/i18n'
import type { TokensOverride } from '@speed/tokens'
import { AppThemeProvider } from '../src/theme/AppThemeProvider.js'
import { UI_KIT_NAMESPACE, uiKitResources } from '../src/resources.js'

export interface RenderWithProvidersOptions {
  /** Start language of the fresh instance; defaults to zh-CN. */
  readonly language?: string
  /**
   * Reuse an existing instance instead of creating one -- the caller
   * owns its creation AND namespace registration (a second registration
   * on the same instance throws by design).
   */
  readonly i18n?: I18nInstance
  /** Project-layer token overrides passed to AppThemeProvider. */
  readonly projectTokens?: TokensOverride
  /** Tenant-layer token overrides passed to AppThemeProvider. */
  readonly tenantOverrides?: TokensOverride
}

export interface RenderWithProvidersResult extends RenderResult {
  /** The instance the tree renders with; language-switch tests act on it. */
  readonly i18n: I18nInstance
}

export const TEST_LANGUAGES = ['zh-CN', 'en-US'] as const

export function createUiKitI18n(language: string = 'zh-CN'): I18nInstance {
  const instance = createI18n({
    supportedLanguages: TEST_LANGUAGES,
    defaultLanguage: language,
    storage: null,
    urlParameterName: null,
    navigatorLanguages: [],
  })
  registerNamespace(instance, UI_KIT_NAMESPACE, uiKitResources)
  return instance
}

export function renderWithProviders(
  ui: ReactElement,
  options: RenderWithProvidersOptions = {},
): RenderWithProvidersResult {
  const {
    language = 'zh-CN',
    i18n,
    projectTokens,
    tenantOverrides,
  } = options
  const instance = i18n ?? createUiKitI18n(language)
  const result = render(
    <I18nextProvider i18n={instance}>
      <AppThemeProvider
        i18n={instance}
        projectTokens={projectTokens}
        tenantOverrides={tenantOverrides}
      >
        {ui}
      </AppThemeProvider>
    </I18nextProvider>,
  )
  return { ...result, i18n: instance }
}
