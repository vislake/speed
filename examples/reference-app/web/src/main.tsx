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
 *     refresh) also empties the whole query cache
 *     (evictQueriesOnSessionEnd, below): every row the departing
 *     principal's reads cached -- tenant-namespaced data and the
 *     identity-domain rows the account surface reads alike -- is gone
 *     the moment the session is, so a different account signing in
 *     afterward, into any tenant, starts from an empty cache and can
 *     never inherit an earlier session's rows (reference-app-web.md
 *     P1-1 and P1-apisdk-1).
 *
 *  2a. The query client itself -- created by createAppQueryClient,
 *     below, whose no-retry policy is this host's deliberate answer to
 *     where transient retries belong (reference-app-web.md P2-rnweb-2).
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
 *     library packages stay bundler-free by discipline). The built page
 *     is also what the reference-app server itself serves in a deployed
 *     shape: the Dockerfile builds this directory's dist/ into the image
 *     and the server serves it from disk under APP_WEB_DIST (see
 *     cmd/server/frontend.go -- the round that discharged the serving
 *     deferral this comment used to record). What still lands with the
 *     M4 e2e/html-runner work is browser automation driving that
 *     server-served page; until then the shipped browser story is the
 *     dev-server page plus rendering under test harnesses.
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
import { clearNotesDraft } from './views/notes-draft.js'

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
 * Wires the page's principal-bound leftovers to empty the moment the
 * session ends: the query cache (every query was fetched under the
 * departing principal's access token) and the notes create form's
 * half-typed draft (views/notes-draft.ts -- a draft is the same class
 * of leftover as a cached row: text typed by the departing account
 * must not greet the next account signing into this page). A manual
 * sign-out and a session death (a silently refused refresh) both
 * settle the same authenticated -> anonymous AuthSnapshot transition
 * (auth-core's session.ts clears the same way for either), so this
 * fires on both.
 *
 * The eviction is total -- removeQueries() with no filter -- because
 * every query this page holds was fetched under the departing
 * principal's access token, and no query is safe to carry across an
 * authenticated -> anonymous boundary. That includes two domains the
 * earlier, tenant-only eviction (reference-app-web.md P1-1) missed:
 *
 *  - the tenant-namespaced rows (['tenant', tenantId, ...] -- the notes
 *    list), which the old eviction cleared for the session's one
 *    tenant, and
 *  - the identity-domain rows the account surface reads through bare
 *    spec-path keys ('/api/v1/authn/sessions', '/api/v1/authn/
 *    login-history' and '/api/v1/authn/identities' -- nothing in
 *    @speed/api-sdk is tenant-namespaced), which the old eviction never
 *    touched: the keys carry no tenant segment for a ['tenant',
 *    tenantId] removal to reach, so a session-end left them cached for
 *    a different account signing in afterward to inherit -- the
 *    account page would answer the earlier account's sessions, login
 *    history and bound identities out of the shared QueryClient's
 *    memory (reference-app-web.md P1-apisdk-1).
 *
 * Evicting everything on the authenticated -> anonymous edge closes
 * both at the root: whatever domain a future surface reads in, its
 * rows cannot outlive the session that fetched them, and a later
 * account always starts from an empty cache. (user-menu.tsx's own
 * tenant-switch eviction -- still mid-session, where this eviction
 * deliberately does not fire -- clears the same tenant prefix plus the
 * identity-domain keys, since a switch rotates the access token those
 * rows were answered under.)
 *
 * Returns the session's own unsubscribe function for a caller that
 * wants to tear this down (unit tests do); bootstrapReferenceApp does
 * not hold onto it, since the composed page never tears itself down
 * before an unload a fresh reload starts over from anyway.
 */
export function evictQueriesOnSessionEnd(
  session: AuthSession,
  queryClient: QueryClient,
): () => void {
  let previous = session.getSnapshot()
  return session.subscribe((snapshot) => {
    if (previous.state === 'authenticated' && snapshot.state === 'anonymous') {
      queryClient.removeQueries()
      clearNotesDraft()
    }
    previous = snapshot
  })
}

/**
 * The page's QueryClient. It retries nothing -- neither queries nor
 * mutations -- by deliberate policy, because transient retries belong
 * to the transport, not to this layer:
 *
 *  - @speed/api-client's frozen retry policy already retries exactly
 *    the transient classes a repetition could redeem -- 429 (honouring
 *    Retry-After) plus 502/503/504, network failures and timeouts, on
 *    idempotent methods only, up to three attempts under full-jitter
 *    backoff -- inside a single request. By the time a queryFn call
 *    rejects, that budget is already spent; a react-query retry would
 *    only re-run it whole (up to a twelve-attempt worst case across
 *    the two layers) and add exponential backoff on top.
 *  - Every answer that carries a code -- an envelope answer such as a
 *    403 rbac.permission_denied, or a client.* transport code -- is a
 *    definitive answer a repetition cannot redeem. Retrying a refused
 *    authorization read three times with the default backoff burns a
 *    doomed ~7-second window before the refusal surfaces, and re-arms
 *    it on every refetch (reference-app-web.md P2-rnweb-2).
 *  - A mutation must never re-fire after a lost response: a create
 *    whose answer timed out server-side may already have committed,
 *    and a retry would duplicate it.
 *
 * Recovery is not lost: every query is staleTime 0, so a remount or a
 * window refocus re-reads under the current token; the transport's own
 * transient retries cover momentary outages; and a refused read now
 * surfaces to the gate on the first response, where the surface's
 * fail-closed states answer it.
 */
export function createAppQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
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

  const queryClient = createAppQueryClient()
  evictQueriesOnSessionEnd(session, queryClient)
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
