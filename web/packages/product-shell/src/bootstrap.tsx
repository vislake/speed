/**
 * bootstrapSpeedApp -- the app-entry assembly of the shell tier: the
 * one call a delivered app's main module makes, wired from a single
 * declarative definition, so a new app composes the platform instead of
 * re-deriving it.
 *
 * The entry is a subpath export (`@speed/product-shell/bootstrap`), the
 * api-sdk `./runtime` precedent: the main entry keeps the component
 * package's dependency floor exactly as documented (auth-core, auth-ui,
 * layout-kit, i18n), while this entry additionally imports the pieces an
 * app entry point exists to drive -- @speed/api-client (the credential
 * store, the HTTP client), @speed/api-sdk's runtime seam
 * (bindRequestFn), @speed/ui-kit (the theme provider) and
 * @tanstack/react-query -- every one of them already in the main
 * entry's transitive closure (auth-core depends on api-client and
 * api-sdk; layout-kit and auth-ui depend on ui-kit), so the manifest
 * additions cost a consumer nothing it does not already install.
 *
 * What the assembly wires, in order:
 *
 *  1. i18n -- one fresh bilingual instance over the platform defaults
 *     (?lang= URL parameter, stored choice, navigator languages, en-US
 *     last). The namespaces of the four packages the assembly itself
 *     composes register automatically -- ui-kit, layout-kit, auth-ui
 *     and product-shell's own -- because the stack it renders always
 *     includes them; a host can no longer forget one and read raw keys.
 *     The platform error-copy bundle registers with them and is named as
 *     the instance's fallback namespace, so a backend error code no
 *     package namespace covers resolves to the module catalog's own
 *     words instead of the raw key. The definition's `namespaces` carry
 *     the app's own additions: the slot packages its views compose
 *     (tenancy-ui, account-ui, ...) and its own bundle. Registration
 *     happens before render; double registration throws, so each
 *     namespace appears exactly once.
 *
 *  2. The session -- a memory access-token store (the credential never
 *     touches storage) feeding the auth-core session state machine over
 *     the generated authn operations, attached with attachSession. A
 *     reload starts anonymous by contract.
 *
 *  3. The client -- createClient over the environment's own fetch, with
 *     the session's silent refresh as the 401-refresh leg and the i18n
 *     instance as the language source (every request announces the
 *     instance's current language through accept-language, read per
 *     attempt, so the backend's requester-language tiers see exactly
 *     what the frontend chain resolved), bound into
 *     the api-sdk runtime seam every generated operation calls through.
 *     All API traffic is generated-code traffic from here on.
 *
 *  4. The query client and the session-end strategy -- the no-retry
 *     policy below, and watchSessionEnd's shipped default: the moment
 *     the session ends, everything the departing principal's reads
 *     cached is evicted. The definition's `sessionEnded` overrides the
 *     default action.
 *
 *  5. The provider stack -- I18nextProvider around AppThemeProvider
 *     around QueryClientProvider, then the definition's `providers`
 *     (outermost first) around the definition's `view`. The reference
 *     composition: `view` is the app's shell composition (ProductShell
 *     with its host slots), and one provider hands the app's views the
 *     assembled session and client.
 *
 * The returned handles (root, i18n, queryClient, session) are for
 * hosts and harnesses that act on the composition; unmount() tears the
 * page down. A page bootstraps exactly once, into one container.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { StrictMode } from 'react'
import type { ReactElement, ReactNode } from 'react'
import { createRoot } from 'react-dom/client'
import type { Root } from 'react-dom/client'
import { createClient, createMemoryAccessTokenStore } from '@speed/api-client'
import type { RequestFn } from '@speed/api-client'
import { bindRequestFn } from '@speed/api-sdk/runtime'
import { attachSession, createAuthSession } from '@speed/auth-core'
import type { AuthSession } from '@speed/auth-core'
import { AUTH_UI_NAMESPACE, authUiResources } from '@speed/auth-ui'
import { createI18n, I18nextProvider, registerNamespace } from '@speed/i18n'
import type { I18nInstance, ResourceBundle } from '@speed/i18n'
import {
  PLATFORM_ERRORS_NAMESPACE,
  registerPlatformErrors,
} from '@speed/i18n/platform-errors'
import { LAYOUT_KIT_NAMESPACE, layoutKitResources } from '@speed/layout-kit'
import {
  AppThemeProvider,
  UI_KIT_NAMESPACE,
  uiKitResources,
} from '@speed/ui-kit'
import { PRODUCT_SHELL_NAMESPACE, productShellResources } from './resources.js'

/** One i18n namespace the app registers itself: a package family its
 * views compose (each sibling ships its own pair) or the app's own
 * bundle. The four namespaces of the packages the assembly composes
 * register automatically and must not be listed here. */
export interface SpeedAppNamespace {
  readonly namespace: string
  readonly resources: Readonly<Record<string, ResourceBundle>>
}

/** The services the assembled composition passes to the definition's
 * providers. `api` is the client as the RequestFn it structurally is --
 * the same value every generated operation reaches through the bound
 * runtime seam, shareable with hand-written per-key layers
 * (@speed/api-client/react). */
export interface SpeedAppServices {
  /** The attached session views drive sign-in, tenant switch and
   * sign-out through. */
  readonly session: AuthSession
  /** The client the assembly bound into the api-sdk runtime seam. */
  readonly api: RequestFn
  /** The query client the provider stack serves. */
  readonly queryClient: QueryClient
}

/** An app-level provider wrapping the app's view inside the standard
 * stack (inside QueryClientProvider): a normal provider component's
 * body, given the assembled services. The definition's providers nest
 * outermost first. */
export type SpeedAppProvider = (
  services: SpeedAppServices,
  children: ReactNode,
) => ReactElement

/** The context of a session-end transition (an authenticated session
 * turning anonymous -- a sign-out or a session death, the same snapshot
 * flip). */
export interface SpeedSessionEndContext {
  readonly session: AuthSession
  readonly queryClient: QueryClient
  /** The shipped default action: removes every query. Total by design:
   * every row in the cache was fetched under the departing principal's
   * access token, and identity-domain rows live under bare spec-path
   * keys a tenant-scoped removal cannot reach -- a different account
   * signing in afterward must start from an empty cache. An override
   * that wants the default keeps this call in its own order. */
  readonly evictAllQueries: () => void
}

/** The session-end action: what happens the moment the session ends.
 * Absent from the definition, the shipped default (total query-cache
 * eviction) runs. */
export type SpeedSessionEndedHandler = (context: SpeedSessionEndContext) => void

/** The declarative definition of an app: everything that varies per
 * app, in one table. The machinery around it -- the session, the
 * client, the seam binding, the provider stack, the session-end
 * strategy -- is the assembly's. */
export interface SpeedAppDefinition {
  /** The app's own namespaces, beyond the family's automatic four (see
   * the header). Each registers exactly once, before render. */
  readonly namespaces?: readonly SpeedAppNamespace[]
  /** App-level providers between the standard stack and the view,
   * outermost first. */
  readonly providers?: readonly SpeedAppProvider[]
  /** The app's view root: the shell composition (ProductShell with its
   * host slots) or any view the app renders instead. */
  readonly view: ReactNode
  /** Overrides the shipped session-end default (see
   * SpeedSessionEndedHandler). */
  readonly sessionEnded?: SpeedSessionEndedHandler
  /** The client's base URL. Defaults to the page's own origin, the
   * same-origin deployment shape. */
  readonly baseUrl?: string
}

/** What the assembly produced, for hosts and harnesses that act on the
 * composition. */
export interface SpeedAppBootstrap {
  /** The mounted root; unmount() tears the page down. */
  readonly root: Root
  /** The instance the tree renders with (language switching acts on
   * it). */
  readonly i18n: I18nInstance
  /** The query client the tree renders with, and the one the session-
   * end strategy evicts. */
  readonly queryClient: QueryClient
  /** The attached session, for hosts that drive its operations
   * directly. */
  readonly session: AuthSession
}

/**
 * The shipped session-end strategy. Watches the session for the one
 * transition that matters -- authenticated turning anonymous, whether
 * from a sign-out or a silently refused refresh (auth-core settles both
 * the same way) -- and on that edge runs the definition's handler, or,
 * absent one, the default: evictAllQueries(). Edges that only rotate
 * the principal (a tenant switch keeps state authenticated) fire
 * nothing; a session that was never authenticated fires nothing (the
 * first snapshot is the memory's baseline, not a transition). An
 * unauthenticated -> authenticated edge just moves the memory forward.
 *
 * The total eviction is not a coarse hammer. Two domains make it
 * necessary: the tenant-namespaced rows (['tenant', tenantId, ...])
 * keyed under the session's one tenant, and the identity-domain rows
 * (bare spec-path keys -- sessions, login history, bound identities --
 * nothing in @speed/api-sdk is tenant-namespaced), which carry no
 * tenant segment for a tenant-scoped removal to reach. Only a total
 * eviction stops a different account signing in afterward, into any
 * tenant, from inheriting the earlier account's rows out of the shared
 * query client's memory. (Mid-session tenant-switch eviction is the
 * host's own concern; this strategy deliberately does not fire there.)
 *
 * Returns the session's own unsubscribe function for a caller that
 * wants to tear this down; bootstrapSpeedApp does not hold onto it,
 * since the composed page never tears itself down before an unload a
 * fresh reload starts over from anyway.
 */
export function watchSessionEnd(
  session: AuthSession,
  queryClient: QueryClient,
  handler?: SpeedSessionEndedHandler,
): () => void {
  let previous = session.getSnapshot()
  return session.subscribe((snapshot) => {
    if (previous.state === 'authenticated' && snapshot.state === 'anonymous') {
      const context: SpeedSessionEndContext = {
        session,
        queryClient,
        evictAllQueries: () => {
          queryClient.removeQueries()
        },
      }
      if (handler !== undefined) {
        handler(context)
      } else {
        context.evictAllQueries()
      }
    }
    previous = snapshot
  })
}

/**
 * The query client's retry policy, deliberate: nothing retries at this
 * layer -- neither queries nor mutations -- because transient retries
 * belong to the transport, not to this layer:
 *
 *  - @speed/api-client's frozen retry policy already retries exactly
 *    the transient classes a repetition could redeem -- 429 (honouring
 *    Retry-After) plus 502/503/504, network failures and timeouts, on
 *    idempotent methods only -- inside a single request. By the time a
 *    queryFn call rejects, that budget is spent; a react-query retry
 *    would only re-run it whole (up to a twelve-attempt worst case
 *    across the two layers) and add exponential backoff on top.
 *  - Every answer that carries a code -- an envelope answer such as a
 *    403 rbac.permission_denied, or a client.* transport code -- is a
 *    definitive answer a repetition cannot redeem.
 *  - A mutation must never re-fire after a lost response: a create
 *    whose answer timed out server-side may already have committed, and
 *    a retry would duplicate it.
 *
 * Recovery is not lost: every query is staleTime 0, so a remount or a
 * window refocus re-reads under the current token, and the transport's
 * own retries cover momentary outages.
 *
 * Exported because the policy is the assembly's, not a bootstrap
 * internal: a host or suite that renders the app's views under the
 * same client outside bootstrapSpeedApp (a view-level test harness,
 * a custom composition) needs the identical policy rather than a
 * reconstructed copy that could drift from this one.
 */
export function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  })
}

/**
 * Builds the whole app composition into the given container (the
 * header lists what is wired, in order). Calling it twice into one
 * container double-mounts; a page bootstraps exactly once.
 */
export function bootstrapSpeedApp(
  container: Element,
  definition: SpeedAppDefinition,
): SpeedAppBootstrap {
  const i18n = createI18n({ fallbackNamespaces: [PLATFORM_ERRORS_NAMESPACE] })
  registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)
  registerNamespace(i18n, LAYOUT_KIT_NAMESPACE, layoutKitResources)
  registerNamespace(i18n, AUTH_UI_NAMESPACE, authUiResources)
  registerNamespace(i18n, PRODUCT_SHELL_NAMESPACE, productShellResources)
  registerPlatformErrors(i18n)
  for (const { namespace, resources } of definition.namespaces ?? []) {
    registerNamespace(i18n, namespace, resources)
  }

  const accessTokenStore = createMemoryAccessTokenStore()
  const session = createAuthSession(accessTokenStore)
  attachSession(session)

  const client = createClient({
    baseUrl: definition.baseUrl ?? window.location.origin,
    accessTokenStore,
    refreshAccessToken: () => session.refresh(),
    // The frontend negotiation chain's resolved value, transported: the
    // provider reads the instance per attempt (createClient's own
    // contract), so a request retried after a language switch announces
    // the language the user is looking at, and a request the backend
    // serves as "requester is recipient" content (an SMS code, the type
    // directory) renders in it.
    languageProvider: () => i18n.language,
  })
  bindRequestFn(client)

  const queryClient = createQueryClient()
  watchSessionEnd(session, queryClient, definition.sessionEnded)

  const services: SpeedAppServices = { session, api: client, queryClient }
  let view: ReactNode = definition.view
  for (const provide of [...(definition.providers ?? [])].reverse()) {
    view = provide(services, view)
  }

  const root = createRoot(container)
  root.render(
    <StrictMode>
      <I18nextProvider i18n={i18n}>
        <AppThemeProvider i18n={i18n}>
          <QueryClientProvider client={queryClient}>
            {view}
          </QueryClientProvider>
        </AppThemeProvider>
      </I18nextProvider>
    </StrictMode>,
  )
  return { root, i18n, queryClient, session }
}
