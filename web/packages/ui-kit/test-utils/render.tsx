/**
 * Shared DOM-test harness for ui-kit packages' tests.
 *
 * renderWithProviders mounts a unit under the tree a real host builds:
 * I18nextProvider (the app-level provider components' translations need)
 * around AppThemeProvider (theme + MUI locale linkage + CssBaseline).
 * The i18n instance is created per call through the shared
 * createTestI18n (see @speed/test-utils/render's header), with the
 * ui-kit namespace registered.
 *
 * Tests that exercise a language switch keep the returned instance and
 * act on it (await switchLanguage(i18n, 'en-US')); the provider's
 * 'languageChanged' subscription re-renders the tree.
 */

import { render } from '@testing-library/react'
import type { ReactElement } from 'react'
import { I18nextProvider, registerNamespace } from '@speed/i18n'
import type { I18nInstance } from '@speed/i18n'
import type { TokensOverride } from '@speed/tokens'
import { createTestI18n } from '@speed/test-utils/render'
import type { RenderWithProvidersOptions as BaseOptions } from '@speed/test-utils/render'
import type { RenderWithProvidersResult } from '@speed/test-utils/render'
import { AppThemeProvider } from '../src/theme/AppThemeProvider.js'
import { UI_KIT_NAMESPACE, uiKitResources } from '../src/resources.js'

export type { RenderWithProvidersResult }

export interface RenderWithProvidersOptions extends BaseOptions {
  /** Project-layer token overrides passed to AppThemeProvider. */
  readonly projectTokens?: TokensOverride
  /** Tenant-layer token overrides passed to AppThemeProvider. */
  readonly tenantOverrides?: TokensOverride
}

export function createUiKitI18n(language: string = 'zh-CN'): I18nInstance {
  const instance = createTestI18n(language)
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
