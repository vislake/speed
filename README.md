# speed

A reusable SaaS foundation for Go and React.

New SaaS projects pull in the capabilities they need via `go get` / `npm install`, generate a minimal starter skeleton with a CLI, and start with multi-tenant isolation, authentication and permissions, organizations and members, subscription billing and metering, an AI gateway, background jobs, media storage, notifications, observability and an operations console already in place.

## Status

**Milestone M0 is complete.** The design is complete and the M0 deliverables are landed on main with tests passing, following [the roadmap](docs/internal/15-roadmap.md); implementation continues module by module from M1. The following Go modules are implemented and tested (`go build`, `go vet`, `golangci-lint run ./...` and `go test -race` all pass): `go/pkgcore` — the dependency floor every other module builds on: the `Module`/`Registry`/`Kernel` wiring contract plus the dual implementations of its infrastructure seams — in-memory and Redis-backed `KVStore` and `EventBus`, console and SMTP mailers behind one `Mailer` contract, and a local-directory and S3-backed object store behind one `ObjectStore` contract — and the merged message catalog (`pkgcore/i18n`), which aggregates every module's locale files during `Kernel.Bootstrap` into an immutable `*Catalog` that renders a message in the locale asked for; `go/dbkit` — the dual-dialect data-access layer with the mandatory generic `Repository[T]`, field-level encryption, and blind-index columns that keep encrypted fields queryable by exact match; `go/tenancy` — tenant resolution, the audited system-context escape hatch, and the isolation-assertion test suite; `go/observability` — OpenTelemetry initialization for both deployment modes, a context-aware structured logger with PII/secret redaction on by default, and HTTP request metrics with a cardinality-bounded route label; `go/config` — dynamic configuration with a schema every module declares into at register time and the module freezes at attach, a `configs` table with system-to-tenant scope fallback, encrypted-at-rest storage for sensitive values whose change events carry redaction markers instead of plaintext, and the pre-auth `/api/config/public` and `/api/system/features` endpoints serving host-resolved public values and dependency-checked feature flags; `go/jobs` — the asynchronous job queue, with a standalone SQLite-backed worker pool and a distributed Redis-backed (Asynq) implementation behind the same `Queue`/`Task`/`Handler` contract; and `go/ratelimit` — a `KVStore`-backed rate limiter shared by modules that need to limit how often something happens, one dimension per call with multi-dimension composition left to the caller. Pull-request CI (`.github/workflows/pr-check.yml`) runs real checks for exactly those seven modules — golangci-lint, vet, race-tested unit tests, and workspace-context plus `GOWORK=off` builds via a reusable per-module workflow — and for each implemented npm package below, via the reusable per-package workflow (a frozen-lockfile pnpm install from the `web/` workspace root, then per-package lint, strict typecheck, vitest unit suite and dist/ build) — alongside repository-level checks (the CJK scan, the all-modules build, the `go work sync` drift gate and the workspace ESLint rules' own unit tests — `no-literal-text` and `no-direct-http`, once per PR from the `web/` root). Five further pipelines are live: `pr-full` for PRs labeled `full-ci` (the Docker-backed integration tiers and the reference-app lint leg and unit suite), `docs-check` for PRs touching docs or i18n resources (the i18n key-parity check), `api-contract` for PRs touching the reference app's notes API fragment or the generator toolchain (regenerates the notes module's API surface on both halves with the pinned generators — oapi-codegen for the backend and orval into `@speed/api-sdk` for the frontend — failing when either committed artifact drifts from the spec, and compiles the app), and `release` for a manual dispatch with a version number (validates the version form, then runs the lockstep release coordinator `tools/release/lockstep-release.py` in offline-verification mode and its `--self-test` — proving offline the roadmap's M0 exit condition that one command can release every Go module and npm package under one version; no publish credential is wired, real publishing is scheduled for the v1.0 release at M4), and `security` on every pull request plus a daily 05:37 UTC schedule (a pnpm audit of the web lockfile, a checksum-pinned gitleaks secret scan, CodeQL analysis for Go and TypeScript/JavaScript, and the dependency-license scan against the committed manifest). The remaining three workflow files (e2e, nightly, scaffold-verify) are gated stubs as of this writing. The remaining planned Go modules are still placeholder stubs; on the npm side, `@speed/tokens` (design tokens as dependency-free data), `@speed/i18n` (the react-i18next wrapper with the no-silent-fallback missing-key discipline mirroring `pkgcore/i18n`'s catalog), `@speed/ui-kit` (the theme factory mapping merged tokens onto an MUI v9 theme, plus six controlled, bilingual components) and `@speed/api-client` (the single home of hand-written HTTP on the web side — `createClient` wiring an injectable fetch, a memory-only access-token store with a silent 401-refresh seam, conservative retry and one normalized `ApiError` type, and the only package whose `src` the `speed/no-direct-http` ESLint rule whitelists) and `@speed/api-sdk` (the orval-generated typed surface of the notes spec fragment — generated code never touches HTTP, routing every call through one hand-written seam into the `api-client` runtime, every file stamped with a DO-NOT-EDIT header carrying the pinned orval version) are implemented and tested under the `web/` pnpm workspace.

- 📐 [Design overview](docs/internal/00-overview.md) ← **start here**
- 🗺️ [Roadmap and milestones](docs/internal/15-roadmap.md)
- ⚠️ [Risk register](docs/internal/17-risks.md)

> Design documents under `docs/internal/` are written in Chinese for internal design discussion. Everything else in this repository — code comments, module docs, public documentation — is English.

## Core Design Choices

| Choice | What it means |
|---|---|
| **Import modules, don't fork** | Capabilities ship as independently released Go modules and npm packages; only the minimal starter skeleton is generated by a CLI and freely editable |
| **Modular monolith** | Modules are released independently but compiled into a single binary and called in-process — no service mesh, no Kubernetes-shaped infrastructure |
| **Two deployment modes** | The standalone deployment mode runs as a single process with zero external dependencies and starts in seconds; the distributed deployment mode adds PostgreSQL, Redis and the observability stack |
| **Lockstep versioning** | Every module shares one version number; only same-version combinations are supported, so there is no compatibility matrix |
| **Contract first** | Every REST API has OpenAPI as its single source of truth, and all frontend call code is generated — front/back drift is caught by the compiler |
| **Bilingual by default** | Chinese and English from day one, never retrofitted |
| **Validated against real requirements** | A built-in `reference-app` is the mandatory first consumer of every module |

## Planned Layout

```
speed/
  go/          Go modules (pkgcore / tenancy / authn / rbac / billing / jobs / ...)
  web/         npm packages (ui-kit / auth-core / api-sdk / layout-kit / ...)
  templates/   minimal starter skeletons produced by the CLIs
  examples/    reference-app: the AI smile simulation platform
  deploy/      layered docker-compose files (standalone / full / observability / dev tools)
  docs/
    internal/  design documents (Chinese)
    site/      public documentation for consuming teams
```

The full layout and release strategy live in [02 Repository and Release](docs/internal/02-repo-and-release.md).

## Coding Standards

| Standard | Applies to |
|---|---|
| [Backend coding standards](.claude/skills/backend-coding-standards/SKILL.md) | all Go code under `go/**` |
| [Frontend coding standards](.claude/skills/frontend-coding-standards/SKILL.md) | all React/TypeScript code under `web/**` |
| [Commit convention](.claude/skills/commit-convention/SKILL.md) | every git commit |

In Claude Code these three load automatically as skills; otherwise read the files directly.

## For AI Coding Assistants

Read [CLAUDE.md](CLAUDE.md) first — it carries the architecture overview, the full discipline list, and pointers to everything else. Then follow the three standards above when writing code.
