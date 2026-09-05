# AGENTS.md — web workspace

Working conventions for the pnpm workspace under `web/`. The repository-wide
rules in the root `CLAUDE.md` and the root `README.md`'s status table apply;
this file adds what is specific to the npm side.

## Before you edit

- Every deliverable npm package ships **English** documentation, comments
  and commit messages. CJK appears only in product resources and test
  fixtures under directories named `locales/`, `locale/`, `i18n/` or
  `translations/` (the repo CI CJK scanner exempts those names).
- Never add a second Node- or pnpm-version source. Node: `web/.nvmrc`;
  pnpm: `web/package.json`'s `packageManager`. CI's setup-node-env action
  consumes both; if you change either, the action needs no edit by design.
- Root `package.json` carries only shared dev tooling. A package's real
  dependencies, peers and `exports` map live in its own manifest.

## Adding a package

1. Scaffold under `web/packages/<name>` following the shape in the root
   `web/README.md` (ESM-only exports map, tsconfig pair, scripts
   `lint`/`typecheck`/`test`/`build`).
2. Add it to the CI matrix: `.github/workflows/fast-check.yml`'s `npm-packages`
   job calls the reusable `npm-package-ci.yml` once per package path.
3. Write README.md + AGENTS.md and run every gate from the package
   directory: `pnpm lint`, `pnpm typecheck`, `pnpm test`, `pnpm build`, all
   zero warnings.
4. New runtime/peer dependencies need justification in the pull request:
   they land in every consuming project's bundle or `package.json` peer set.
5. Public API is frozen by convention like Go module APIs (lockstep
   versioning is repo-wide): a breaking change ships deliberately, with the
   round that owns it.

## Test placement and hygiene

- Unit tests are colocated per source file (`registry.ts` ->
  `registry.test.ts`). A test file that does not map 1:1 to a source file is
  named for the behavior it verifies.
- Shared helpers live in the package's `test-utils/` (e.g.
  `i18n/test-utils/welcome.ts`) and are imported by tests; never duplicate
  them inline.
- Environment-dependent globals (navigator, localStorage, location) are
  never relied on implicitly: inject deterministic stand-ins or guard the
  reads, or the suite breaks under Node and CI.
- Vitest runs from the package directory; pure packages need no vitest
  config (Node environment). A DOM-rendering package declares its own
  small config instead -- ui-kit's `vitest.config.ts` sets the jsdom
  environment and its setup file -- and keeps jsdom, Testing Library and
  friends in its own devDependencies, never the shared root.
- CI runs `pnpm install --frozen-lockfile` from `web/`, then
  lint/typecheck/test/build per package with the root-provided toolchain.

## The strict typecheck

`web/tsconfig.base.json` is strict: `strict`, `noUncheckedIndexedAccess`,
`verbatimModuleSyntax` (type-only imports must be `import type`),
`isolatedModules`, `resolveJsonModule`. Shape-drift guardrails in tests use
`// @ts-expect-error` lines, which the typecheck verifies. Do not loosen the
base to accommodate new code; adjust the code.

## The eslint surface

One flat config at `web/eslint.config.mjs` serves the whole workspace
(discovered by ascending from each package). It enforces
`typescript-eslint` recommended plus `no-explicit-any` as an error, and
carries the workspace's own rules behind the `speed/` plugin namespace:
`speed/no-literal-text` (implementation and rule tests in
`web/eslint-rules/`) errors on user-facing text written inline in
package `src`, and `speed/no-direct-http` errors on hand-written HTTP
(fetch/XMLHttpRequest/axios/node-fetch) anywhere but `@speed/api-client`,
the rule's single config-level whitelist -- every request must route
through the api-client request function (see CLAUDE.md's API contract
section). Package tests and `test-utils/` are exempt by config for
both rules, because fixture strings and scripted stand-ins are data.
The rules' unit tests run from the workspace root, not from a package
directory (the rules live outside every package, so no per-package
suite picks them up): locally via
`pnpm exec vitest run eslint-rules/no-literal-text.test.mjs eslint-rules/no-direct-http.test.mjs`
from `web/`, and in CI by fast-check's `repo-checks` job, which runs that
same command once per PR.

Still deferred, tracked in the CI workflow headers with their owning
rounds -- do not half-enable: the `react-hooks` plugin awaits a
stateful-components round.

## CJK scanner exemption

The repo's `tools/scan_cjk.py` full-text-scans everything outside
`docs/internal/`, except directory names in
{`locales`, `locale`, `i18n`, `translations`} and files in
{`.git`, `.idea`, `.vscode`, `node_modules`, `vendor`}. Concretely for this
workspace: bilingual test fixtures live under
`packages/<name>/test-utils/locales/<namespace>/<lang>.json` -- or,
when the fixtures ARE the shipped resources, under the package's
`src/locales/` (ui-kit's zh-CN/en-US bundles); `.ts` and `.md` files
assert or describe content by importing those fixtures, never by
embedding language text.
