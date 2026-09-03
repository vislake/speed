/**
 * Shared DOM-test harness for reference-app-web tests.
 *
 * renderWithProviders mounts a unit under the tree the app's own
 * bootstrap builds (see src/main.tsx): I18nextProvider around
 * AppThemeProvider (theme + MUI locale linkage + CssBaseline), around
 * QueryClientProvider -- the app renders @speed/api-sdk's react-query
 * hooks through the shared-QueryClient contract, so every rendered unit
 * needs a query client, the one provider auth-ui's own harness (the
 * template for this file, via account-ui's) does not carry. The client
 * is fresh per call and retries nothing: an operation the test scripts
 * to fail must surface its error on the first attempt, not after
 * react-query default retries, and a mutation must not outlive its
 * test.
 *
 * The i18n instance is created per call with a deterministic
 * configuration (no storage, no URL, no navigator) and every namespace
 * a rendered unit can read is registered: the five namespace-shipping
 * package families the app composes -- ui-kit (whose built-in strings
 * components compose without saying so), layout-kit, auth-ui,
 * tenancy-ui and account-ui -- plus the app's own reference-app
 * namespace. A fresh instance per call keeps registerNamespace's
 * double-registration guard from firing across tests. This harness is
 * the app layer's own copy of the account-ui package's harness --
 * layer-local by design, the standing same-layer pattern across the
 * workspace (extracting a shared harness package is recorded
 * DEFERRED).
 *
 * Tests that exercise a language switch keep the returned instance and
 * act on it (await switchLanguage(i18n, 'en-US')); the provider's
 * 'languageChanged' subscription re-renders the tree.
 */

import { render } from '@testing-library/react'
import type { RenderResult } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactElement } from 'react'
import { attachSession } from '@speed/auth-core'
import {
  createI18n,
  I18nextProvider,
  registerNamespace,
  type I18nInstance,
} from '@speed/i18n'
import { AppThemeProvider } from '@speed/ui-kit'
import { UI_KIT_NAMESPACE, uiKitResources } from '@speed/ui-kit'
import { LAYOUT_KIT_NAMESPACE, layoutKitResources } from '@speed/layout-kit'
import { AUTH_UI_NAMESPACE, authUiResources } from '@speed/auth-ui'
import { TENANCY_UI_NAMESPACE, tenancyUiResources } from '@speed/tenancy-ui'
import { ACCOUNT_UI_NAMESPACE, accountUiResources } from '@speed/account-ui'
import { AppServicesProvider } from '../app-services.js'
import type { AppServices } from '../app-services.js'
import {
  REFERENCE_APP_NAMESPACE,
  referenceAppResources,
} from '../resources.js'

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

/** Fresh bilingual instance with all six namespaces registered. */
export function createAppI18n(language: string = 'zh-CN'): I18nInstance {
  const instance = createI18n({
    supportedLanguages: TEST_LANGUAGES,
    defaultLanguage: language,
    storage: null,
    urlParameterName: null,
    navigatorLanguages: [],
  })
  registerNamespace(instance, UI_KIT_NAMESPACE, uiKitResources)
  registerNamespace(instance, LAYOUT_KIT_NAMESPACE, layoutKitResources)
  registerNamespace(instance, AUTH_UI_NAMESPACE, authUiResources)
  registerNamespace(instance, TENANCY_UI_NAMESPACE, tenancyUiResources)
  registerNamespace(instance, ACCOUNT_UI_NAMESPACE, accountUiResources)
  registerNamespace(instance, REFERENCE_APP_NAMESPACE, referenceAppResources)
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
  const instance = i18n ?? createAppI18n(language)
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

export interface RenderWithAppServicesOptions extends RenderWithProvidersOptions {
  /**
   * Attach the services' session to the auth-core hooks before
   * rendering, mirroring the host bootstrap's attachSession call
   * (last bind wins; the previous session's transitions stop reaching
   * the hooks). Defaults to true; pass false to render a unit that
   * must see the hooks' unattached fail-closed state.
   */
  readonly attach?: boolean
}

export interface RenderWithAppServicesResult extends RenderWithProvidersResult {
  /** The services the tree rendered with. */
  readonly services: AppServices
}

/**
 * Renders a unit under the app's own composition plus its
 * AppServicesProvider (the services layer main.tsx adds around the
 * view machine), attaching the services' session to the auth-core
 * hooks exactly as the host bootstrap does -- view units read the
 * current tenant and the auth state from those hooks, so the journey
 * they exercise is the journey a real page runs.
 */
export function renderWithAppServices(
  ui: ReactElement,
  services: AppServices,
  options: RenderWithAppServicesOptions = {},
): RenderWithAppServicesResult {
  const { attach = true, ...providerOptions } = options
  if (attach) {
    attachSession(services.session)
  }
  const result = renderWithProviders(
    <AppServicesProvider session={services.session} api={services.api}>
      {ui}
    </AppServicesProvider>,
    providerOptions,
  )
  return { ...result, services }
}
