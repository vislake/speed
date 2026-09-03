# AGENTS.md — @speed/layout-kit

## What this package is

Shared app chrome at the same architectural tier as `@speed/ui-kit`:
`AppShell` (a responsive header, nav drawer and content region) and
`RouteGuard` (a route/content gate driven entirely by a host-injected
status). Both are fully controlled and props-driven, carry no
business or tenant semantics, and — the property that matters most for
this package — carry no opinion about *how* a host authenticates or
authorizes a user. `product-shell` (shipped, this package's first
consumer) and `admin-shell` (a later round) share this exact package
while wiring in two different authorization sources; that only stays
true if nothing here
ever imports a concrete one. Built-in user-facing strings live in the
bilingual `layout-kit` namespace; the repo's text discipline (both
languages, identical key sets, nothing inline) is enforced over this
package's `src` by the workspace's own `speed/no-literal-text` ESLint
rule. The public surface is `src/index.ts`; everything under
`src/internal/` is plumbing and deliberately not exported.

## Non-negotiable rules

- **`RouteGuard` must not import any concrete authentication or
  routing package.** Its authorization decision arrives as the
  `status: RouteGuardStatus` prop — a plain value the host computes and
  re-renders with, never a callback (`() => boolean` or similar) that
  this package invokes. This is the single most important rule in the
  package: an `auth-core` or router import here, however small, would
  force every consumer shell onto one authorization mechanism
  and defeat the reason this package exists as a separate layer from
  `product-shell`/`admin-shell`. If a change looks like it needs one,
  stop and push the decision back onto the host's `status` computation
  instead.
- **No dependency on `auth-core`, `api-client`, `api-sdk`, or any
  package that assumes a specific authentication mechanism.** Same
  rule as above, stated as a dependency-graph constraint: check
  `package.json` before adding anything, not just import statements in
  `RouteGuard.tsx`.
- **AppShell never does path-matching or routing logic.** `navItems`
  (including each item's `selected` state) is entirely host-computed;
  different hosts use different routers, and this package must work
  with all of them identically.
- **This package makes zero HTTP calls.** There is no scope in which
  it legitimately needs to — if a change wants to fetch anything, that
  is a sign scope has drifted; stop and re-read the package README's
  Deferrals section before adding a request anywhere. The workspace's
  `speed/no-direct-http` ESLint rule enforces this (this package is
  simply absent from its one whitelist, `packages/api-client`).
- **Text renders from the `layout-kit` namespace, from `@speed/ui-kit`'s
  own namespace (via `RouteGuard`'s default denied fallback), or from
  host props — never inline in code.** The `speed/no-literal-text` rule
  (workspace config at `web/eslint.config.mjs`, implementation and rule
  tests in `web/eslint-rules/`) enforces this over `src/`; package
  tests and `test-utils/` are exempt by config because fixture strings
  are data. Route new text through `useLayoutKitTranslation` instead of
  weakening the config.
- **The two locale files stay a bilingual pair.** A new built-in string
  adds one key to both `src/locales/zh-CN.json` and `en-US.json` in
  the same commit, with identical nested structure. Registration
  rejects drift at runtime (`registerNamespace` in the host and in
  `renderWithProviders`), and `tools/check_i18n_keys.py` checks the raw
  files in CI.
- **The namespace registers once, at host bootstrap — never inside the
  package.** Components only consume the registered namespace; the
  package never imports `@speed/i18n`'s `registerNamespace` outside
  `test-utils`.
- **Components are fully controlled.** No fetch, no data mutation, no
  business state, no implicit routing decisions. `AppShell`'s mobile
  drawer open state is the one allowed interaction-local exception
  (uncontrolled by default, promotable to controlled), the same
  carve-out `ui-kit`'s `ConfirmDialog` relies on for its double-confirm
  arm.
- **No direct `@speed/tokens` dependency.** `AppShell` reads
  `breakpoints.values` / `zIndex.values` through the ambient MUI theme
  a host's `AppThemeProvider` already builds (`useTheme()`), exactly as
  `ui-kit`'s own components do — not through a direct package import.
  Reintroducing one needs a concrete gap in the ambient theme, recorded
  the same way this decision is (README's Dependencies section).
- **`RouteGuard`'s one concrete coupling is `@speed/ui-kit`, and it
  stops at chrome.** Its default `deniedFallback` reuses `ui-kit`'s
  `EmptyState variant="noPermission"` rather than inventing a second
  "access denied" text block. Do not let this widen into any other
  `ui-kit` import that carries authorization semantics — `ui-kit`
  itself has none to offer, and none should be added to it on this
  package's behalf.
- **Accessibility is asserted, with recorded exceptions.** `AppShell`
  is page-level chrome (fixed header, nav drawer, main region), so its
  tests run axe with the `region` rule left **enabled** and its
  landmarks (`nav`, `header`/banner, `main`) explicitly asserted;
  `RouteGuard` gates per-widget host content, so its tests use the same
  `region`-disabled carve-out `ui-kit`'s own component tests use.
  `color-contrast` is disabled everywhere in this package's tests for
  the same reason `ui-kit` disables it: jsdom computes no layout or
  color, so a contrast result there is neither trustworthy nor
  actionable. Do not disable further rules without the same kind of
  recorded rationale.
- **Framework peers stay peers.** `react`, `react-dom`, `@mui/material`
  and `@emotion/*` are peer (required) dependencies — single copies in
  the host tree are what make theme/context identity work; this
  package never depends on them directly. `@speed/i18n` and
  `@speed/ui-kit` are regular dependencies.
- **The public API is frozen by convention.** Lockstep versioning makes
  an exported-signature change a breaking release; extend the surface
  only intentionally. A public change ships, in one commit: the code,
  its tests, this AGENTS.md, the README (contract prose and the
  resource table when keys change), and the compiled usage example when
  the documented composition changes.

## Testing

Unit tests are vitest + jsdom, one file per source file under `src/`,
shared helpers only in `test-utils/` (`renderWithProviders` mounts the
unit under the real host tree — `I18nextProvider` around `ui-kit`'s own
`AppThemeProvider`, both namespaces registered, fresh i18n instance per
call so the double-registration guard never fires across tests;
`expectNoAxeViolations` runs axe, always `await`ed; `mockMatchMedia`
stubs jsdom's missing `window.matchMedia` for `AppShell`'s
desktop/mobile split). Bilingual assertions import the shipped bundles
(`../locales/zh-CN.json`, `en-US.json`, and — where a test asserts
`RouteGuard`'s denied fallback — `ui-kit`'s own shipped bundles) —
never inline a language literal. `src/usage-example.test.tsx` compiles
and executes the README's Quick start composition, so the documented
usage cannot drift from the API; when that composition changes, this
file changes with it.

## Deferrals (recorded, do not re-open silently)

- **`auth-core` wiring**: no real authorization source exists yet (a
  later, undispatched round); the Quick start's `status` stub is
  deliberate and needs no change to this package once one lands.
- **`admin-shell`**: out of scope; this package ships only the shared
  chrome the platform-staff shell will later assemble. The tenant-facing
  half of that row is closed: `product-shell` is this package's first
  consumer, composing `AppShell` as its authenticated frame while
  leaving `RouteGuard` to hosts -- its gated-journey suite composes the
  gate inside the shell's children, fed a status the fixture host
  computes from the role lists it attached, the exact host-injected
  shape this package's non-negotiable rules require.
- **Reference-app consumer**: `examples/reference-app` has no frontend
  directory yet (Go-only today); the required consumer proof is
  satisfied at the package level (`src/usage-example.test.tsx`), the
  same honesty standard `ui-kit` used before any shell consumer
  existed.
- **Storybook**: no preview-harness round exists yet; components are
  covered by jsdom tests + axe, and color-contrast verification awaits
  a browser-side visual round, same as `ui-kit`.
- **The `no-literal-text` rule** catches the direct literal routes but
  not indirect ones (ternary branches, literals crossing component
  boundaries) — documented partial enforcement; hosts own their
  literals.
