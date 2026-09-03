# AGENTS.md — @speed/api-sdk

Guidance for AI tooling working in or against this package. The public
surface, generation mechanics and deferred items are documented in the
package `README.md`; this file records the invariants that keep the
package safe, mirroring the repo rules in `CLAUDE.md` and
`docs/internal/21-api-contract.md`.

## What this module is

The generated, spec-first typed surface of speed APIs for the
frontend: orval output from each module's openapi fragment, calling the
`@speed/api-client` runtime through one hand-written seam
(`src/runtime.ts`). Generated code never performs HTTP itself; the
`speed/no-direct-http` ESLint rule whitelists only `@speed/api-client`,
and `src/` must keep passing it on every regeneration.

## Invariants (code review enforces; do not weaken)

- **`src/index.ts` is generated; never hand-edit it.** It is stamped
  with a DO-NOT-EDIT header from `web/orval.config.ts` that carries the
  pinned orval version -- tool drift manifests as a header diff. Change
  the generator config and regenerate; never patch the artifact.
- **`src/runtime.ts` is the only hand-written source file.** Never add
  a second hand-written file inside `src/` to bridge a generation gap.
  Tooling gaps are fixed in tooling: the extensionless-mutator-import
  problem is solved by `web/scripts/orval-nodenext-fixup.mjs`, a
  deterministic post-generation rewrite that exits non-zero when orval
  changes its emission, and by the tsconfig pair (`bundler` resolution
  for typecheck, `nodenext` for build), not by shipped source.
- **The mutator seam is a binding, not an import.** `speedRequest`
  adapts orval's axios-shaped call to the request function a host bound
  with `bindRequestFn(createClient(...))`. Calls made while unbound
  throw a programmer error (`[speed-api-sdk] no request function
  bound: ...`); rebinding replaces the previous function, last bind
  wins, no once-guard (tests and hot reload rebind).
- **No tenant concept exists in generated code.** No tenant header,
  no tenant id in query keys, no `tenant_id` in request or response
  types (the notes fragment documents its absence). Tenant query-key
  namespacing is an M1 consumer-shell discipline, recorded in
  `web/orval.config.ts` and the README.
- **orval stays out of the lockfile.** Every runner -- the Taskfile
  `api:gen` frontend leg and the `api-contract.yml` regen step --
  invokes `pnpm dlx orval@8.17.0 --config orval.config.ts` from `web/`
  followed by the fixup script. A version bump lands in all three
  places (config comment, Taskfile, workflow) at once.
- **No user-facing text, no i18n resources.** Errors are typed as the
  spec's `{code, params}` envelope; codes map to bilingual text in
  consumer catalogs. The runtime seam's programmer errors are constant
  English strings.
- **The peer family is the QueryClient contract.** `react` and
  `@tanstack/react-query` v5 are peers so hosts share one
  QueryClient/Provider (`docs/internal/21-api-contract.md`); the
  package never constructs one. Its own hooks are exercised in tests
  with a `QueryClientProvider` around `renderHook`.

## In this round vs. deferred

Landed: the generated notes surface (`src/index.ts`), the runtime seam,
the regeneration tooling (config + fixup script) and the CI wiring that
regenerates and diffs the artifact.

Deferred with reasons:

- Redocly merge of module fragments into a single spec -- waits for the
  second fragment, M1 (`docs/internal/21-api-contract.md`).
- The oasdiff breaking-change gate -- waits for the first release
  baseline, M4 (recorded mechanism decision, not a fake gate).
- Real UI consumers (reference-app shells) and tenant query-key
  namespacing -- M1. Until then the package is test-consumed-only;
  its unit tests bind a fake request function and never touch a
  network.
- Release-time packaging of the merged spec into the published SDK --
  M4 machinery with release-foundation (`docs/internal/18-cicd.md`);
  do not claim it lives here.

## Public surface

Two entry points:

- `.` (generated `src/index.ts`) -- the operation functions, hooks and
  response models orval derives from the module fragments. The shape
  is orval's, not ours: expect it to change only through the generator.
- `./runtime` (`src/runtime.ts`) -- `bindRequestFn` and `speedRequest`
  (the mutator), the stable hand-written seam M1 shells import.

`src/runtime.test.ts` pins the seam's behaviour; `src/index.test.ts`
pins the generated surface's observable behaviour (hooks issue the
expected calls through the mutator with the expected query keys).
Extend `runtime.ts` deliberately; the generated file only via
`web/orval.config.ts`.

## Development

From this directory (`web/packages/api-sdk/`), or workspace-wide from
`web/` with `pnpm -r`:

```sh
pnpm lint        # eslint; generated src/ must pass, incl. speed/no-direct-http
pnpm typecheck   # strict; bundler resolution maps sibling sources via tsconfig paths
pnpm test        # vitest (jsdom); colocated unit tests, no network
pnpm build       # nodenext tsc build to dist/ (gitignored); pre-builds @speed/api-client
```

Regenerate (from `web/`):

```sh
pnpm dlx orval@8.17.0 --config orval.config.ts
node scripts/orval-nodenext-fixup.mjs
```

The committed artifact is the post-fixup file; CI reproduces both steps
and diffs.
