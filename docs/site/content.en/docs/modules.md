---
title: Modules
weight: 3
---

# Module index

speed ships as independently released Go modules and npm packages, not
an application. A consumer project imports the ones it needs. Every row
below links to that module's own `AGENTS.md` or `README.md` in the
repository — this site keeps no copy of module documentation, so the
linked file is always the current one.

> [!NOTE]
> This page lists what exists, not what is proven equally solid.
> Read-for-yourself is still the rule: the root `CLAUDE.md`'s *Repository
> Status* section (see [For AI Agents](../ai-agents/)) is the single
> source of truth for which modules genuinely pass `go build` / `go vet`
> / `golangci-lint` / `go test -race` in CI today — all 21, `go/admin`
> included, per `fast-check.yml`'s own `go-modules` matrix.

## Go modules (`go/`, 21)

One row per `go.work` `use` entry. Dependencies flow bottom-up — see
[For AI Agents](../ai-agents/) for the coarse ordering.

| Module | What it is | Docs |
|---|---|---|
| `pkgcore` | The dependency floor: the `Module`/`Registry`/`Kernel` wiring contract, tenant-context primitives, `apperr`, the seam-registry mechanism (`EventBus`/`KVStore`/`Mailer`/`ObjectStore`), deployment-mode presets, and the merged i18n catalog. | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/pkgcore/AGENTS.md) |
| `dbkit` | Dual-dialect (SQLite/PostgreSQL) `*gorm.DB` wrapper: the mandatory generic `Repository[T]`, versioned migrations, field-level encryption, blind-index exact-match lookup, soft-delete/hard-delete, and the audit-capture write plugin. | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/dbkit/AGENTS.md) |
| `tenancy` | Tenant-resolution middleware, the audited `WithSystemContext` escape hatch, and the `AssertIsolated` / `AssertNotTenantScoped` test suites every other module's repositories run. | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/tenancy/AGENTS.md) |
| `observability` | OpenTelemetry initialization for both deployment modes, a context-aware structured logger with on-by-default PII/secret redaction, and an HTTP middleware for cardinality-bounded request metrics. | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/observability/AGENTS.md) |
| `config` | Dynamic configuration: every module declares its schema at register time; values are scoped system→tenant, cached, and Sensitive items sit encrypted at rest. Ships the two pre-auth endpoints a login page needs. | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/config/AGENTS.md) |
| `jobs` | The `Queue`/`Task`/`Job`/`Handler` contract shared by both deployment modes: `StandaloneQueue` (SQLite, in-process) and a Redis-backed queue for the distributed mode, split into its own subpackage so a standalone-only consumer never pays for it. | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/jobs/AGENTS.md) |
| `ratelimit` | A `KVStore`-backed rate limiter shared by `authn`, `integration` and other modules that need to limit how often something happens. | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/ratelimit/AGENTS.md) |
| `pki` | Signing-key and X.509 certificate lifecycle: the `Signer` seam, internal CA issuance, revocation, CRL generation and JWKS export. `authn`'s `KeySource` is its real, frozen-API consumer; the X.509/CA layer has no real consumer yet, a documented exception. | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/pki/AGENTS.md) |
| `authn` | Authentication — who a caller is, never what they may do. Passwords (argon2id), Ed25519-signed access tokens, single-use rotating refresh tokens, TOTP MFA, and social/enterprise SSO sign-in (Google, GitHub, WeChat, DingTalk, Feishu, OIDC). | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/authn/AGENTS.md) |
| `rbac` | Role-based access control, deny-by-default with exact `resource:action` matching. Deliberately never imports `authn` — it only ever sees a `Subject{TenantID, UserID}` the authenticating side assembles. | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/rbac/AGENTS.md) |
| `org` | A tenant's organization tree (adjacency list plus materialized path), the memberships bound to it, and invitations that create them. | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/org/AGENTS.md) |
| `storage` | Tenant object storage: upload/complete/derive lifecycle over the `ObjectStore` seam, structural metadata stripping, thumbnail derivation, and a crash-convergent delete + expiry sweep. | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/storage/AGENTS.md) |
| `notification` | The platform's messaging surface: a live notification-type registry business modules declare into, per-user preferences, an in-app inbox with SSE push, and consent-verified external delivery (email/SMS). | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/notification/AGENTS.md) |
| `billing` | Plan/Feature/Grant/Entitlements, a channel-agnostic Subscription/Invoice lifecycle, a concurrency-safe credits ledger, and real payment-gateway integrations (Stripe, Alipay, WeChat Pay). | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/billing/AGENTS.md) |
| `metering` | Usage recording with two reliability tiers — fail-open analytics-grade, and outbox-backed billing-grade that cannot silently drop an event — converging on real-time aggregate counters. | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/metering/AGENTS.md) |
| `sharing` | Public share links: 256-bit tokens, a forced default expiry, revocation effective on the very next access check, full access logging, and outward-identical refusal answers regardless of reason. | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/sharing/AGENTS.md) |
| `integration` | A tenant's outward-facing API surface: API-key issuance and layered rate limiting, plus outbound webhooks with HMAC signing and two-time SSRF/DNS-rebinding protection. | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/integration/AGENTS.md) |
| `ai-gateway` | A vendor-agnostic LLM chat and image-generation gateway (`ChatProvider`/`ImageProvider` seams, a zero-dependency OpenAI-compatible default, scope-tiered encrypted BYOK credentials). | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/ai-gateway/AGENTS.md) |
| `compliance` | Per-tenant retention sweeps, immediate right-to-erasure with a proven cross-tenant non-erasure property, export delivery through `sharing`, and read-only audit querying. | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/compliance/AGENTS.md) |
| `admin` | The operations-console backend: a tenant ledger, a full impersonation pipeline with dual-identity audit trail, cross-tenant user search, and a read-only audit-query shell. Round 2 — role management, a usage dashboard, per-tenant enforcement, an audit-export leg — has not landed yet. | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/admin/AGENTS.md) |
| `saasctl` | The consumer-facing CLI — see [Quickstart](../quickstart/) for its four commands. | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/saasctl/AGENTS.md) |

## Web packages (`web/packages/`, 11) + the consumer shell

An `@speed/`-scoped npm package per row, all riding one pnpm workspace
rooted at `web/`.

| Package | What it is | Docs |
|---|---|---|
| `@speed/tokens` | The design-token tree as dependency-free pure data — spacing, breakpoints, typography scale, z-index — with MUI-parity rows pinned by tests. | [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/tokens/AGENTS.md) |
| `@speed/i18n` | The react-i18next wrapper mirroring `pkgcore/i18n`: language negotiation, and namespace registration that refuses a key-set mismatch between `zh-CN` and `en-US` bundles. | [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/i18n/AGENTS.md) |
| `@speed/ui-kit` | The MUI v9 theme factory (`createAppTheme`) over the token tree, plus seven controlled components (`PageHeader`, `EmptyState`, `ConfirmDialog`, `FormField`, `FormLayout`, `DataTable`, `FileUploader`). | [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/ui-kit/AGENTS.md) |
| `@speed/api-client` | The web side's single home of hand-written HTTP: injectable fetch, memory-only access-token store, single-flight 401 refresh, conservative transient retries, and one normalized `ApiError` shape. | [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/api-client/AGENTS.md) |
| `@speed/api-sdk` | The generated typed surface of the merged OpenAPI document (orval output) — never hand-edited, routes every call through `api-client`'s single binding seam. | [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/api-sdk/AGENTS.md) |
| `@speed/layout-kit` | Shared app chrome: `AppShell` (app bar, responsive nav drawer) and `RouteGuard`, gated on a host-injected status only — no auth or routing package as a dependency. | [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/layout-kit/AGENTS.md) |
| `@speed/auth-core` | The headless session layer: `createAuthSession` turns the generated authn operations into one observable memory-only state machine (login, refresh, tenant switch, step-up). | [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/auth-core/AGENTS.md) |
| `@speed/auth-ui` | The sign-in component family over an auth-core session: password/SMS/social channels, registration, and the sign-out/session-ended views. Every component is controlled. | [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/auth-ui/AGENTS.md) |
| `@speed/tenancy-ui` | One controlled `TenantSwitcher` component — session, tenant list and current tenant are all host-supplied props. | [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/tenancy-ui/AGENTS.md) |
| `@speed/product-shell` | The tenant-facing assembly shell: `ProductShell` composes `AppShell`, the auth-ui sign-in family and auth-core hooks into one three-branch view machine. | [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/product-shell/AGENTS.md) |
| `@speed/account-ui` | The signed-in account-management family: sessions list, login history, social-identity bindings and MFA enrollment/step-up, driven through generated react-query hooks. | [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/account-ui/AGENTS.md) |
| `examples/reference-app/web` *(consumer shell)* | Not an `@speed/*` package — the reference app's own web host, an external member of the same pnpm workspace, composing every package above into one real browser-facing app. | [README.md](https://github.com/vislake/speed/blob/main/examples/reference-app/web/README.md) |

> [!NOTE]
> See the [reference app's own README](https://github.com/vislake/speed/blob/main/examples/reference-app/README.md)
> for the full backend + frontend picture — it is the **mandatory first
> consumer** of every module (a module API the reference app does not
> actually exercise is not considered done).
