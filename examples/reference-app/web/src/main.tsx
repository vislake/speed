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
 *     anonymous: the session is memory-only by contract. A session-end
 *     transition (a sign-out or a session death -- a silently refused
 *     refresh) also evicts the departing tenant's namespaced query
 *     cache (evictTenantQueriesOnSessionEnd, below), mirroring
 *     user-menu.tsx's own tenant-switch eviction so a different
 *     account signing into the same tenant afterward never inherits
 *     rows an earlier session's reads cached (reference-app-web.md
 *     P1-1).
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
 *     react-query hooks read from, then the app's own
 *     AppServicesProvider carrying the session and the client (as the
 *     RequestFn the config hooks fetch through) down to the views.
 *
 *  5. The view machine -- AppView, the product-shell composition: the
 *     auth-core-driven three-branch machine (signed out showing the
 *     sign-in surface, signed in with a tenant showing the AppShell
 *     frame over the hash-routed surfaces, a dead session converging
 *     to the session-ended screen), with the frame's nav, header brand
 *     (the server-served Public brand.site_name value) and user menu
 *     (tenant switcher over the demo roster, sign-out) all host
 *     content. The host page itself ships with this directory: index.html
 *     is a real HTML entry that imports this bootstrap and mounts it into
 *     the #root element exactly once, runnable through the vite dev
 *     server and the production build this directory now carries
 *     (vite.config.ts -- the one bundler in the workspace, since the
 *     library packages stay bundler-free by discipline). What still lands
 *     with the M4 html-runner work is serving that built page from the
 *     reference-app server itself; until then the shipped browser story
 *     is the dev-server page plus rendering under test harnesses.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { StrictMode } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { createClient, createMemoryAccessTokenStore } from '@speed/api-client'
import { bindRequestFn } from '@speed/api-sdk/runtime'
import { attachSession, createAuthSession } from '@speed/auth-core'
import type { AuthSession } from '@speed/auth-core'
import {
  createI18n,
  I18nextProvider,
  registerNamespace,
  type I18nInstance,
} from '@speed/i18n'
import {
  AppThemeProvider,
  UI_KIT_NAMESPACE,
  uiKitResources,
} from '@speed/ui-kit'
import { LAYOUT_KIT_NAMESPACE, layoutKitResources } from '@speed/layout-kit'
import { AUTH_UI_NAMESPACE, authUiResources } from '@speed/auth-ui'
import { TENANCY_UI_NAMESPACE, tenancyUiResources } from '@speed/tenancy-ui'
import { ACCOUNT_UI_NAMESPACE, accountUiResources } from '@speed/account-ui'
import { AppView } from './app.js'
import { AppServicesProvider } from './app-services.js'
import {
  REFERENCE_APP_NAMESPACE,
  referenceAppResources,
} from './resources.js'
import { TENANT_QUERY_PREFIX } from './views/user-menu.js'

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
 * Wires the tenant-namespaced query cache to empty the departing
 * tenant's rows the moment the session ends. A manual sign-out and a
 * session death (a silently refused refresh) both settle the same
 * authenticated -> anonymous AuthSnapshot transition (auth-core's
 * session.ts clears the same way for either), so this fires on both.
 *
 * This mirrors user-menu.tsx's own tenant-switch eviction exactly --
 * the same ['tenant', tenantId] prefix, the same queryClient.
 * removeQueries call -- rather than a new cache-management mechanism:
 * a tenant switch evicts the tenant being left mid-session, this
 * evicts the tenant the session was in when it ended. Before this,
 * nothing in the app ever cleared a query cached under a tenant's key
 * on sign-out, so a later sign-in to the same tenant -- by the same
 * account after a session death, or a different account the operator
 * switches to on a shared machine -- inherited rows an earlier
 * session's reads left behind (reference-app-web.md P1-1): the read
 * itself answers a genuine refusal for the new principal, but a gate
 * derived from the query alone cannot un-render rows a shared
 * QueryClient never forgot. Evicting on session end closes that at
 * its root instead of leaving it to the gate to paper over.
 *
 * A full user-scoped query-key segment (['tenant', tenantId, userId,
 * ...]) was the other shape considered and rejected: every tenant-
 * scoped read in this app already re-fetches under the new principal's
 * access token on every mount (staleTime 0, the generated hooks'
 * default), so the leak's live window is exactly "an unmounted
 * component's stale cache entry between one session ending and the
 * next one's first read of the same key" -- precisely what an
 * eviction on the session-end transition closes, without adding a
 * dimension every tenant-scoped query key in the app would need to
 * carry from here on.
 *
 * Returns the session's own unsubscribe function for a caller that
 * wants to tear this down (unit tests do); bootstrapReferenceApp does
 * not hold onto it, since the composed page never tears itself down
 * before an unload a fresh reload starts over from anyway.
 */
export function evictTenantQueriesOnSessionEnd(
  session: AuthSession,
  queryClient: QueryClient,
): () => void {
  let previous = session.getSnapshot()
  return session.subscribe((snapshot) => {
    if (previous.state === 'authenticated' && snapshot.state === 'anonymous') {
      const tenantId = previous.principal?.tenant_id
      if (typeof tenantId === 'string' && tenantId !== '') {
        queryClient.removeQueries({
          queryKey: [TENANT_QUERY_PREFIX, tenantId],
        })
      }
    }
    previous = snapshot
  })
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
  evictTenantQueriesOnSessionEnd(session, queryClient)
  const root = createRoot(container)
  root.render(
    <StrictMode>
      <I18nextProvider i18n={i18n}>
        <AppThemeProvider i18n={i18n}>
          <QueryClientProvider client={queryClient}>
            <AppServicesProvider session={session} api={client}>
              <AppView />
            </AppServicesProvider>
          </QueryClientProvider>
        </AppThemeProvider>
      </I18nextProvider>
    </StrictMode>,
  )
  return { root, i18n, queryClient }
}
