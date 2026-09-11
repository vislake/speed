/**
 * Shared DOM-test harness for product-shell tests.
 *
 * renderWithProviders mounts a unit under the tree a real host builds:
 * I18nextProvider (the app-level provider component translations need)
 * around AppThemeProvider (theme + MUI locale linkage + CssBaseline).
 * The i18n instance is created per call through the shared
 * createTestI18n (see @speed/test-utils/render's header), with the
 * namespaces the rendered shell can read registered: the ui-kit
 * namespace (the theme provider and EmptyState texts), the layout-kit
 * namespace (the AppShell frame's built-in strings), the auth-ui
 * namespace (the default session-ended screen's) and the product-shell
 * namespace (the machine's own session-ended transition announcement)
 * -- the four namespaces every product-shell host registers at
 * bootstrap, exactly as the README quick start documents.
 *
 * Tests that exercise a language switch keep the returned instance and
 * act on it (await switchLanguage(i18n, 'en-US')); the provider's
 * 'languageChanged' subscription re-renders the tree.
 */

import { render } from '@testing-library/react'
import type { ReactElement } from 'react'
import { I18nextProvider, registerNamespace } from '@speed/i18n'
import type { I18nInstance } from '@speed/i18n'
import { AppThemeProvider, UI_KIT_NAMESPACE, uiKitResources } from '@speed/ui-kit'
import {
  LAYOUT_KIT_NAMESPACE,
  layoutKitResources,
} from '@speed/layout-kit'
import { AUTH_UI_NAMESPACE, authUiResources } from '@speed/auth-ui'
import { createTestI18n } from '@speed/test-utils/render'
import type { RenderWithProvidersOptions } from '@speed/test-utils/render'
import type { RenderWithProvidersResult } from '@speed/test-utils/render'
import {
  PRODUCT_SHELL_NAMESPACE,
  productShellResources,
} from '../src/resources.js'

export type { RenderWithProvidersOptions, RenderWithProvidersResult }

/** Fresh bilingual instance with the four namespaces registered. */
export function createProductShellI18n(language: string = 'zh-CN'): I18nInstance {
  const instance = createTestI18n(language)
  registerNamespace(instance, UI_KIT_NAMESPACE, uiKitResources)
  registerNamespace(instance, LAYOUT_KIT_NAMESPACE, layoutKitResources)
  registerNamespace(instance, AUTH_UI_NAMESPACE, authUiResources)
  registerNamespace(instance, PRODUCT_SHELL_NAMESPACE, productShellResources)
  return instance
}

export function renderWithProviders(
  ui: ReactElement,
  options: RenderWithProvidersOptions = {},
): RenderWithProvidersResult {
  const { language = 'zh-CN', i18n } = options
  const instance = i18n ?? createProductShellI18n(language)
  const result = render(
    <I18nextProvider i18n={instance}>
      <AppThemeProvider i18n={instance}>{ui}</AppThemeProvider>
    </I18nextProvider>,
  )
  return { ...result, i18n: instance }
}
