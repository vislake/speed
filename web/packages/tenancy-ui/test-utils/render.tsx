/**
 * Shared DOM-test harness for tenancy-ui tests.
 *
 * renderWithProviders mounts a unit under the tree a real host builds:
 * I18nextProvider (the app-level provider component translations need)
 * around AppThemeProvider (theme + MUI locale linkage + CssBaseline).
 * The i18n instance is created per call through the shared
 * createTestI18n (see @speed/test-utils/render's header), with both
 * namespaces the rendered affordance can read registered: the tenancy-ui
 * namespace for this package's own strings and the ui-kit namespace --
 * carried over from the harness shape the auth-ui suite established (its
 * provider tree registers both namespaces the tree renders under), so a
 * later tenancy-ui affordance rendering ui-kit text needs no harness
 * change.
 *
 * Tests that exercise a language switch keep the returned instance and
 * act on it (await switchLanguage(i18n, 'en-US')); the provider's
 * 'languageChanged' subscription re-renders the tree.
 */

import { render } from '@testing-library/react'
import type { ReactElement } from 'react'
import { I18nextProvider, registerNamespace } from '@speed/i18n'
import type { I18nInstance } from '@speed/i18n'
import { AppThemeProvider } from '@speed/ui-kit'
import { UI_KIT_NAMESPACE, uiKitResources } from '@speed/ui-kit'
import { createTestI18n } from '@speed/test-utils/render'
import type { RenderWithProvidersOptions } from '@speed/test-utils/render'
import type { RenderWithProvidersResult } from '@speed/test-utils/render'
import { TENANCY_UI_NAMESPACE, tenancyUiResources } from '../src/resources.js'

export type { RenderWithProvidersOptions, RenderWithProvidersResult }

/** Fresh bilingual instance with both namespaces registered. */
export function createTenancyUiI18n(language: string = 'zh-CN'): I18nInstance {
  const instance = createTestI18n(language)
  registerNamespace(instance, TENANCY_UI_NAMESPACE, tenancyUiResources)
  registerNamespace(instance, UI_KIT_NAMESPACE, uiKitResources)
  return instance
}

export function renderWithProviders(
  ui: ReactElement,
  options: RenderWithProvidersOptions = {},
): RenderWithProvidersResult {
  const { language = 'zh-CN', i18n } = options
  const instance = i18n ?? createTenancyUiI18n(language)
  const result = render(
    <I18nextProvider i18n={instance}>
      <AppThemeProvider i18n={instance}>{ui}</AppThemeProvider>
    </I18nextProvider>,
  )
  return { ...result, i18n: instance }
}
