# @speed/test-utils

The workspace's shared frontend test-support package: the single home of
the DOM-test scaffolding the `@speed/*` packages and the reference-app
web host would otherwise each carry a copy of.

It is **private and never published** — a workspace member consumed by
the packages' and the app's tests, not a deliverable library. It lives
beside `packages/` rather than inside it because `web/packages/*` is the
release pipeline's deliverable set (`tools/release/lockstep-release.py`
derives the npm publish order from exactly that directory, without a
private-package filter and under one uniform version), and a private
member inside that set would enter the release plan and the publish
loop. The reference-app web host — the workspace's other private member
— lives outside `web/packages/*` for the same reason.

## Resolution

Consumers reach the package through the workspace's single package map
(`web/scripts/speed-aliases.mjs`): the specifiers `@speed/test-utils/<module>`
alias onto this package's live `src/<module>.ts(x)` in every vitest/vite
config (the map's `speedAliases()` derivation), and each consuming
package's tsconfig `paths` section pins the same targets for `tsc` (the
alias-manifest suite in the reference app proves both). Each consuming
package declares the dependency in its `devDependencies`. There is no
`dist`: the package is consumed as source, the same way the map resolves
every workspace sibling, so `build` is the standalone compile check.

This package's own `@speed` imports (the session harness drives
`@speed/auth-core` sessions over `@speed/api-client` and the
`@speed/api-sdk` runtime seam; the render layer builds on `@speed/i18n`)
resolve through the same alias map and resolve in `tsc` through this
package's `paths`, and are deliberately **not** declared in this
manifest: a declared edge onto a package that declares this one back --
auth-core, whose tests drive the harness -- would make the workspace
dependency graph cyclic, which pnpm reports as a standing warning on
every install. Only the third-party libraries (react, axe-core,
testing-library, react-query) are declared here, because those resolve
through `node_modules` rather than the map.

## Modules

- `axe.ts` — the axe-core assertion harness: `expectNoAxeViolations`
  (the widget tier, `region`/`landmark-one-main` disabled),
  `expectNoAxeViolationsForPage` (the page tier for packages whose unit
  under test is page-level chrome), and `runHeadingOrderCheck` (the
  single-rule probe for level-dependent headings). Each package's
  `test-utils/axe.ts` re-exports the tier its scans need.
- `render.tsx` — the render-primitive layer: `TEST_LANGUAGES`, the
  deterministic bilingual `createTestI18n` base, the
  retries-nothing `createTestQueryClient`, and the shared
  `RenderWithProvidersOptions` / `RenderWithProvidersResult` shapes.
  Each package's `test-utils/render.tsx` keeps its own provider tree
  (which providers a package's units need is package policy) and builds
  it on these primitives.
- `setup.ts` — the vitest setup side effect every suite shares: jest-dom
  matchers plus the explicit RTL cleanup (vitest exposes no global
  `afterEach`, and RTL's automatic cleanup fires only off one).
- `matchMedia.ts` — the MediaQueryList stub for the AppShell frame's
  `useMediaQuery` call, with a handle to flip the reported answer and
  fire the stub's own change listeners for a mounted tree.
- `emitted-css.ts` — the emotion stylesheet-text probe for
  breakpoint-keyed `sx` rules jsdom cannot evaluate.
- `session-harness.ts` — the scripted-request-function harness for
  `@speed/auth-core` sessions: a fake `RequestFn` bound through the
  api-sdk runtime seam drives a real session over a fresh memory store.
- `real-client.ts` — the real-client network rig: a genuine
  `@speed/api-client` client over a fetch stand-in answering from a
  script, optionally over the session's own refresh seam; each
  consuming package projects the observed request into its own recorded
  call shape. Also the shared `jsonResponse` / `errorResponse` /
  `makePair` / `signInWithPassword` answer helpers.

## Testing

`src/session-harness.test.ts` is the harness's own regression (the
script dispatch gate's prototype-chain branch). Run it with
`pnpm test` from this directory.
