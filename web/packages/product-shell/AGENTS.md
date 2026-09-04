# AGENTS.md — @speed/product-shell

## What this package is

The tenant-facing assembly shell — the top of the dependency graph on the
web side (docs/internal/12-frontend.md's shell tier). It composes three
packages that must never import each other into one ready-to-copy front
door: `@speed/layout-kit`'s `AppShell` (the authenticated frame),
`@speed/auth-ui`'s `SignInScreen`/`SignOutButton`/`SessionEndedScreen`
(the pre-auth and ended surfaces, passed in by the host) and
`@speed/auth-core`'s hooks (the session snapshot the machine reads). It is
the one tier whose package code is allowed to consume `auth-core` hooks —
`auth-ui`, `layout-kit` and everything below them never do — and it stops
there: the shell reads the snapshot and renders a branch; it never calls a
session operation, never fetches, never navigates.

The public surface is exactly two names: `ProductShell` and
`ProductShellProps`, both from `src/index.ts`. There is no namespace, no
`resources.ts`, no locale directory: the shell renders zero text of its
own. Every built-in string on screen in the authenticated branch comes from
the layout-kit namespace (AppShell's frame), every one in the default ended
view from the auth-ui namespace (`SessionEndedScreen`) — both registered by
the host, who registers them for the underlying packages anyway.

## The machine (the one thing this package decides)

`ProductShell` renders one of three branches, checked in this order:

1. `authenticated` snapshot → the `AppShell` frame around `children`.
2. anonymous snapshot and the app was reached this mount
   (`reachedApp` local state) → the host's `sessionEnded` node, or the
   default `SessionEndedScreen` (from `@speed/auth-ui`) when no slot is
   given. Only the *default* screen gets the internal wiring that returns
   the viewer to the sign-in view on its action.
3. anonymous and the app was never reached → the host's `signIn` node,
   or nothing when no slot is given (the shell deliberately ships no
   default sign-in surface: the channel mix is a host product decision).

Branch 2 must stay ahead of branch 3: a signed-out user who was inside the
app must never fall back to a fresh-visitor sign-in. `reachedApp` is
component-local state — it resets on unmount and is not a persistence
layer; the *session's* authenticated state lives in `@speed/auth-core` and
is the only authority the machine reads.

## Non-negotiable rules

- **Exactly one component, one decision.** Do not grow a second export,
  a config object, a `useProductShell` hook or a routing layer into this
  package. A change that wants to make the shell do more than branch
  should be a `children` concern (host-owned) or a new prop on the one
  component — and the prop must be a value or element, never a callback
  the package invokes to learn the session, which it already reads itself.
- **The shell reads the session, never drives it.** `useAuthState` is the
  whole of this package's contact with auth-core. No login/logout/refresh/
  switch calls anywhere in package code: those are event-handler
  operations of the host-supplied children (`SignInScreen`'s forms,
  `SignOutButton`'s click). `attachSession` stays host-side, called once
  before render; auth-core's last-bind-wins contract means this package
  must never attach on its own. Before any attach — and after a logout —
  the hooks fail closed to the anonymous snapshot, so an unattached shell
  can only ever render the sign-in branch (or nothing): that fail-closed
  behaviour is contract and is pinned by the suite.
- **Dependencies stop at `auth-core`, `auth-ui`, `layout-kit`.**
  Everything the shell renders arrives through those three: `AppShell`
  and its frame strings through layout-kit, the ended screen through
  auth-ui, the snapshot through auth-core. Never add `ui-kit`, `i18n`,
  `api-client` or `api-sdk` as a dependency of this package — layout-kit
  and auth-ui already carry them, and an extra edge here is how a shell
  quietly starts depending on machinery it must stay agnostic to (the
  platform-facing sibling shell will reuse this same package later).
  Test-only needs go in `devDependencies` — the suites' `@speed/tenancy-ui`
  is exactly that: a composition partner of journey code, never of the
  package's own imports.
- **All chrome props pass through to `AppShell` untouched.** The shell
  must not reinterpret, reorder or path-match `navItems` — their
  `selected` state is host-computed, per layout-kit's contract, and a
  shell that starts deciding selection has begun routing. `navItems`,
  `header`, `headerActions`, `userMenu`, the drawer controls and `sx`
  are picked off the props and forwarded verbatim; adding a prop means
  adding it to the pass-through `Pick<AppShellProps, ...>`, never to a
  parallel interpretation.
- **Zero text, zero resources, zero registration.** The shell renders no
  strings: built-ins belong to layout-kit's and auth-ui's namespaces,
  everything else is host content (`navItems` labels, `header`,
  `children`) and therefore the host's i18n responsibility. The
  `speed/no-literal-text` rule (workspace config) enforces this over
  `src/`; a bare text node or an inline attribute string in package code
  is a review error. Do not add a `locales/` directory to this package.
- **No network and no session calls in package code.** The shell adds no
  HTTP surface of its own; the requests the composed views make are
  session operations travelling through the host's bound api-client. If
  a change looks like it wants to fetch or call an endpoint, that is
  scope drift — stop and read the README's "What this shell does not do
  (yet)" section.
- **The public API is frozen by convention.** Lockstep versioning makes
  an exported-signature change a breaking release; extend the surface
  only intentionally. A public change ships, in one commit: the code,
  its tests, this AGENTS.md, the README, and the compiled usage example
  when the documented composition changes.
- **Framework peers stay peers.** `react`, `react-dom`, `@mui/material`
  and `@emotion/*` are peer (required) dependencies; `auth-core`,
  `auth-ui` and `layout-kit` are regular dependencies.

## Testing

Unit tests are vitest + jsdom, one file per source file under `src/`,
shared helpers only in `test-utils/`. `renderWithProviders` mounts the
unit under the real host tree — `I18nextProvider` around `ui-kit`'s
`AppThemeProvider`, fresh i18n instance per call with exactly the three
namespaces a real host registers (`ui-kit`, `layout-kit`, `auth-ui`);
the journey suites build their instance from the same helper and
register tenancy-ui's namespace on it (the fourth, exactly as a host
composing the switcher must) — and `test-utils/setup.ts` installs the
desktop `matchMedia` stub the frame's responsive drawer needs. Bilingual
assertions import the shipped sibling bundles relatively
(`../../auth-ui/src/locales/zh-CN.json`, the layout-kit and tenancy-ui
equivalents) — never an inline translation. Three suites:

- `src/components/ProductShell.test.tsx` — the view machine. It drives
  real sessions over the real-client rig (a genuine `@speed/api-client`
  over a scripted fetch answering genuine `Response` objects, bound
  through the api-sdk runtime seam exactly as a host binds one) and
  asserts the three branches, both slot overrides, the fail-closed
  unattached shape, and axe (the `region` rule left enabled: the
  authenticated branch is a full page, layout-kit's page-chrome
  precedent; `color-contrast` disabled, jsdom computes no layout).
- `src/usage-example.test.tsx` — compiles and executes the README's
  Quick start composition (the four-namespace bootstrap, one attached
  session, the documented slots — the `userMenu` composing tenancy-ui's
  `TenantSwitcher` beside `SignOutButton`, fed by `useCurrentTenant`)
  and pins the journey's requests in order, bodies included, through
  the switch turn, so the documented usage cannot drift from the API.
- `src/gated-journey.test.tsx` — the host-side permission-gating
  composition over the same frame, as packaged evidence. Its fixture
  host (a view-id mini-router in `children` — the README's documented
  host duty played for real) gates every destination with layout-kit's
  `RouteGuard`, fed a status it derives from auth-core's `usePermission`
  over lists it attaches from role-load responses and re-attaches on
  switch (auth-core's survival rules drop the tenant-domain list at the
  switch commit). The suite journeys a switch whose pending→allowed
  reload drops and restores the lists, a denial spell whose refresh
  keeps the guard settled so `onDenied` fires exactly once across
  re-renders with the default ui-kit noPermission fallback, a refused
  switch to a non-member tenant (tenancy-ui's error text, snapshot
  unchanged, retryable) and a server-side session death converging to
  the session-ended screen.

`test-utils/` copies auth-ui's real-client rig (same fetcher shape, same
`jsonResponse` over genuine Response objects) and stays in lockstep with
it by hand; it additionally records each call's body beside its method,
path and `authorization`, which the switch-turn pins need. `makePair`
rides along, with the token-issuing overrides a multi-tenant journey
scripts (a switch answers with an access token and no refresh token —
the authn API's shape). The journey suites compose tenancy-ui's
switcher, so tenancy-ui sits in `devDependencies`, aliased to its
sources by `tsconfig.json`'s `paths` and `vitest.config.ts`'s alias list
(lockstep, as with every sibling); package code never imports it.
`attachSession` is module-level and last-bind-wins, so it persists across
tests in a file: a test that needs an anonymous or specific session must
bind one explicitly before rendering.

## Deferrals (recorded, do not re-open silently)

- **Permission gating.** This package still never consumes layout-kit's
  `RouteGuard` and never attaches permission lists to the session: the
  gate is host composition in `children`, and its evidence is the
  fixture host of the gated-journey suite (Testing), not shell code.
  When a later round wires the shell's own guard, this package may grow
  the gate — until then it must not invent one.
- **Tenant switcher in package code.** The switcher never appears in
  this package's code; hosts compose tenancy-ui's `TenantSwitcher` into
  the `userMenu` slot, and that composition — including the host's
  re-attach duty on switch — is packaged evidence of the usage-example
  and gated-journey suites (Testing). A first-party switcher surface is
  a later round's product decision; the package must not grow one.
- **Platform-facing shell.** `admin-shell` is the same tier for platform
  staff, a later round; this package must stay free of anything
  platform-shaped so the two shells can share their foundations.
- **Reference-app consumer.** The reference app's consumer shell
  (`examples/reference-app/web`) mounts this package: `main.tsx`'s
  bootstrap renders `ProductShell` as the app's view machine over the
  real session and client composition, making the shell this package's
  composed consumer; the required consumer proof stays at the package
  level (`src/usage-example.test.tsx`), the same honesty standard
  `ui-kit` and `layout-kit` used before any shell consumer existed.
  The browser page leg is M4's html-runner/e2e work.
- **Storybook.** No preview-harness round exists yet; components are
  covered by jsdom tests + axe, and color-contrast verification awaits a
  browser-side visual round, same as the packages below.
