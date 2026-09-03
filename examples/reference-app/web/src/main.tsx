/**
 * main.tsx -- reference-app-web's bootstrap: the single composition
 * point where the app wires the @speed packages the way a delivered
 * consumer project does, and the browser-leg consumer proof of their
 * host contract.
 *
 * The file exports a bootstrap function and never runs module-scope
 * side effects: importing the module does nothing, so component suites
 * and future harnesses can import it safely, and the whole composition
 * runs exactly once per page load, wherever the host mounts it.
 *
 * What bootstrapReferenceApp wires, in order:
 *
 *  1. i18n -- one fresh bilingual instance under the browser's default
 *     negotiation (the ?lang= URL parameter, the stored choice, then
 *     the navigator languages, zh-CN last), with every namespace a
 *     rendered unit can read registered exactly once: the five
 *     namespace-shipping package families (ui-kit, whose built-in
 *     strings the components compose without saying so, layout-kit,
 *     auth-ui, tenancy-ui and account-ui) plus the app's own
 *     reference-app namespace. Cross-language fallback is impossible
 *     by construction: a missing key renders as the key itself.
 *
 *  2. The session -- a memory access-token store (the credential never
 *     touches storage; nothing here writes localStorage) feeding the
 *     auth-core session state machine over the generated authn
 *     operations, attached to the auth-core hooks. A reload starts
 *     anonymous: the session is memory-only by contract.
 *
 *  3. The client -- the app's one HTTP surface: @speed/api-client's
 *     createClient over the environment's own fetch (no fetch option:
 *     createClient captures globalThis.fetch at construction), with
 *     the session's silent refresh as the 401-refresh leg, bound into
 *     the api-sdk runtime seam every generated operation calls
 *     through. All API traffic is generated-code traffic from here on;
 *     no other module in the app touches HTTP.
 *
 *  4. The providers -- I18nextProvider around AppThemeProvider (token
 *     theme + the MUI locale of the active language + CssBaseline)
 *     around the shared QueryClientProvider contract the generated
 *     react-query hooks read from.
 *
 * B1 renders a skeleton placeholder in place of the real frame: the
 * three-branch view machine (product-shell over the AppShell frame with
 * the tenant switcher, RouteGuard gates wired behind real permission
 * fetches) and the business surfaces land in the shell iteration that
 * follows this one. The html runner that mounts this bootstrap into a
 * real browser page served by the reference-app server lands with the
 * M4 html-runner work -- until then this module compiles, typechecks
 * and renders under test harnesses, which is the whole of the shipped
 * browser story.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { StrictMode, type ReactElement } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { createClient, createMemoryAccessTokenStore } from '@speed/api-client'
import { bindRequestFn } from '@speed/api-sdk/runtime'
import { attachSession, createAuthSession } from '@speed/auth-core'
import {
  createI18n,
  I18nextProvider,
  registerNamespace,
  useTranslation,
  type I18nInstance,
} from '@speed/i18n'
import {
  AppThemeProvider,
  EmptyState,
  UI_KIT_NAMESPACE,
  uiKitResources,
} from '@speed/ui-kit'
import { LAYOUT_KIT_NAMESPACE, layoutKitResources } from '@speed/layout-kit'
import { AUTH_UI_NAMESPACE, authUiResources } from '@speed/auth-ui'
import { TENANCY_UI_NAMESPACE, tenancyUiResources } from '@speed/tenancy-ui'
import { ACCOUNT_UI_NAMESPACE, accountUiResources } from '@speed/account-ui'
import {
  REFERENCE_APP_NAMESPACE,
  referenceAppResources,
} from './resources.js'

/** What a page's bootstrap produced: the mounted root, the i18n
 * instance and the query client, for hosts and harnesses that act on
 * the composition. */
export interface ReferenceAppBootstrap {
  /** The mounted root; unmount() tears the page down. */
  readonly root: Root
  /** The instance the tree renders with (language switching acts on it). */
  readonly i18n: I18nInstance
  /** The query client the tree renders with. */
  readonly queryClient: QueryClient
}

/**
 * Builds the whole app composition into the given container. Calling it
 * twice into one container double-mounts; a page bootstraps exactly
 * once.
 */
export function bootstrapReferenceApp(
  container: Element,
): ReferenceAppBootstrap {
  const i18n = createI18n({
    supportedLanguages: ['zh-CN', 'en-US'],
    defaultLanguage: 'zh-CN',
  })
  registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)
  registerNamespace(i18n, LAYOUT_KIT_NAMESPACE, layoutKitResources)
  registerNamespace(i18n, AUTH_UI_NAMESPACE, authUiResources)
  registerNamespace(i18n, TENANCY_UI_NAMESPACE, tenancyUiResources)
  registerNamespace(i18n, ACCOUNT_UI_NAMESPACE, accountUiResources)
  registerNamespace(i18n, REFERENCE_APP_NAMESPACE, referenceAppResources)

  const accessTokenStore = createMemoryAccessTokenStore()
  const session = createAuthSession(accessTokenStore)
  attachSession(session)

  const client = createClient({
    baseUrl: window.location.origin,
    accessTokenStore,
    refreshAccessToken: () => session.refresh(),
  })
  bindRequestFn(client)

  const queryClient = new QueryClient()
  const root = createRoot(container)
  root.render(
    <StrictMode>
      <I18nextProvider i18n={i18n}>
        <AppThemeProvider i18n={i18n}>
          <QueryClientProvider client={queryClient}>
            <ShellPlaceholder />
          </QueryClientProvider>
        </AppThemeProvider>
      </I18nextProvider>
    </StrictMode>,
  )
  return { root, i18n, queryClient }
}

/** The B1 skeleton view: the ui-kit EmptyState speaking the app's own
 * namespace keys, standing in for the product-shell view machine and
 * the AppShell frame the shell iteration composes. */
function ShellPlaceholder(): ReactElement {
  const { t } = useTranslation(REFERENCE_APP_NAMESPACE)
  return (
    <EmptyState
      title={t('skeleton.title')}
      description={t('skeleton.description')}
    />
  )
}
