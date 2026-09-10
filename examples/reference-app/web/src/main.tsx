/**
 * main.tsx -- reference-app-web's bootstrap: the app's declarative
 * definition handed to the platform's entry assembly
 * (@speed/product-shell/bootstrap's bootstrapSpeedApp), the one call a
 * delivered app's main module makes.
 *
 * The file exports a bootstrap function and never runs module-scope
 * side effects: importing the module does nothing, so component suites
 * and future harnesses can import it safely, and the whole composition
 * runs exactly once per page load, wherever the host mounts it. The
 * assembly itself -- the fresh bilingual i18n instance with the
 * family's four namespaces registered, the memory-token session
 * attached over the generated authn operations, the one client bound
 * into the api-sdk runtime seam with the session refresh as its
 * 401-refresh leg, the no-retry query client, the session-end strategy
 * and the provider stack (I18next -> theme -> QueryClient -> declared
 * providers) -- is the package's, documented there.
 *
 * What remains here is the app's variation, in one table:
 *
 *  - The namespaces the app's views render beyond the family's
 *    automatic four: tenancy-ui's (the tenant switcher in the user
 *    menu), account-ui's (the account surface) and the app's own
 *    reference-app bundle.
 *
 *  - The app's services provider: AppServicesProvider carries the
 *    assembled session and client down to the views (the session for
 *    every view that drives a session operation, the client as the one
 *    RequestFn the config hooks share).
 *
 *  - The view machine: AppView, the app's product-shell composition
 *    (app.tsx), as the single view root.
 *
 *  - The session-end override: the base root assembly already ships a
 *    total query-cache eviction the moment the session ends (nothing a
 *    departing principal's reads cached may greet the next account);
 *    the app adds its own principal-bound leftover of the same class,
 *    the notes create form's half-typed draft (views/notes-draft.ts --
 *    text typed by the departing account must not greet the next
 *    account signing into this page). Both fire on the same
 *    authenticated -> anonymous transition -- a manual sign-out and a
 *    session death (a silently refused refresh) settle it the same
 *    way -- so the override composes the default action with the
 *    draft's clearing.
 *
 * The host page itself ships with this directory: index.html is a real
 * HTML entry that imports this bootstrap and mounts it into the #root
 * element exactly once, runnable through the vite dev server and the
 * production build this directory now carries (vite.config.ts -- the
 * one bundler in the workspace, since the library packages stay
 * bundler-free by discipline). The built page is also what the
 * reference-app server itself serves in a deployed shape: the
 * Dockerfile builds this directory's dist/ into the image and the
 * server serves it from disk under APP_WEB_DIST
 * (internal/app/frontend.go). What does not ship is browser automation
 * driving that server-served page; the shipped browser story is the
 * dev-server page plus rendering under test harnesses.
 */

import { ACCOUNT_UI_NAMESPACE, accountUiResources } from '@speed/account-ui'
import {
  bootstrapSpeedApp,
  type SpeedAppBootstrap,
} from '@speed/product-shell/bootstrap'
import { TENANCY_UI_NAMESPACE, tenancyUiResources } from '@speed/tenancy-ui'
import { AppView } from './app.js'
import { AppServicesProvider } from './app-services.js'
import {
  REFERENCE_APP_NAMESPACE,
  referenceAppResources,
} from './resources.js'
import { clearNotesDraft } from './views/notes-draft.js'

/** What a page's bootstrap produced: the mounted root, the i18n
 * instance, the query client and the session, for hosts and harnesses
 * that act on the composition. */
export type ReferenceAppBootstrap = SpeedAppBootstrap

/**
 * Builds the whole app composition into the given container. Calling it
 * twice into one container double-mounts; a page bootstraps exactly
 * once.
 */
export function bootstrapReferenceApp(
  container: Element,
): ReferenceAppBootstrap {
  return bootstrapSpeedApp(container, {
    namespaces: [
      { namespace: TENANCY_UI_NAMESPACE, resources: tenancyUiResources },
      { namespace: ACCOUNT_UI_NAMESPACE, resources: accountUiResources },
      {
        namespace: REFERENCE_APP_NAMESPACE,
        resources: referenceAppResources,
      },
    ],
    providers: [
      ({ session, api }, children) => (
        <AppServicesProvider session={session} api={api}>
          {children}
        </AppServicesProvider>
      ),
    ],
    view: <AppView />,
    sessionEnded: (context) => {
      context.evictAllQueries()
      clearNotesDraft()
    },
  })
}
