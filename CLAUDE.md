# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository Status

**Milestone M0 is in progress** (see `docs/internal/15-roadmap.md`). `go.work` lists the planned Go modules; most are still placeholder stubs (`go.mod` + a one-line `doc.go` + an `AGENTS.md` pointing at the relevant design doc — nothing to build against yet). The following modules have real, tested implementation: `go/pkgcore`, the dependency floor every other module sits on (the `Module`/`Registry`/`Kernel` wiring contract — one `Register(reg *Registry)` call per module, with the `Registry` built by the three-argument `NewRegistry(bus, kv, mailer)` — tenant-context primitives, `apperr`, the bootstrap `config` loader, and the dual-implementation infrastructure seams: an in-memory and a Redis-backed `KVStore` and `EventBus` (the distributed `EventBus` is a Redis Streams bus with consumer-group readers, both Redis halves living in pkgcore itself), the `Mail`/`Mailer` seam (`NewConsoleMailer` for the standalone mode, `NewSMTPMailer` for the distributed one — injected via `WithMailer`, whose absence fails a distributed bootstrap with `ErrMissingDistributedMailer`; `Registry.Mailer()` exposes the transport), and the `ObjectStore` seam (`NewLocalObjectStore` for the standalone mode's local directory — a throwaway temp directory by default, a persistent one when the host injects its own via `WithObjectStore` — and `NewS3ObjectStore` for the distributed one, injected via `WithObjectStore`, whose absence fails a distributed bootstrap with `ErrMissingDistributedObjectStore`; `Registry.ObjectStore()` exposes it); plus the merged message catalog: `pkgcore/i18n` aggregates the locale files every module ships (each message-shipping module's `Locales()` embed.FS, one `zh-CN.toml` and one `en-US.toml` in M0 — `go/pkgcore/locales` carrying pkgcore's own seed bundle) through `Builder.AddModule` while `Kernel.Bootstrap` walks the module graph, `Build` freezing the merge into an immutable `*Catalog` whose `Lookup`/`LookupPlural` render a message in the locale asked for, never silently falling back to another language; the catalog is installed on the `Registry` only after every module has registered (`Registry.Locales()` is nil inside `Register`), and a `Registry` built directly with `NewRegistry` carries neither catalog nor ObjectStore — both arrive through `Bootstrap`); `go/dbkit` (the dual-dialect `*gorm.DB` wrapper, the mandatory generic `Repository[T]`, `MigrationRegistry`, field-level encryption, and the blind-index machinery that keeps an encrypted field queryable by exact match — `BlindIndex` computing HMAC-SHA256 over normalized input, with `NewBlindIndexer` binding a validated 32-byte key and a canonical normalizer (`NormalizeEmail`, `NormalizePhoneE164`) to each index column); `go/tenancy` (the tenant-resolution `Middleware`, the audited `WithSystemContext` escape hatch, and the `tenancytest.AssertIsolated` / `AssertNotTenantScoped` suites every other module's repositories are required to run); and `go/observability` (OpenTelemetry initialization via `Init` for both deployment modes, the context-aware `FromContext` structured logger — wrapped by an on-by-default PII/secret redaction layer that masks sensitive attribute keys and secret-shaped values before any sink, with no per-call opt-out — and an HTTP `Middleware` recording request-count/duration metrics behind a cardinality-bounded route label — `tenant_id` never becomes a metric label, per this file's own Security rule); `go/jobs` (the `Queue`/`Task`/`Job`/`Handler` contract shared by both deployment modes, `StandaloneQueue` — the standalone mode's SQLite-backed in-process worker pool — and `AsynqQueue`, the distributed mode's Redis-backed implementation); and `go/ratelimit` (a `KVStore`-backed rate limiter shared by `authn`, `integration`, and other modules that need to limit how often something happens — one dimension per call, with multi-dimension composition left to the caller).

On the web side, the `web/` pnpm workspace (its own workspace root — the repository root is the `go.work` workspace, and the two roots deliberately never overlap; layout and rationale in `web/README.md`) currently ships four implemented, tested packages: `@speed/tokens`, the design-token tree as dependency-free pure data (the typed `defaultTokens` assembly and `deepMerge`, the copy-on-write override mechanism that rebuilds only touched branches, so hostile `__proto__` keys become inert own properties), whose MUI-parity rows (`breakpoints.values`, `zIndex.values`) are pinned by tests — an MUI major that changes those defaults fails here, in the token package, before any theme adapter ships silently-wrong chrome; `@speed/i18n`, the react-i18next wrapper that is the frontend counterpart of `pkgcore/i18n`'s catalog: `createI18n` negotiates the start language from URL parameter, stored choice, the profile-language extension slot (fed by the M1 user-profile step), navigator languages, then the `zh-CN` default, and `registerNamespace` validates — before any mutation — that every supported language is covered by bundles with identical leaf key sets. The missing-key discipline matches the Go catalog: `fallbackLng: false` plus `load: 'currentOnly'` make cross-language fallback impossible, so a key missing in the loaded language renders as the key itself and fires a visible handler — never another language's text. MUI localization is bridged through the isolated `@speed/i18n/mui-locale` subpath, keeping MUI out of the main entry's import graph; and `@speed/ui-kit`, the first DOM-rendering package: the `createAppTheme` theme factory maps the merged token tree (defaults ← project ← tenant, each a `deepMerge` diff layer) onto an MUI v9 theme — spacing from the token unit, the elevation shadows floored onto MUI's 25-slot shadow ramp, the typography role table from the token scale, the adapter decisions documented and test-pinned in the package — `AppThemeProvider` composes that theme with the MUI locale of the active language and the CssBaseline, and six controlled components (`PageHeader`, `EmptyState`, `ConfirmDialog`, `FormField`, `FormLayout`, `DataTable`) render only state given through props — sorting, selection, pagination and filtering report up through callbacks; a component never fetches or stores data. Every built-in string renders from the bilingual `ui-kit` namespace (one `zh-CN.json` plus one `en-US.json` under `src/locales/`, identical key sets, registered once by the host), and the workspace's own `speed/no-literal-text` ESLint rule (implementation and rule tests in `web/eslint-rules/`) refuses user-facing text written inline in package `src`; and `@speed/api-client`, the web side's single home of hand-written HTTP: `createClient` wires an injectable `fetch` (never an implicit environment global — captured at construction, and construction without one fails), a memory-only `AccessTokenStore` (no storage API exists in the package — the access token is a credential an XSS would walk away with from `localStorage`, and the refresh token's httpOnly-cookie mechanics are M1 authn work; the token is re-read before every attempt, so a retry after a refresh carries the fresh token), a per-request timeout through an internal `AbortController` (caller cancellation rejects the raw `AbortError`, never wrapped, never retried), a silent single-flight 401 refresh that retries the refused request exactly once — any method, outside the transient-retry budget — with the fresh token (`refreshAccessToken?: () => Promise<boolean>` is the seam M1 authn fills; failure reports `access token refresh failed` and surfaces the 401 as an `auth: true` `ApiError`), conservative transient retries (idempotent methods only — GET/HEAD/OPTIONS — on 429 honouring `Retry-After` plus 502/503/504/network/timeout, exponential full-jitter backoff under the frozen `DEFAULT_RETRY_POLICY` of 3 attempts / 200ms / 4s ceiling), and every failure normalized into one `ApiError` carrying the API envelope's `code`/`traceId`/`params`/`details` — or a reserved `client.*` code (`client.network`, `client.timeout`, `client.protocol`, `client.http.<status>`) when the API layer itself failed — with an `isApiError` type guard and a structured `Reporter` seam (constant message, snake_case attributes; the console sink is a stopgap until the M1 diagnostics round). No tenant header exists anywhere — tenant context travels inside the access token; no i18n resources ship — codes map to bilingual text in consuming packages' catalogs; `useFeature`/`usePublicConfig` hooks, uploads/SSE, and the generated `@speed/api-sdk` surface are deferred with their reasons in the package README. The workspace's `speed/no-direct-http` ESLint rule (implementation and rule tests in `web/eslint-rules/`) enforces the single-home claim: a direct `fetch` call, a `window.fetch`/`globalThis.fetch` call, a `new XMLHttpRequest`, or an `axios`/`node-fetch` import in any other package's `src` is an error, with this package as the rule's one config-level whitelist. `web/.nvmrc` (Node 24) and the `packageManager` field of `web/package.json` (pnpm 11.1.2) are the single version sources the shared CI action reads.

This matters for how you work here: **`go build github.com/vislake/speed/go/...`, `go vet`, `golangci-lint run ./...`, and `go test ./... -race` genuinely run and pass today for `go/pkgcore`, `go/dbkit`, `go/tenancy`, `go/observability`, `go/jobs`, and `go/ratelimit`** (run them from inside each module's own directory for the vet/lint/test forms — the repo root is a `go.work` workspace, not a module, so a bare `./...` from the root only works with the full import-path form). The web-side mirror holds for the four packages above: **`pnpm -r lint`, `pnpm -r typecheck`, `pnpm -r test`, and `pnpm -r build` genuinely run and pass today for `web/packages/tokens`, `web/packages/i18n`, `web/packages/ui-kit`, and `web/packages/api-client`** (run from inside `web/`, the pnpm workspace root). `go/dbkit` and `go/tenancy` also have a real integration tier (`go test -tags=integration ./...`) that starts real PostgreSQL via testcontainers, `go/pkgcore` and `go/jobs` have the same tier backed by a real Redis (pkgcore additionally exercising its MinIO/S3-backed `ObjectStore` implementation) — Docker must be running for any of these forms, though not for the plain unit-test form; `go/observability` has no such tier by design, not by gap — its OTLP path for the distributed deployment mode is proven in-process against a real generated gRPC collector server instead (see `go/observability/AGENTS.md`'s Testing section), needing no Docker. Do not assume this claim is stale without checking; conversely, do not assume any *other* module has real code behind it just because its directory exists — check for more than a `doc.go` stub before relying on one. `Taskfile.yml` exists and its underlying commands work, but the `task` CLI binary itself is not installed in this environment as of this writing — run the commands it wraps directly (`go test ./...`, `go vet ./...`, `golangci-lint run ./...`) rather than assuming `task test` works until you've confirmed `task` is on PATH. CI mirrors this status with the same honesty: `.github/workflows/pr-check.yml` runs on every pull request — the same six-module matrix as the paragraph above (per-module `golangci-lint`, `go vet`, unit tests under `-race`, a workspace-context build and a `GOWORK=off` standalone build, via the reusable `go-module-ci` workflow), a per-package npm matrix for the four web packages above (a `pnpm install --frozen-lockfile` from `web/`, then lint / strict typecheck / unit tests / build per package, via the reusable `npm-package-ci` workflow, with Node and pnpm from `setup-node-env`'s single version sources and the pnpm store cached on the lockfile hash) — plus a `repo-checks` job that runs `tools/scan_cjk.py` over the whole tree, the six architecture-discipline semgrep rules under `tools/semgrep_rules/` over `go/` `examples/` `tools/` (each rule's planted-violation fixtures excluded at the CLI level; rule catalog and honest limitations in `tools/README.md`), a workspace-wide `go build github.com/vislake/speed/go/...`, a `go work sync` drift gate, `tools/check_toolchain.py` (the `.mise.toml` tool-mirror drift gate), and the workspace eslint rules' own vitest unit tests (`web/eslint-rules/` — `no-literal-text` and `no-direct-http` — once per PR from the `web/` root: the rules live outside every package, so no per-package suite would exercise them). Three further pipelines are live on demand: `pr-full.yml` on PRs labeled `full-ci` (the same six-module matrix through the same reusable, the Docker-backed integration tiers of dbkit/tenancy/jobs/pkgcore, and the reference-app lint leg and unit suite), `docs-check.yml` on PRs touching docs or i18n resources, via path filtering (runs `tools/check_i18n_keys.py` and the docs-site structural check `tools/check_docs_site.py`), and `api-contract.yml` on PRs touching the API-contract toolchain side of the reference app — the notes module's spec fragment and generator config under `examples/reference-app/internal/notes/api/`, the Taskfile `api:gen` task, or api-contract.yml itself (regenerates the notes module's API surface with the same pinned oapi-codegen v2.8.0 the Taskfile task uses, fails via `git diff --exit-code` when the committed artifact is not what the spec generates, and `go build`s the reference app so a spec whose generated interface outgrew its handler cannot compile). `security.yml` is live from the same implementing round that ships the semgrep ruleset, the license scanner and the gitleaks config: it runs on every PR plus a daily 05:37 UTC schedule — pnpm audit against the web lockfile, a checksum-pinned gitleaks secret scan (`.gitleaks.toml` extends the default rules with exactly one allowlist, for observability's redaction-test fixtures), a CodeQL Go + TypeScript/JavaScript analysis (the Go extractor covers the go.work workspace natively), and the license scan; its header's TOOL POSTURE / DEFERRED sections record what it does not yet run and why — govulncheck cannot pass on the current tree (clearing its stdlib findings needs a go-directive raise off the 1.25.0 toolchain line and its module findings need test-support dependency bumps by the owning modules, both still open), and trivy has no images to scan since nothing ships a Dockerfile yet. The remaining three files in `.github/workflows/` are gated stubs as of this writing (e2e, nightly, scaffold-verify): their jobs fail at a stub-guard step until their implementing rounds land, and none of them triggers on pull requests. `release.yml` is live on demand too — the M0 lockstep-release pipeline, dispatched manually with a version number (the workflow_dispatch `version` input, release-version form required): it validates the version against `^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`, runs the release coordinator (`tools/release/lockstep-release.py`) in its default offline-verification mode — the full one-version plan derived at runtime: every go.work `go/` module with its `go/<module>/<version>` tag, every `web/packages/*` package with the version the changesets fixed group bumps it to (`web/.changeset/config.json`), exit 0 only when the plan is consistent — and then the coordinator's own `--self-test` suite. No publish credential is wired anywhere (`permissions: contents: read`), so nothing in `release.yml` can publish; real publishing is scheduled for the v1.0 release at M4. So a rule in this file that claims CI enforces it is only as real as the workflow behind it, which today means pr-check on every PR, pr-full on `full-ci`-labeled PRs, docs-check on doc/i18n-touching PRs, api-contract on spec-toolchain-touching PRs, security on every PR plus a daily schedule, and release on a manual dispatch.
## Language Rule (read this first)

- **`docs/internal/**` is written in Chinese.** It holds internal design discussion and decision rationale.
- **Everything else is English**: code comments, godoc/TSDoc, module docs, per-module `AGENTS.md` files, package READMEs, the root `README.md`, `.claude/skills/**`, and commit messages.
- User-facing product text is bilingual (zh-CN + en-US) and lives in i18n resources, never in code.

CI fails on CJK characters found outside `docs/internal/` (i18n resources and `docs/site/` localization directories excepted).

## Planned Commands

Defined in `docs/internal/19-dev-workflow.md`. Task runner is Taskfile; toolchain versions are pinned in the root `.mise.toml`, mirrored from each tool's authoritative source (`go.work`, `web/.nvmrc`, `web/package.json`, the Taskfile header, setup-go-env's `GOLANGCI_VERSION`) with a CI drift gate (`tools/check_toolchain.py`) proving the mirrors cannot drift.

```
task setup        # install toolchain, fetch deps, initialize the database
task dev          # run backend + frontend in standalone deployment mode with hot reload
task test         # test the affected modules
task test:full    # full matrix (dual deployment mode x dual dialect)
task lint         # lint everything
task api:gen      # merge specs, generate backend interfaces and frontend sdk
task docs:serve   # preview the docs site locally
task new:module   # scaffold a new module (prints the registration checklist: go.work use entry, CI matrix, roadmap rows)
task release:plan # verify the lockstep release plan for one version, offline (M0 round)
```

`task dev` must work in **standalone deployment mode** — single process, SQLite, zero external dependencies. Local development does not require `docker compose`.

## Architecture

### Shape

A **modular monolith distributed as libraries**. This is the single most important thing to internalize: speed is not an application, it is independently released Go modules and npm packages that business projects pull in via `go get` / `npm install`. They compile into one binary and call each other in-process — no service discovery, no Kubernetes-shaped infrastructure. Only a minimal starter skeleton is generated by CLI and freely editable by consumers.

Consequences that drive most design decisions:

- Every exported signature change propagates to every delivered project. Treat public API as frozen unless you are intentionally shipping a breaking change.
- Every dependency added here lands in someone else's `go.sum` or bundle. Adding one needs justification in the pull request.
- Implementation details belong under `internal/` so consumers cannot import them.

### Module dependency direction

Dependencies flow strictly bottom-up (full graph in `docs/internal/01-architecture.md`):

```
pkgcore -> dbkit / observability / ratelimit -> tenancy -> config / jobs -> storage / notification
        -> authn / rbac / org / metering -> billing / ai-gateway / sharing / integration
        -> compliance -> admin
```

### Module wiring

Every module implements `pkgcore.Module` and registers everything through a single `Register(reg *Registry)` call — routes, config schema, feature flags, permissions, job handlers, notification types, events, audit actions. The `Registry` struct exists so that adding a new cross-cutting mechanism does not change the `Module` interface, which under lockstep versioning would break every module at once.

### Dual deployment modes

Every infrastructure dependency is an interface in `pkgcore` with **two implementations**, selected by `SPEED_DEPLOYMENT_MODE=standalone|distributed` during kernel wiring:

- **standalone** — single process, SQLite, in-memory KV / event bus / queue, console mailer, mock payment, zero external dependencies, starts in seconds.
- **distributed** — PostgreSQL, Redis, S3-compatible storage, real providers, the LGTM observability stack.

A side benefit worth knowing: the standalone implementations double as test doubles, so most unit tests need no testcontainers.

### Multi-tenancy

Shared database with `tenant_id` isolation, guarded three ways: a GORM plugin that auto-injects the filter, a mandatory generic `dbkit.Repository[T]` base, and PostgreSQL RLS in distributed deployment mode. Tables fall into four data domains (tenant / identity / platform / link) — `users` is deliberately **not** tenant-scoped, since a person can belong to several tenants; `memberships` bridges them.

### API contract

**Spec-first, non-negotiable order**: edit `api/openapi.yaml` → `task api:gen` → compilation failures reveal every handler to fix → implement → update frontend → commit together. The generated Go server interface participates in compilation, so drift between spec and implementation cannot compile. The backend half of that loop is implemented today for the reference app's notes module: its handler implements the generated `api.ServerInterface` (compile-time assertion at the bottom of `internal/notes/handler.go`) and the interface is regenerated from the module's `api/openapi.yaml` fragment by the Taskfile `api:gen` task, with artifact consistency and handler compilation enforced by the `api-contract.yml` pipeline (see Repository Status for exactly what it runs); the frontend half (orval) awaits a later web/ workspace round: the generated `@speed/api-sdk` package is a round of its own, separate from the `@speed/api-client` runtime it calls into, which M0 ships (see the package README's deferred list).

### Versioning

**Lockstep**: all Go modules and npm packages share one version number and release together; only same-version combinations are supported. This removes the compatibility matrix entirely, at the cost of consumers upgrading everything at once (`saasctl upgrade` handles the rewrite).

## Architecture Discipline

Every rule below is enforced by code review, and by CI where the tooling for it exists (Repository Status above says what CI genuinely runs today) — **these are not style suggestions**. Code that violates any of them should not be merged. The reasoning behind each lives in `docs/internal/`; the detailed how-to lives in `.claude/skills/`.

### Dependencies and module boundaries

- **Do not let `rbac` depend on `authn`.** Authorization only knows `Subject{TenantID, UserID}`; the authenticating side assembles the Subject and calls authorization.
- **Do not import another business module's structs for database relations.** Use ID references plus domain events — `authn` publishes `UserCreated`, `org` subscribes to create the default workspace; `org` never imports `authn.User`.
- **Do not import concrete infrastructure implementations in business code.** Depend on the `pkgcore` interfaces (`KVStore`, `EventBus`, `ObjectStore`, `Mailer`), never on `go-redis`, an S3 SDK, and so on.
- **Do not expose a capability on an interface that only one implementation can satisfy.** Interfaces are designed against the weaker side, which is the standalone deployment mode.

### API contract

- **Do not hand-write backend API calls.** The frontend may only use the generated hooks from `@speed/api-sdk`; `fetch` / `axios` are permitted only inside `@speed/api-client`.
- **Do not change the implementation before the spec.** Spec first, always.
- **Do not edit any file in `@speed/api-sdk`.** It is generated and overwritten wholesale on the next release.

### Multi-tenant isolation

- **Do not hold a `*gorm.DB` and write queries yourself.** Business repositories for tenant-owned data must embed `dbkit.Repository[T]`. Identity and platform data (see the data-domain table in `docs/internal/04-data-and-tenancy.md`) can't use it — the generic constraint requires `TenantScoped`, which those domains must *not* implement — so they use `dbkit.Open()`'s plain `*gorm.DB` directly; see `go/dbkit/AGENTS.md`'s "Known limitations" for why that's safe rather than a loophole.
- **Do not use `db.Table` / `db.Model` / `db.Raw` to work around the Repository.** The three bypass entry points are checked in CI today by the semgrep rule `tools/semgrep_rules/raw-gorm-bypass.yml` (repo-checks, every PR), whose header names the allowlisted sites (dbkit's own internals, `go/jobs/store.go`'s platform-data queries) and the residual gap it deliberately leaves (a workaround through another `*gorm.DB` method, e.g. `Exec` with hand-written SQL) — code review owns the residue.
- **Do not hand-write `WHERE tenant_id = ?`.** Tenant filtering is injected by the GORM plugin and the Repository; writing it by hand means you are bypassing the guard.
- **Do not accept a caller-supplied `tenant_id` at the API layer.** The tenant comes from the access token claims, never from request parameters, headers or bodies.
- Every new repository **must** run `tenancytest.AssertIsolated` (tenant data) or `AssertNotTenantScoped` (identity and platform data).
- **Do not reach for raw SQL to escape tenant filtering.** The only legitimate cross-tenant path is `pkgcore.WithSystemContext` (the raw primitive, implemented today) — the `tenancy` module, once built, wraps it with audit publishing for business-module use. Either way, it is restricted to `admin`, `compliance`, `jobs` and `authn`, and audited on every use.
- **Do not create cross-module foreign keys.** Store IDs only — cross-module FKs make independently released migrations and cascading deletes unmanageable.

### Asynchronous work

- **Do not assume a worker has tenant context.** Rebuild `tenantctx` explicitly inside the job, or the Repository will fail closed.
- **Do not put business compensation in the queue layer.** The queue offers an `OnFailure` hook; refunding credits and similar compensation belongs to the business module.
- Long-running operations **must** go through the `jobs` queue and report progress; never run them synchronously inside an HTTP request.

### Deployment modes

- **Do not branch on `if mode == "standalone"` in business logic.** Deployment-mode differences belong exclusively to kernel wiring.
- Any new infrastructure dependency **must** ship both a standalone implementation (zero external dependencies) and a distributed one.

### Logging

- **Use structured logging only.** Take the logger from the context (`obs.FromContext(ctx)`) so trace and tenant correlation survives.
- **Do not build log messages by concatenation or `fmt.Sprintf`.** The message is a constant string; everything variable goes into key-value attributes.
- **Do not use `fmt.Println`, `log.Printf`, or `console.log`.** Frontend diagnostics go through the reporter in `@speed/api-client`.
- Attribute keys are `snake_case` and shared across the stack: `tenant_id`, `user_id`, `job_id`, `trace_id`, `duration_ms`.

### Internationalization

- **Do not hardcode user-facing text**, in any language. UI packages must contain no bare text nodes; Go returns structured error codes.
- New text **must** ship with both `zh-CN` and `en-US` resources. The backend half of that rule is enforced in code rather than CI: `pkgcore/i18n`'s `Builder.AddModule` fails a module whose language files' id sets differ (`ErrParityMismatch`) while `Kernel.Bootstrap` merges its catalog. `tools/check_i18n_keys.py` checks the same key-set parity over the raw files; the docs-check pipeline runs it on every PR touching documentation or i18n resources (Repository Status).
- Backend-generated content (emails, invoices, notifications) renders in the **recipient's** locale, not the operator's UI language.

### Database and migrations

- **Do not use `AutoMigrate`.** Migrations are versioned SQL generated by Atlas from the GORM models, one set per dialect.
- **Do not use PostgreSQL-only features**: `gen_random_uuid()`, native arrays, JSONB operator filtering, `NOW()`. Generate IDs in the application and use `datatypes.JSON`.

### Security

- **Do not merge social login accounts on matching email alone.** Auto-link only when the provider reports a verified email and is on the trusted list; otherwise require sign-in first, then binding.
- **Do not use `tenant_id` as a Prometheus metric label.** High cardinality will take Prometheus down; tenant dimensions belong in span attributes and log fields.
- **Do not let outbound webhooks reach internal addresses.** SSRF protection is mandatory, including DNS-rebinding protection.
- **Do not write plaintext PII, secrets or tokens into logs, traces or API responses.** Redaction is on by default.
- **Do not send messages to unverified phone numbers or email addresses.** External contacts must complete consent verification first; the verification message itself is the only exception and is rate limited.
- **Do not forward internal domain events straight to outbound webhooks.** Map them to a versioned public event schema.
- Audit records produced during impersonation **must** carry both the impersonated user (`Actor`) and the real administrator (`OnBehalfOf`).

### Testing

- **Unit test files are named after the file they test** (`registry.go` → `registry_test.go`; `PlanCard.tsx` → `PlanCard.test.tsx`). A test file that doesn't map 1:1 to one source file is named for the behaviour it verifies, never a generic word like `misc` or `extra`.
- **Do not scatter shared test helpers across test files.** Put them in a dedicated `internal/testutil` package (Go) or `test-utils/` directory (frontend) — never duplicated, never inline in a file another package's tests need to import.
- **Do not mix integration tests into the unit test set.** They live in a physically separate directory (`integration_test/` per Go package, `e2e/` for Playwright), so a plain `go test ./...` or unit test run never touches them.
- Full detail and examples: `.claude/skills/backend-coding-standards/SKILL.md` §13, `.claude/skills/frontend-coding-standards/SKILL.md` §12.

### Documentation

- A new public API **must** ship, in the same pull request, with usage docs, a compilable example, and an entry in the module's `AGENTS.md`.
- Code examples in documentation are compiled and run by CI; a failing example fails the build.
- The configuration reference is generated from the config schema — **do not hand-write it**.

### Commits and merging

- **Rebase onto the target branch before merging; fast-forward merges only.** History stays linear.
- Commit messages are English, in Conventional Commits form, scoped by module: `fix(billing): prevent credit over-deduction under concurrent debits`.
- **Every bug fix ships with a test that reproduces the bug** (failing before the fix, passing after). If one genuinely cannot be added, explain why and what the follow-up is.
- **Do not ignore warnings.** Compiler, lint, deprecation, console, a11y and race-detector warnings are all first-class issues; anything deferred must be called out explicitly.

## Traps Specific to This Codebase

Each of these has bitten real SaaS products:

- **Workers do not inherit tenant context.** Rebuild it explicitly (`pkgcore.WithTenant(ctx, job.TenantID)`) or the Repository fails closed.
- **Encrypted fields cannot be queried.** Phone numbers are encrypted at rest yet used as a login identifier; the blind-index column such a lookup needs is a `go/dbkit` facility implemented today (`NewBlindIndexer`: HMAC-SHA256 over the canonical form, with `NormalizePhoneE164` / `NormalizeEmail` supplying the E.164 / lowercased-email forms), so a module storing such an identifier must use that facility rather than reimplementing it.
- **Billing-grade metering cannot fail open.** It uses the outbox pattern in the same transaction as the business write; only analytics-grade metering may drop events.
- **Notifications are event-driven.** Business modules publish domain events; `notification` subscribes. The sole exception is synchronous verification codes. External recipients who are not users require consent verification before anything is sent.

## Where the Rest Lives

| Location | Content |
|---|---|
| `.claude/skills/**` | Coding standards handbooks — how to write the code, with templates and checklists. Load the relevant skill before writing: `backend-coding-standards`, `frontend-coding-standards`, `commit-convention`. |
| `docs/internal/**` | Design decisions and *why* they were made, including rejected alternatives. Start at `00-overview.md`, which carries the full navigation table. |
| per-module `AGENTS.md` | Module-level discipline that ships with the module to consuming projects, whatever AI tooling they use. |

## Reference App

`examples/reference-app` is an AI smile simulation platform (dental SaaS) and is the **mandatory first consumer** of every module — a module API that it does not actually use is not considered done. It was chosen because it exercises every edge of a general SaaS: multi-level organizations, pay-per-use credits, long-running AI jobs, media handling, external sharing, sensitive-data compliance, third-party integration. Validating the design against it already exposed six missing capabilities.

An end-to-end suite with recorded/replayed AI responses — real provider calls happen only during milestone acceptance and demos — is planned to run in CI with the e2e pipeline (`.github/workflows/e2e.yml`, a gated stub deferring to the roadmap M4 e2e item); no live workflow runs an end-to-end reference-app suite today (its unit suite runs on `full-ci`-labeled PRs via pr-full).
