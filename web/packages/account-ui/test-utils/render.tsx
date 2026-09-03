/**
 * Shared DOM-test harness for account-ui tests.
 *
 * renderWithProviders mounts a unit under the tree a real host builds:
 * I18nextProvider (the app-level provider components' translations need)
 * around AppThemeProvider (theme + MUI locale linkage + CssBaseline),
 * around QueryClientProvider -- account-ui surfaces read their data
 * through the @tanstack/react-query hooks generated into @speed/api-sdk,
 * so every rendered unit needs a query client, the one provider auth-ui's
 * own harness (the template for this file) does not carry. The client is
 * fresh per call and retries nothing: an operation the test scripts to
 * fail must surface its error on the first attempt, not after react-query
 * default retries, and a mutation must not outlive its test.
 *
 * The i18n instance is created per call with a deterministic
 * configuration (no storage, no URL, no navigator) and both namespaces a
 * rendered component family can read are registered: the account-ui
 * namespace for this package's own strings and the ui-kit namespace,
 * because the surfaces compose ui-kit components whose built-in strings
 * speak ui-kit-namespace keys. A fresh instance per call keeps
 * registerNamespace's double-registration guard from firing across
 * tests.
 *
 * Tests that exercise a language switch keep the returned instance and
 * act on it (await switchLanguage(i18n, 'en-US')); the provider's
 * 'languageChanged' subscription re-renders the tree.
 */

import { render } from '@testing-library/react'
import type { RenderResult } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactElement } from 'react'
import {
  createI18n,
  I18nextProvider,
  registerNamespace,
  type I18nInstance,
} from '@speed/i18n'
import { AppThemeProvider } from '@speed/ui-kit'
import { UI_KIT_NAMESPACE, uiKitResources } from '@speed/ui-kit'
import { ACCOUNT_UI_NAMESPACE, accountUiResources } from '../src/resources.js'

export interface RenderWithProvidersOptions {
  /** Start language of the fresh instance; defaults to zh-CN. */
  readonly language?: string
  /**
   * Reuse an existing instance instead of creating one -- the caller
   * owns its creation AND namespace registration (a second registration
   * on the same instance throws by design).
   */
  readonly i18n?: I18nInstance
}

export interface RenderWithProvidersResult extends RenderResult {
  /** The instance the tree renders with; language-switch tests act on it. */
  readonly i18n: I18nInstance
  /**
   * The query client the tree renders with. Tests whose operations
   * invalidate or seed react-query state act on it.
   */
  readonly queryClient: QueryClient
}

export const TEST_LANGUAGES = ['zh-CN', 'en-US'] as const

/** Fresh bilingual instance with both namespaces registered. */
export function createAccountUiI18n(language: string = 'zh-CN'): I18nInstance {
  const instance = createI18n({
    supportedLanguages: TEST_LANGUAGES,
    defaultLanguage: language,
    storage: null,
    urlParameterName: null,
    navigatorLanguages: [],
  })
  registerNamespace(instance, ACCOUNT_UI_NAMESPACE, accountUiResources)
  registerNamespace(instance, UI_KIT_NAMESPACE, uiKitResources)
  return instance
}

/** Fresh client that never retries: see the header. */
export function createTestQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  })
}

export function renderWithProviders(
  ui: ReactElement,
  options: RenderWithProvidersOptions = {},
): RenderWithProvidersResult {
  const { language = 'zh-CN', i18n } = options
  const instance = i18n ?? createAccountUiI18n(language)
  const queryClient = createTestQueryClient()
  const result = render(
    <I18nextProvider i18n={instance}>
      <AppThemeProvider i18n={instance}>
        <QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>
      </AppThemeProvider>
    </I18nextProvider>,
  )
  return { ...result, i18n: instance, queryClient }
}
