# @speed/tenancy-ui

The tenant-switch affordance of a frontend built on speed: one
controlled component, `TenantSwitcher`, that renders the host's current
tenant as a trigger and the host's tenant list as a menu, and switches
the session to the picked tenant through `session.switchTenant`. The
component sits at the same tier as `ui-kit` and `auth-ui` -- it is
deliberately auth-aware (it drives an `@speed/auth-core` session passed
in as a prop) but entirely controlled: it never consumes the auth-core
hooks, never reads session state beyond the single operation it drives,
never attaches or persists a session, never navigates and never touches
the network directly -- every request is the session's own generated
operation over the host-bound client. A successful switch is quiet and
fires the host's `onSwitched` callback exactly once, after the commit;
everything that happens next -- refetching the tenant's data, cleaning
the previous tenant's query cache, re-attaching `/me`-derived
permission lists -- is the host's. Every built-in string renders from
the bilingual `tenancy-ui` namespace registered through `@speed/i18n`.

## What ships

| Module | Exports |
|---|---|
| `TenantSwitcher.tsx` | `TenantSwitcher`, `TenantSwitcherProps`, `TenantOption` |
| `resources.ts` | `TENANCY_UI_NAMESPACE`, `tenancyUiResources` |

Everything else (`src/internal/`) is shared plumbing -- the
code-to-text error resolver, the failure banner, the translation hook
-- and is deliberately not exported.

## Quick start

A host composes the switcher over the session-and-client wiring
`@speed/auth-core`'s README documents. The bootstrap has four parts:
the bilingual i18n instance with both namespaces a rendered component
can read, the client and session over one memory access-token store,
the session attached to the hooks, and the host gate that renders the
sign-in affordance while anonymous and the app -- with the switcher in
its header -- once authenticated. This package is the auth-agnostic
neighbour of `auth-ui` at the same tier, never its importer, so the
sign-in affordance below is host content; the same-tier rule that keeps
`auth-ui` from depending on `billing-ui` keeps this package's consumer
off `auth-ui`'s catalog.

```tsx
import {
  createClient,
  createMemoryAccessTokenStore,
} from '@speed/api-client'
import { bindRequestFn } from '@speed/api-sdk/runtime'
import {
  createAuthSession,
  attachSession,
  useAuthState,
  useCurrentTenant,
} from '@speed/auth-core'
import { createI18n, I18nextProvider, registerNamespace } from '@speed/i18n'
import {
  AppThemeProvider,
  UI_KIT_NAMESPACE,
  uiKitResources,
} from '@speed/ui-kit'
import { TENANCY_UI_NAMESPACE, tenancyUiResources, TenantSwitcher } from '@speed/tenancy-ui'
import type { TenantOption } from '@speed/tenancy-ui'

// 1. The session and the client share one memory access-token store.
//    refreshAccessToken is the session's silent refresh: an expired-token
//    401 on any request runs it once and retries with the fresh token.
const store = createMemoryAccessTokenStore()
const session = createAuthSession(store)
bindRequestFn(
  createClient({
    baseUrl: 'https://api.example.com',
    fetch: fetchImpl, // the host's fetch implementation
    accessTokenStore: store,
    refreshAccessToken: () => session.refresh(),
  }),
)

// 2. The hooks read the attached session (last bind wins).
attachSession(session)

// 3. The bilingual instance, both namespaces registered exactly once.
//    The switcher renders no ui-kit chrome itself -- its strings and
//    its inline error banner are plain MUI elements under the
//    tenancy-ui namespace. The ui-kit namespace is registered because
//    a host app composes under ui-kit (the AppThemeProvider below) and
//    renders ui-kit chrome, which reads that namespace.
//    registerNamespace validates identical leaf key sets before it
//    mutates.
const i18n = createI18n({
  supportedLanguages: ['zh-CN', 'en-US'],
  defaultLanguage: 'zh-CN',
  storage: null,
  urlParameterName: null,
  navigatorLanguages: [],
})
registerNamespace(i18n, TENANCY_UI_NAMESPACE, tenancyUiResources)
registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)

// 4. The host's tenant list -- which tenants the signed-in user may
//    switch between is the host's data, fetched through its own
//    generated operations. The switcher renders it verbatim.
const tenants: TenantOption[] = [
  { id: 'tenant-1', name: 'Sunshine Dental' },
  { id: 'tenant-2', name: 'Bright Smile Clinic' },
  { id: 'tenant-3', name: 'Harbor View Orthodontics' },
]

// The host gate: anonymous viewers see the host's sign-in affordance;
// authenticated viewers see the app with the switcher in the header,
// currentTenantId flowing from the hook that reads the session snapshot,
// so a committed switch re-renders the trigger onto the new tenant.
function App() {
  const auth = useAuthState()
  const currentTenant = useCurrentTenant()
  if (auth.state === 'anonymous') {
    return <button type="button" onClick={() => void session.loginWithPassword({
      identifier: 'alice@example.com', password: 's3cret-pass',
    })}>Sign in</button>
  }
  return (
    <div>
      <header>
        <TenantSwitcher
          session={session}
          tenants={tenants}
          currentTenantId={currentTenant?.tenantId ?? null}
          onSwitched={(tenantId) => {
            // The host's own post-switch work: refetch the tenant's
            // data, and under the tenant-namespaced query-key rule of
            // docs/internal/12-frontend.md, remove the previous
            // tenant's cache so repeated switches do not accumulate:
            //   queryClient.removeQueries({ queryKey: ['tenant', oldId] })
            // Permission lists are re-attached the same way (a switch
            // commit drops the tenant-domain set by auth-core design).
          }}
        />
      </header>
      <main>{/* the current tenant's workspace */}</main>
    </div>
  )
}

export function Root() {
  return (
    <I18nextProvider i18n={i18n}>
      <AppThemeProvider i18n={i18n}>
        <App />
      </AppThemeProvider>
    </I18nextProvider>
  )
}
```

This exact composition is compiled and executed by the package suite
(`src/usage-example.test.tsx`) over a real `@speed/api-client`: its
fetch stand-in answers with genuine `Response` objects, and the journey
signs in, switches to a second tenant (the trigger label flips because
the host hook re-reads the committed snapshot), has the switch back
refused with `authn.tenant_membership_required` (the alert renders the
answer's code text and nothing changes -- the store keeps its token and
the trigger stays ready), and retries the same switch into the first
tenant again. The whole exchange -- one login and three switch attempts
-- is pinned in order, including each switch's bearer token and request
body. The documented usage cannot drift from the API.

## TenantSwitcher

`TenantSwitcher` props:

| Prop | Type | Notes |
|---|---|---|
| `session` | `AuthSession` (required) | the live session whose `switchTenant` the menu drives |
| `tenants` | `readonly TenantOption[]` (required) | the host-supplied list; `TenantOption` is `{ id: string; name: string }`, both readonly |
| `currentTenantId` | `string \| null` (required) | the tenant the principal is signed into, per the host's data source (typically `useCurrentTenant()`) |
| `onSwitched` | `(tenantId: string) => void` (optional) | fired exactly once after a switch commits; the host's data-reload hook |

Behaviour, all of it controlled and test-pinned:

- **The trigger shows the current tenant.** Its label is the matched
  `tenants` row's `name` -- host data, rendered verbatim, never
  translated. With no matching row, or with `currentTenantId` null, the
  trigger is disabled and shows the `tenantSwitcher.noCurrentTenant`
  text.
- **Opening the list shows every tenant; the current row is disabled
  and can never re-trigger a switch.** Its `onClick` guard returns
  before the session operation could start, so even a synthetic click
  on the disabled row changes nothing.
- **Picking another row calls `session.switchTenant(id)`.** The request
  is the generated operation over the host-bound client; the tenant
  travels in the switch request body, never in a header (tenant context
  travels in the access token, per the frontend standards).
- **While the switch is in flight the trigger is disabled and a
  `role="status"` notice renders the `tenantSwitcher.switching` text**,
  so the affordance never queues a second switch behind the first.
- **A successful switch is quiet**: the list closes, the trigger
  re-enables and no alert renders. `onSwitched` fires exactly once,
  after the commit -- never for a failed switch.
- **A refused switch renders the answer's code text in one
  `role="alert"` banner and changes nothing locally**: the store keeps
  its token, the trigger stays enabled on the same current tenant, and
  the next pick retries -- a retry clears the previous alert. Non-2xx
  API errors, and the `client.*` transport failures, all land here; a
  code outside the whitelist below renders the `errors.unknown`
  fallback, never a raw key.
- **The switcher never knows the current tenant on its own.** The
  `currentTenantId` prop is the host's data; the component exists to
  change it and reports the change through `onSwitched`. It never reads
  session state beyond the switch call and never re-attaches permission
  sets -- those survival rules are auth-core's, the re-attach itself
  the host's.

## Text and i18n

Every built-in string ships in the bilingual `tenancy-ui` namespace,
twelve leaves per language under two sections:

- `tenantSwitcher` -- the two component states: `noCurrentTenant` and
  `switching`.
- `errors` -- the code-to-text table of the tenant-switch surface,
  nested per source: `errors.authn.*` (the session-lifecycle answers a
  switch can draw), `errors.client.*` (transport failures) and the
  `errors.unknown` fallback.

`registerNamespace` enforces the standing discipline -- canonical
language keys, full coverage, identical leaf key sets across languages
-- before it mutates, so a key added to one language file and forgotten
in the other fails registration. The suite pins the bilingual values by
importing the shipped JSON bundles, never by inlining language.

### Error text: the reachable-code whitelist

The switch surface resolves a failure to text only for the codes it can
actually draw, nine of them:

| Code | When |
|---|---|
| `authn.tenant_membership_required` | the switch was refused: the principal is not a member of the target tenant |
| `authn.session_not_found` | the session the switch travelled on no longer exists server-side |
| `authn.session_revoked` | the session was revoked |
| `authn.refresh_token_invalid` | a refresh was refused -- the session is over |
| `authn.refresh_token_reused` | the held token was replayed -- the session is over |
| `authn.token_expired` | the presented access token expired |
| `client.network` | no connection |
| `client.timeout` | the request timed out |
| `client.protocol` | the service answered outside the contract |

Everything else -- any other `authn.*`, any `authn.http.<status>`, any
unknown string -- renders the `errors.unknown` fallback, so a raw code
can never appear on screen. The whitelist is pinned in both directions
against the bundles by the internal error-text suite: a code added to
the whitelist without its two bundle keys, or a bundle key added
without its whitelist code, fails the suite.

The `errors.authn.*` and `errors.client.*` texts are deliberate,
verbatim copies of the `auth-ui` error texts for the same codes:
same-tier packages cannot import one another's catalogs, and two
versions of one server code's text must not drift apart in the product.
The copy is noted in `resources.ts`, and the error-text suite imports
the auth-ui bundles themselves as test data, so a divergence from the
auth-ui bundle is a translation bug that fails that suite.

## Accessibility

The trigger is a `Button` with `aria-haspopup="menu"`, `aria-expanded`
and `aria-controls` on the open list; the menu renders MUI's menu
semantics, with the current-tenant row `disabled` -- announced and
skipped by assistive tech, and inert even to synthetic clicks. The
in-flight notice is a `role="status"` live region (announced without
interrupting), the failure banner a `role="alert"`. The component suite
runs axe over the open list on every relevant state. As in `ui-kit`
and `auth-ui`, `color-contrast` stays axe-disabled in jsdom (no real
paint) and is verified browser-side in a later round.

## Testing

Unit tests are vitest + jsdom, one file per source file under `src/`,
shared helpers only in `test-utils/`. The vitest config aliases the
`@speed/*` specifiers onto the sibling packages' `src` entries, so
tests run against live sources -- no test file imports another
package's `dist/`. Three layers of helpers, mirroring
`@speed/auth-core`'s and `auth-ui`'s:

- `render.tsx` -- `renderWithProviders` mounts a unit under the tree a
  real host builds (`I18nextProvider` around `AppThemeProvider`), with
  a fresh bilingual instance per call and both namespaces a rendered
  component can read registered. Bilingual assertions import the
  shipped bundles (`../locales/zh-CN.json`, `en-US.json`), never inline
  a language literal.
- `session-harness.ts` -- `makeHarness` drives component tests'
  sessions: a scripted fake `RequestFn` bound through the same
  `bindRequestFn` seam a host's real client binds, a real session over
  a fresh memory store, and assertions on observable state only (store
  tokens, request bodies, raw `ApiError`s). The endpoint constants are
  the same literal keys auth-core's harness uses.
- `real-client.ts` -- the journey rig: `makeRealClientRig` builds a
  real `@speed/api-client` `createClient` whose fetch stand-in answers
  from a script with genuine `Response` objects, recording every
  request's method, path, authorization header and serialized body.

`src/resources.test.ts` pins the bundle discipline, the internal
`error-text` suite pins the whitelist pairing in both directions and
the verbatim copy against auth-ui's own bundles (imported as test
data), and `src/TenantSwitcher.test.tsx` pins the component behaviour
above -- every text expectation reads the bundle values.
`src/usage-example.test.tsx`
compiles and executes the Quick start composition end to end over the
real-client rig (four requests in a pinned order: the password sign-in
and the three switch attempts, with each switch's bearer token and
`{ tenant_id }` body asserted), discharging in form the runtime
consumption of the switch endpoint -- the browser-and-real-server leg
stays with the reference-app shells.

## Dependencies

| Package | Kind | Why |
|---|---|---|
| `react`, `react-dom` | peer (required, ^18 or ^19) | the host owns the React tree |
| `@mui/material` | peer (required, ^9) | the `Button`/`Menu`/`MenuItem` primitives and the ambient theme |
| `@emotion/react`, `@emotion/styled` | peer (required, ^11) | MUI's own runtime requirements |
| `@speed/auth-core` | dependency | `TenantSwitcherProps.session` is the `AuthSession` type, so a consumer resolving the published `.d.ts` needs the specifier declared as a dependency; today `src/` imports it type-only |
| `@speed/i18n` | dependency | the namespace registration and the translation hook every string renders through |

`@speed/ui-kit` (theme provider used only by the test tree),
`@speed/api-client` and `@speed/api-sdk` stay devDependencies: no
`src/` code references them and no public type of this package does
either. No direct HTTP exists anywhere in `src/` -- every request is
the session's generated switch operation over the shared seam, and the
workspace's `speed/no-direct-http` rule enforces that (this package is
simply absent from the rule's one whitelist, `packages/api-client`);
the `speed/no-literal-text` rule enforces the namespace discipline over
`src/` the same way it does in every package.

## Known limitations

- **The trigger label and list entries are host text.** The switcher
  renders the `tenants` rows verbatim; tenant names are the host's
  data, translated by nobody. The built-in strings cover only the
  component's own states.
- **The current tenant is the host's fact.** `TenantSwitcher` shows
  `currentTenantId`'s row and reports a change through `onSwitched`; a
  host that never updates the prop after a switch shows a stale
  trigger. The quick start's `useCurrentTenant` flow is the intended
  shape.
- **Permission sets and query caches are the host's to move.** A switch
  commit drops the tenant-domain permission set by auth-core design
  (the survival rules in its README), and tenant-namespaced query
  caches accumulate unless the host removes the previous tenant's --
  both are documented in the quick start's `onSwitched`, never done
  here.
- **Session state does not survive a page load** -- auth-core's own
  known limitation, inherited by any host of this component: reloading
  starts anonymous and the user signs in again. See `@speed/auth-core`'s
  README for the full account.

## Deferrals and recorded decisions

- **Reference-app consumer**: `examples/reference-app` has no frontend
  directory yet (Go-only today). The runtime end-to-end consumption of
  the switch endpoint is discharged in form at the package level --
  `src/usage-example.test.tsx` drives the composed switcher over a real
  `@speed/api-client` bound through the same seam a host binds, with a
  scripted fetch answering genuine `Response` objects -- the same
  honesty standard every package here used before a shell consumer
  existed. The remaining leg, a browser driving a real server (and the
  shell's tenant list itself, which the product-shell rounds wire),
  lands with the reference-app shells.
- **The tenant list has no in-package source.** No endpoint answers
  "which tenants may this principal switch between" in the shipped
  surface, so the list is host data by contract. When a membership
  listing surface exists, hosts compose with it; nothing here changes.
- **Auth-core hooks stay host-side by design.** The switcher takes the
  session and the current tenant as props and reports through
  callbacks; the host observes transitions with `useAuthState` and
  `useCurrentTenant`. A component that consumed the hooks would have to
  be mounted under the host's `attachSession` anyway.
- **Error texts deliberately duplicate auth-ui's.** Same-tier packages
  cannot import one another's catalogs; the verbatim copy and its
  drift risk are recorded in `resources.ts`, the error-text suite pins
  the pairing within this package's own bundles, and it pins every
  copied leaf against the auth-ui bundles imported as test data, so a
  drift between the packages fails the suite.
- **Storybook / browser-side visual verification**: no preview-harness
  round exists yet, same deferral `ui-kit`, `layout-kit` and `auth-ui`
  carry; `color-contrast` stays axe-disabled for the same jsdom reason
  and is verified browser-side in a later round.

## Development

From `web/packages/tenancy-ui`: `pnpm lint`, `pnpm typecheck`, `pnpm
test`, `pnpm build`. The build compiles `@speed/auth-core` and
`@speed/i18n` first (their declarations are what the published `.d.ts`
files reference) and then emits this package's `dist/`; `pnpm build`
from the `web/` root does the same for every package. Shared test
helpers live in `test-utils/`, bilingual fixtures are the shipped
locale files under `src/locales/`, and `src/usage-example.test.tsx`
compiles and executes the Quick start composition above, so the
documented usage cannot drift from the API.
