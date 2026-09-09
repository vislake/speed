# AGENTS.md — @speed/product-shell

## What this package is

The tenant-facing assembly shell — the top of the dependency graph on the
web side. It composes three
packages that must never import each other into one ready-to-copy front
door: `@speed/layout-kit`'s `AppShell` (the authenticated frame),
`@speed/auth-ui`'s `SignInScreen`/`SignOutButton`/`SessionEndedScreen`
(the pre-auth and ended surfaces, passed in by the host) and
`@speed/auth-core`'s hooks (the session snapshot the machine reads). It is
the one tier whose package code is allowed to consume `auth-core` hooks —
`auth-ui`, `layout-kit` and everything below them never do — and it stops
there: the shell reads the snapshot and renders a branch; it never calls a
session operation, never fetches, never navigates.

The public surface is four names: `ProductShell` and `ProductShellProps`
plus the `PRODUCT_SHELL_NAMESPACE` / `productShellResources` pair every
sibling ships for its namespace. The shell renders one string of its own —
the polite `announcements.sessionEnded` announcement of the session-ended
flip, read from the product-shell namespace through `@speed/i18n` — and no
other copy: every built-in string on screen in the authenticated branch
comes from the layout-kit namespace (AppShell's frame), every one in the
default ended view from the auth-ui namespace (`SessionEndedScreen`), all
registered by the host, who registers them for the underlying packages
anyway. The announcement renders only when the product-shell namespace is
registered; an unregistered host keeps the focus half of the a11y
contract and never sees raw key text (a missing namespace is a host
configuration state, not a missing key, so the missing-key discipline must
not fire for it).

## The machine (the one thing this package decides)

`ProductShell` renders one of three branches, checked in this order:

1. `authenticated` snapshot → the `AppShell` frame around `children`.
2. anonymous snapshot and the app was reached this mount
   (`reachedApp` local state) → the host's `sessionEnded` node, or the
   default `SessionEndedScreen` (from `@speed/auth-ui`) when no slot is
   given. Only the *default* screen gets the internal wiring that returns
   the viewer to the sign-in view on its action — and only when a sign-in
   view exists to return to: with no `signIn` slot the reset would land
   on the deliberately-blank fresh-visitor branch, a whitescreen dead
   end, so the action keeps the viewer on the ended screen and the
   host's own composition (its blank-branch pairing, or its own
   `sessionEnded` node) owns the way back into the app.
3. anonymous and the app was never reached → the host's `signIn` node,
   or nothing when no slot is given (the shell deliberately ships no
   default sign-in surface: the channel mix is a host product decision).

Branch 2 must stay ahead of branch 3: a signed-out user who was inside the
app must never fall back to a fresh-visitor sign-in. `reachedApp` is
component-local state — it resets on unmount and is not a persistence
layer; the *session's* authenticated state lives in `@speed/auth-core` and
is the only authority the machine reads.

The machine is a whole-page switch, so it owns the two a11y duties a page
swap carries, in the sibling family's shape: every branch
flip moves focus into the branch's own container — the branches render
inside one focusable, non-tab-stop wrapper each, never into a slot the
host may not be able to reach — and every flip into the session-ended
view announces itself through the sr-only `role="status"` region the
ended branch carries (the shell cannot tell a server-side death from an
explicit sign-out — the same snapshot flip — and both land the viewer on
a screen the shell itself placed there; the flips the user's own action
aimed at, whose destinations announce themselves, move focus only). The
region's text is set by the flip effect only after the branch committed
with an empty region, so the announcement lands as an insertion into a
live region already in the tree (the FileUploader pattern, not a region
that mounts pre-filled). Focus never moves on the first render — a page
load's focus belongs to the host — and never on a re-render that leaves
the branch unchanged.

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
- **Dependencies stop at `auth-core`, `auth-ui`, `layout-kit` and
  `i18n`.**
  Everything the shell renders arrives through those four: `AppShell`
  and its frame strings through layout-kit, the ended screen through
  auth-ui, the snapshot through auth-core, and the one sentence the
  shell speaks itself — the session-ended announcement — through `i18n`,
  the single exception to the dependency floor, earned by the a11y
  duty the announcement performs: no other package edge is
  permitted here, and `ui-kit`, `api-client` and `api-sdk` stay out —
  an extra edge beyond i18n is how a shell quietly starts depending on
  machinery it must stay agnostic to — a platform-staff sibling shell on
  this same tier must be able to reuse this package's dependency floor.
  Test-only needs go in
  `devDependencies` — the suites' `@speed/tenancy-ui` is exactly that: a
  composition partner of journey code, never of the package's own
  imports.
- **All chrome props pass through to `AppShell` untouched.** The shell
  must not reinterpret, reorder or path-match `navItems` — their
  `selected` state is host-computed, per layout-kit's contract, and a
  shell that starts deciding selection has begun routing. `navItems`,
  `header`, `headerActions`, `userMenu`, the drawer controls and `sx`
  are picked off the props and forwarded verbatim; adding a prop means
  adding it to the pass-through `Pick<AppShellProps, ...>`, never to a
  parallel interpretation.
- **One sentence of the shell's own, nothing else.** The only strings
  this package renders are the `announcements.sessionEnded` key pair in
  its own bilingual bundle (`src/locales/`, identical leaf key sets);
  every other built-in belongs to layout-kit's and auth-ui's
  namespaces, everything else is host content (`navItems` labels,
  `header`, `children`) and therefore the host's i18n responsibility.
  The `speed/no-literal-text` rule (workspace config) enforces this over
  `src/`; a bare text node or an inline attribute string in package code
  is a review error. Do not grow the `locales/` directory past the
  announcement keys: a second sentence of shell-owned copy is a product
  decision that belongs in a host slot, not in the machine.
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
- **Framework peers stay peers.** `react`, `react-dom`, `@mui/material`,
  `@emotion/*` and `react-hook-form` are peer (required) dependencies —
  react-hook-form because the paired auth-ui sign-in family renders with
  it, re-declared here the way every package whose surface pulls a
  sibling's peer in re-declares it; `auth-core`, `auth-ui`, `i18n` and
  `layout-kit` are regular dependencies. The manifest contract is pinned
  by `src/package.json.test.ts`.

## Testing

Unit tests are vitest + jsdom, one file per source file under `src/`,
shared helpers only in `test-utils/`. `renderWithProviders` mounts the
unit under the real host tree — `I18nextProvider` around `ui-kit`'s
`AppThemeProvider`, fresh i18n instance per call with exactly the four
namespaces the shell's own rendered branches draw on (`ui-kit`,
`layout-kit`, `auth-ui` and `product-shell`'s own); the journey suites
build their instance from the same helper and register tenancy-ui's
namespace on it (the fifth, exactly as a host composing the switcher
must) — and
`test-utils/setup.ts` installs the desktop `matchMedia` stub the frame's
responsive drawer needs. Bilingual assertions import the shipped sibling
bundles relatively (`../../auth-ui/src/locales/zh-CN.json`, the
layout-kit and tenancy-ui equivalents, and this package's own
`../locales/zh-CN.json`) — never an inline translation. Four suites:

- `src/components/ProductShell.test.tsx` — the view machine. It drives
  real sessions over the real-client rig (a genuine `@speed/api-client`
  over a scripted fetch answering genuine `Response` objects, bound
  through the api-sdk runtime seam exactly as a host binds one) and
  asserts the three branches, both slot overrides, the fail-closed
  unattached shape, the dead-end regressions (a host without a `signIn`
  view keeps the ended screen when its action is activated — never a
  whitescreen — and the frame still returns on the next sign-in), the
  flip a11y contract (every branch flip moves focus into the new
  branch's container; the session-ended flip additionally announces
  through the `role="status"` region; a host that has not registered the
  product-shell namespace gets focus and no raw key text), and axe (the
  `region` rule left enabled for the authenticated frame — layout-kit's
  page-chrome precedent — and disabled for the ended-branch scan, whose
  screen auth-ui's own suite scans; `color-contrast` disabled, jsdom
  computes no layout).
- `src/package.json.test.ts` — the manifest's peer contract: the
  react-hook-form declaration (range and workspace-pinned devDependency)
  this package's surface requires.
- `src/usage-example.test.tsx` — compiles and executes the README's
  Quick start composition (the five-namespace bootstrap (`ui-kit`,
  `layout-kit`, `auth-ui`, `product-shell`'s own and `tenancy-ui`'s),
  one attached session, the documented slots — the `userMenu`
  composing tenancy-ui's `TenantSwitcher` beside `SignOutButton`, fed
  by `useCurrentTenant`)
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

- **Permission gating.** This package never consumes layout-kit's
  `RouteGuard` and never attaches permission lists to the session: the
  gate is host composition in `children`, and its evidence is the
  fixture host of the gated-journey suite (Testing), not shell code.
  The shell must not invent the gate.
- **Tenant switcher in package code.** The switcher never appears in
  this package's code; hosts compose tenancy-ui's `TenantSwitcher` into
  the `userMenu` slot, and that composition — including the host's
  re-attach duty on switch — is packaged evidence of the usage-example
  and gated-journey suites (Testing). A first-party switcher surface is
  not shipped; the package must not grow one.
- **Platform-facing shell.** `admin-shell`, the same tier for platform
  staff, is not shipped; this package stays free of anything
  platform-shaped so the two shells can share their foundations.
- **Browser automation.** The reference app's consumer shell
  (`examples/reference-app/web`) is a real composed consumer of this
  package: `main.tsx`'s bootstrap registers `PRODUCT_SHELL_NAMESPACE`
  alongside its six sibling namespaces, renders `ProductShell` as the
  app's view machine over the real session and client composition, and
  the server serves the production build from disk under `APP_WEB_DIST`.
  What does not exist is browser automation driving that served page —
  the e2e pipeline is a gated stub, so rendering under test harnesses
  and the dev-server page are the shipped browser story; the package's
  own suite (`src/usage-example.test.tsx`) stays the in-form consumer
  proof.
- **Storybook.** No preview harness exists; components are covered by
  jsdom tests + axe, and color-contrast verification is done nowhere —
  jsdom computes no layout, and no browser-side visual harness exists —
  same as the packages below.
