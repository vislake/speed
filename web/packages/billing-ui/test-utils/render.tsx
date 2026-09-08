/**
 * Shared DOM-test harness for billing-ui tests.
 *
 * renderWithProviders mounts a unit under the tree a real host builds:
 * I18nextProvider (the app-level provider components' translations
 * need) around AppThemeProvider (theme + MUI locale linkage +
 * CssBaseline), around QueryClientProvider -- billing-ui surfaces read
 * their data through the @tanstack/react-query hooks generated into
 * @speed/api-sdk, so every rendered unit needs a query client, the one
 * provider a host of this package supplies beyond the theme tree. The
 * client is fresh per call and retries nothing: an operation the test
 * scripts to fail must surface its error on the first attempt, not
 * after react-query default retries.
 *
 * The i18n instance is created per call with a deterministic
 * configuration (no storage, no URL, no navigator) and both namespaces
 * a rendered component family can read are registered: the billing-ui
 * namespace for this package's own strings and the ui-kit namespace,
 * because the surface composes ui-kit's EmptyState, whose built-in
 * strings speak ui-kit-namespace keys. A fresh instance per call keeps
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
import {
  BILLING_UI_NAMESPACE,
  billingUiResources,
} from '../src/resources.js'

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
export function createBillingUiI18n(language: string = 'zh-CN'): I18nInstance {
  const instance = createI18n({
    supportedLanguages: TEST_LANGUAGES,
    defaultLanguage: language,
    storage: null,
    urlParameterName: null,
    navigatorLanguages: [],
  })
  registerNamespace(instance, BILLING_UI_NAMESPACE, billingUiResources)
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
  const instance = i18n ?? createBillingUiI18n(language)
  const queryClient = createTestQueryClient()
  const result = render(
    <I18nextProvider i18n={instance}>
      <AppThemeProvider i18n={instance}>
        <QueryClientProvider client={queryClient}>
          {/* The billing page every billing-ui surface lives on: the
              page's h1 above the section under test. Every axe scan in
              the suite runs at the end of a behavioural test, and
              page-has-heading-one is determinate in jsdom now (see
              test-utils/axe.ts's header) -- a section rendered with no
              page heading context would fail the scan instead of
              passing by indeterminacy. The section's own header is a
              level-2 heading standing in under this h1. */}
          <h1>Billing</h1>
          {ui}
        </QueryClientProvider>
      </AppThemeProvider>
    </I18nextProvider>,
  )
  return { ...result, i18n: instance, queryClient }
}
