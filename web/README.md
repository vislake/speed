# web/ — the frontend workspace

The repo's npm side. Every deliverable npm package of the platform lives
under `web/packages/*` as a **pnpm workspace** rooted here.

## Why web/ is its own workspace root

The repo root is a Go workspace (`go.work` + one module per directory under
`go/`) and must stay one. The frontend deliberately does **not** share the
root:

- `go work` and other Go tooling walk module roots; a root-level
  `package.json` or `pnpm-workspace.yaml` would sit in that walk and invite
  cross-tool confusion.
- pnpm's workspace protocol (`workspace:*`) and its single lockfile want a
  root of their own; the go.work file and pnpm's artifacts have nothing in
  common.
- Nothing Go-side ever needs to resolve an npm package, and nothing npm-side
  ever needs a Go module. Two roots, one repo, zero overlap.

CI treats the boundary the same way: Go checks run from module directories,
npm checks run from `web/` (see `.github/workflows/reusable/npm-package-ci.yml`).

## Node and pnpm versions: single sources, identical locally and in CI

| What | Single source of truth | Consumers |
|---|---|---|
| Node version | `web/.nvmrc` (content: `24`) | `setup-node`'s `node-version-file` input in `.github/actions/setup-node-env`; local `nvm`/`mise` if you use one |
| pnpm version | `"packageManager": "pnpm@11.1.2"` in `web/package.json` | CI parses it and installs exactly that version; local `corepack` can do the same |

The Node major is pinned to 24 because that is what CI runners provide;
`web/package.json`'s `engines` accepts `>=24` so local development on newer
Node (e.g. 26) keeps working. Never add a second version source; when either
version moves, update the source row above and the CI action stays in sync
by construction.

## Layout

```
web/
  package.json          private root; scripts and shared devDependencies only
  pnpm-workspace.yaml   packages: [packages/*]
  pnpm-lock.yaml        committed; installs run with --frozen-lockfile
  tsconfig.base.json    strict base every package extends
  eslint.config.mjs     one flat config for the whole workspace
  packages/
    tokens/             @speed/tokens  -- design tokens, zero dependencies
    i18n/               @speed/i18n    -- react-i18next wrapper + namespace registry
```

Root `package.json` holds only what every package shares (typescript,
vitest, eslint, typescript-eslint) plus the workspace scripts; a package's
own `package.json` holds its dependencies, peers and its `exports` map.
There is deliberately **no React, no MUI, no test framework** at the root —
each package declares what it actually uses, so `pnpm -r` never installs a
framework a package does not need.

## Commands (run from web/)

```
pnpm install --frozen-lockfile   # reproduce CI's install exactly
pnpm -r lint                     # eslint over every package (flat config)
pnpm -r typecheck                # tsc --noEmit over every package
pnpm -r test                     # vitest run over every package
pnpm -r build                    # emit dist/ (ESM .js + .d.ts) per package
```

Single-package runs work from the package directory (`pnpm test`, `pnpm
build`, ...) or with `pnpm --filter @speed/<name> <script>` from `web/`.

## Package shape (all packages follow it)

- `"type": "module"`; **ESM-only** publish: `exports` maps point `types` and
  `import` at `dist/` `.d.ts`/`.js` pairs. NodeNext consumers import
  normally; no CJS twin ships in M0.
- `"files": ["dist", "README.md", "AGENTS.md"]` — the dist/ tree plus the
  docs travel with the package.
- `tsconfig.json` (typecheck) extends `../../tsconfig.base.json` with
  `noEmit`; `tsconfig.build.json` emits declarations into `dist/` with
  `rootDir: src` so tests and `test-utils/` never leak into the artifact.
- The build runs under `module`/`moduleResolution: nodenext`, which emits
  relative imports verbatim: **sources write explicit `.js` extensions on
  every relative import** (TS2835 otherwise), so the shipped `dist/` is
  loadable by Node ESM and typecheckable by NodeNext consumers as-is.
- Sources: one domain per file, tests colocated (`x.ts` -> `x.test.ts`),
  shared test helpers under the package's `test-utils/`, never duplicated.
- Repository language rule: everything here is English; CJK-bearing
  fixtures live only under a `locales/`-named directory (the repo's CI CJK
  scanner exempts those) and `.ts` sources assert against imported fixtures
  rather than embedding language literals.

## What is not here yet

`ui-kit`, `api-client`, `api-sdk`, Storybook, Playwright, changesets and the
release tooling, and the web side of the app shell are all planned; see the
repo roadmap (`docs/internal/15-roadmap.md`) and the CI workflow headers
for what each round delivers.
