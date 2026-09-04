---
name: backend-coding-standards
description: speed Backend Coding Standards — Mandatory module boundaries, multi-tenant isolation, dual deployment modes, error handling, API, logging and testing rules for writing, editing and reviewing all Go code under go/
triggers:
  - writing backend code
  - editing backend code
  - creating Go files
  - creating API endpoints
  - editing API endpoints
  - adding handler/service/repository
  - backend code review
  - database migrations
  - backend refactoring
  - fixing backend bugs
  - adding backend tests
globs:
  - "go/**/*.go"
  - "go/**/migrations/**/*.sql"
  - "go/**/api/openapi.yaml"
  - "go.work"
---

# speed Backend Coding Standards

This is the single authoritative standard for the Go side. Design rationale lives in `docs/internal/` (written in Chinese, for internal design discussion); the discipline list lives in the root `CLAUDE.md`. **This document tells you how to write the code.**

**The premise that overrides everything else**: speed is not an application — it is a set of libraries that business projects pull in via `go get`. Every exported signature change propagates to every delivered project; every extra dependency lands in someone else's `go.sum`. This premise outranks any stylistic preference below.

---

## 1. Module Boundaries

Independent Go modules, each with its own `go.mod`, developed together through `go.work`. The dependency graph lives in `docs/internal/01-architecture.md` and **only flows bottom-up**.

**Required:**
- Module path is `github.com/<org>/speed/go/<module>`.
- **Package name derives from the directory by stripping hyphens**, since `-` is not a legal character in a Go identifier: `ai-gateway` → package `aigateway`. (A subpackage takes its own directory name: `go/billing/gateway` is package `gateway`.) Lowercase, no separator — do not substitute an underscore or camelCase. Every module generator (including the future `task new:module`) must apply this rule consistently rather than leaving it to individual judgement.
- Every module carries: `api/openapi.yaml`, `migrations/{postgres,sqlite}/`, `locales/{zh-CN,en-US}.toml`, `docs/`, `AGENTS.md`.
- Public API stays in the module root package; implementation details go under `internal/` so consumers cannot import them.
- Create new modules with the scaffolder, `python3 tools/new_module.py` — the planned `task new:module` Taskfile wrapper will call it once wired (see `docs/internal/19-dev-workflow.md`). The script scaffolds the canonical stub and prints a registration checklist — the go.work `use` entry, the CI matrix row, the lockstep release list — the entries hand-rolling a module always misses; it never edits those shared files itself.

**Prohibited:**
- **DO NOT** let `rbac` depend on `authn`. Authorization only knows `Subject{TenantID, UserID}`; whoever authenticates assembles the Subject and calls authorization.
- **DO NOT** import another business module's structs for database relations — use ID references plus domain events.
- **DO NOT** import concrete infrastructure SDKs (`go-redis`, S3 SDK, payment SDKs) inside business modules — depend on the `pkgcore` interfaces.
- **DO NOT** create cross-module foreign keys. Store IDs only.
- **DO NOT** park types in `pkgcore` for convenience — it is the dependency floor for every module, so anything added there is a global cost.

## 2. Module Wiring

Every module implements `pkgcore.Module` and registers routes, config schema, feature flags, permissions, job handlers, notification types, events and audit actions through a single `Register(reg *Registry)`.

```go
func (m *Module) Register(reg *pkgcore.Registry) error {
    reg.Routes.Mount("/api/v1/billing", billingapi.Handler(m.svc)) // implements the spec-generated interface
    reg.Config.Add(configSchema...)
    reg.Features.Add(featureFlags...)
    reg.Permissions.Add("billing:read", "billing:manage")
    reg.Jobs.Handle(&InvoiceGenerator{svc: m.svc})
    reg.Notifications.Add(notificationTypes...)
    reg.Events.Subscribe("org.member.invited", m.onMemberInvited)
    return nil
}
```

**Prohibited:**
- **DO NOT** use `init()` for business registration — everything goes through `Register`, wired at compile time with `google/wire`.
- **DO NOT** perform I/O inside `Register` (no DB connections, no outbound calls) — it only declares; actual startup belongs in `Start(ctx)`.

## 3. Multi-Tenant Isolation (highest priority)

Violating anything in this section is a security defect, not a style issue.

### 3.1 Tenant context has exactly one source

```go
tid, ok := pkgcore.TenantFromContext(ctx) // the only way to read it
```

- **DO NOT** read `tenant_id` from headers, query parameters or request bodies. For authenticated requests the tenant comes from the access token claims and is injected into the context by `tenancy.Middleware` (which calls `pkgcore.WithTenant` under the hood).
- **DO NOT** expose a `tenant_id` parameter on the API surface — spec lint rejects it.

### 3.2 Data access goes through Repository only

```go
// Correct: embed the generic base, tenant filtering is injected automatically
type Repository struct {
    dbkit.Repository[Subscription]
}

// Wrong
db.Where("tenant_id = ?", tid).Find(&subs) // hand-written tenant filter means you are bypassing the guard
db.Table("subscriptions").Find(&subs)      // bypasses Repository
db.Raw("SELECT ...").Scan(&subs)           // same
```

- **DO NOT** hold a `*gorm.DB` inside a business module.
- **DO NOT** hand-write `WHERE tenant_id = ?` — needing to write it means you took the wrong path.
- When a reporting query genuinely needs raw SQL, put it under the module's `internal/query/`, pass the tenant explicitly, and add an isolation test for it.

### 3.3 Data domains

Before creating a table, decide which domain it belongs to (see `docs/internal/04-data-and-tenancy.md`):

| Domain | `TenantScoped`? | Test suite |
|---|---|---|
| Tenant data | Yes | `tenancytest.AssertIsolated` |
| Identity data (users / sessions / login logs) | No | `tenancytest.AssertNotTenantScoped` |
| Platform data (platform plans / channel config) | No | `AssertNotTenantScoped` |
| Link data (memberships) | Yes | `AssertIsolated` |

**Both assertions must run.** A globally visible table that accidentally gets tenant filtering shows up as "the data mysteriously disappeared", which is extremely expensive to debug.

### 3.4 System context is the only escape hatch

```go
ctx, err = pkgcore.WithSystemContext(ctx, pkgcore.SystemReason{
    Actor: actor, Purpose: "admin.tenant_search", Ticket: "SUP-1234",
})
```

- Callable only from `admin`, `compliance`, `jobs` and `authn` — plus `tenancy`'s audited wrapper `tenancy.WithSystemContext`, which business code should call instead of the raw primitive. The caller whitelist is enforced by code review / CODEOWNERS on `go/pkgcore` and `go/tenancy` plus the doc comments on both functions, NOT by depguard: depguard denies whole import paths per file and cannot single out one symbol (`WithSystemContext`) from the rest of an otherwise-needed package. `pkgcore`'s root package also holds `TenantID`/`WithTenant`/`apperr`, which `go/dbkit` — real code, not on the whitelist — legitimately imports; a draft rule shaped "only the whitelist may import `pkgcore`" flagged 23 of dbkit's pre-existing unrelated imports as collateral damage and was reverted. The full reasoning lives in the `.golangci.yml` depguard comment; making this checkable would mean moving `WithSystemContext` into its own `pkgcore` subpackage (a public API decision, not a lint-config side effect).
- `Purpose` is an enum, not free text.
- Keep the scope as narrow as possible — **DO NOT** enable it "conveniently" in middleware.
- It bypasses tenant filtering only. **It never bypasses RBAC.**

## 4. Dual Deployment Modes

The same business code must run correctly under both the standalone (single process / SQLite / in-memory implementations) and distributed (PostgreSQL / Redis) deployment modes.

```go
// Correct: depend on the interface
type Service struct { kv pkgcore.KVStore }

// Wrong
import "github.com/redis/go-redis/v9"
if mode == "standalone" { ... } // deployment-mode branches belong only in kernel wiring
```

- **DO NOT** branch on the deployment mode inside business logic.
- **DO NOT** expose capabilities on an interface that only one implementation can satisfy (such as Redis Lua scripting). When atomicity is required, define semantics both sides can honour, like `IncrByFloat` or `CompareAndSwap`.
- Any new infrastructure dependency **must ship both implementations**. One implementation is not "done".

## 5. Data Models and Migrations

```go
type Subscription struct {
    ID        string    `gorm:"primaryKey;size:26"`  // ULID generated in the application
    TenantID  string    `gorm:"primaryKey;size:26;index:idx_tenant_created,priority:1"`
    Status    string    `gorm:"size:32;not null"`
    Metadata  datatypes.JSON
    CreatedAt time.Time `gorm:"autoCreateTime;index:idx_tenant_created,priority:2"`
}

func (Subscription) GetTenantID() pkgcore.TenantID { return ... } // implements TenantScoped
```

**Dual-dialect hard constraints** (PostgreSQL in the distributed deployment mode, SQLite in the standalone deployment mode — both must pass):
- Generate IDs in the application (ULID/UUID). **DO NOT** use `gen_random_uuid()`.
- Use `datatypes.JSON`. **DO NOT** rely on JSONB operators for filtering.
- **DO NOT** use native PostgreSQL arrays — use JSON or a join table.
- **DO NOT** write `NOW()` — use `autoCreateTime` / `autoUpdateTime`.
- Full-text search goes through the `SearchProvider` interface: tsvector on PostgreSQL, `LIKE` fallback on SQLite.
- `tenant_id` is the leftmost column of every composite index; tenant tables use `(tenant_id, id)` as the primary key.

**Migrations:**
- GORM structs are the schema source of truth. Atlas generates versioned SQL per dialect, exposed through `embed.FS` and aggregated by `dbkit.MigrationRegistry`.
- **DO NOT** use `AutoMigrate` — it is neither auditable nor reversible.
- Migrations must run from zero to head, verified on both dialects.

**Sensitive fields:**
```go
Phone      string `gorm:"serializer:encrypted"` // Serializer provided by dbkit
PhoneIndex string `gorm:"index"`                // HMAC blind index for equality lookups
```
Encrypted fields cannot be queried directly. Whenever equality lookup is needed, add a blind index column and normalize on write (E.164 for phone numbers, lowercase for emails).

## 6. API and Errors

### 6.1 Spec-first — the order is not negotiable

The only correct sequence for changing an endpoint: **edit `api/openapi.yaml` → `task api:gen` → compilation failures reveal every handler to fix → implement → update the frontend → commit together**.

- **DO NOT** change the implementation first and backfill the spec — that is code-first again and throws away the entire compile-time guarantee.
- Handlers must implement the generated server interface. Do not hand-write route binding or parameter parsing.
- `operationId` follows `<module>_<action><Resource>`; schema names follow `<Module><Type>`; paths are prefixed `/api/v1/<module>`.

### 6.2 Errors

```go
return apperr.NotFound("billing.subscription_not_found").
    WithParam("id", id)
```

- Error codes follow `<module>.<reason>` and are registered in the module error catalog, from which the docs entry is generated.
- **DO NOT** return localized text — APIs return a structured error code plus parameters; the frontend resolves it through i18n.
- **DO NOT** use `panic` for error handling (startup-only unrecoverable failures excepted).
- **DO NOT** let internal details (SQL fragments, stack traces, internal IDs) reach the response body.
- Every error carries the trace ID so it can be correlated across logs.

## 7. Asynchronous Work

```go
func (h *ImageGenHandler) Handle(ctx context.Context, job *jobs.Job, progress jobs.ProgressFn) (jobs.Result, error) {
    ctx = pkgcore.WithTenant(ctx, job.TenantID) // the tenant context must be rebuilt
    ...
}
```

- **DO NOT** assume a worker inherits tenant context — without rebuilding it, Repository fails closed.
- **DO NOT** run long operations synchronously inside an HTTP request — enqueue a job and report progress.
- **DO NOT** put business compensation inside the queue layer. The queue provides an `OnFailure` hook; refunding credits and similar compensation belongs to the business module.
- An idempotency key is mandatory and must be derived from the business operation, never random.

## 8. Events and Notifications

- Business modules **do not depend on `notification`**. They publish domain events; notification subscribes and decides what to send. The only exception is synchronous verification-code delivery.
- **DO NOT** forward internal domain events straight to outbound webhooks — map them to a public event schema that is versioned independently.
- Event names follow `<module>.<entity>.<action>`, e.g. `billing.subscription.created`.

## 9. Metering and Billing

- Billing-grade metering goes through the **outbox** pattern (written in the same transaction as the business write). Only analytics-grade metering may fail open with an in-memory buffer.
- Quota decisions go through `billing.Entitlements.Check`. **DO NOT** read subscription tables and compute quota yourself.
- Pay-per-use flows must follow **reserve → confirm/refund**; the failure path must refund.

## 9.5 Security Red Lines

These are the ones most often missed during implementation, and missing them is a security incident rather than a style issue:

- **Outbound requests must be SSRF-protected.** Tenant-configurable destinations (webhooks, OIDC issuers, S3 endpoints) must be validated against private IP ranges before the request is made, including DNS-rebinding protection: validate the resolved address and connect to that exact address.
- **DO NOT** send messages to unverified phone numbers or email addresses. External contacts must complete consent verification (`verified_contacts` status `verified`); the verification message itself is the only exception and is rate limited. **Re-check the status at delivery time**, not only at enqueue time.
- **Impersonation audit records must carry both identities**: `Actor` is the impersonated user, `OnBehalfOf` is the real administrator. Without the latter, an investigation shows "the user deleted their own data".
- **Never merge social login accounts on matching email alone.** Auto-link only when the provider returns `EmailVerified=true` and is on the trusted list; otherwise require sign-in first, then binding.
- **Passwords use argon2id** with configurable parameters. **DO NOT** use bcrypt or a homegrown hashing scheme.
- **DO NOT** concatenate user input into SQL. **DO NOT** write internal error details, secrets or tokens into responses or logs.

## 10. Configuration and Constants

**DO NOT** scatter magic numbers. Timeouts, limits, intervals and thresholds must be one of:
1. Bootstrap config (`pkgcore/config`, koanf, `SPEED_` env prefix) — values that vary by environment.
2. Dynamic config (the `config` module, tenant-overridable, editable from the admin console) — values operations needs to tune.
3. Package-level named constants — stable domain defaults.

```go
// Correct
const defaultListLimit = 20
timeout := cfg.Jobs.HandlerTimeout

// Wrong
time.NewTicker(30 * time.Second)
db.SetMaxOpenConns(10)
secret := "speed-jwt-secret" // never hardcode secrets
```

Every new dynamic config item must register its schema first (key, type, default, range, sensitivity, description). The admin form and the docs are both generated from it.

## 11. Logging and Observability

**Structured logging only.** The backend uses `log/slog` through the `observability` wrapper. Logs are machine-parsed by Loki and correlated with traces — a formatted sentence cannot be queried, filtered or aggregated.

```go
log := obs.FromContext(ctx) // carries trace_id, span_id, tenant_id automatically
log.Info("subscription activated",
    "subscription_id", sub.ID,
    "plan_id", sub.PlanID,
    "amount_cents", sub.Amount.Cents,
)
```

**Rules:**
- **DO NOT** use `fmt.Println`, `fmt.Printf`, `log.Printf`, or `panic` for diagnostics.
- **DO NOT** build messages by concatenation or `fmt.Sprintf` — the message must be a **constant string**, and everything variable belongs in key-value attributes. `log.Info("user " + id + " logged in")` is wrong; `log.Info("user logged in", "user_id", id)` is right.
- **Always take the logger from the context** (`obs.FromContext`) so trace and tenant correlation is preserved. Never create a fresh logger inside a request path.
- Keys are `snake_case`, stable, and reused across modules: `tenant_id`, `user_id`, `job_id`, `trace_id`, `duration_ms`, `error`. Do not invent `userId` in one place and `uid` in another — inconsistent keys make logs unqueryable.
- Message strings are lowercase, no trailing punctuation, and describe an event that happened: `"payment webhook received"`, not `"Received the payment webhook!"`.
- Log levels: `Debug` for developer detail (off in production), `Info` for state changes worth auditing operationally, `Warn` for recovered anomalies, `Error` for failures needing human attention. **DO NOT** log an error and also return it — pick one, otherwise every failure appears three times up the stack.
- **DO NOT** log plaintext PII, secrets, tokens or full prompts. Redaction is on by default; do not defeat it.
- **DO NOT** use `tenant_id` as a Prometheus metric label — high cardinality will take Prometheus down. It belongs in span attributes and log fields.
- Every module must emit the key metrics listed in `docs/internal/09-observability.md`.

## 12. Internationalization

- All user-facing text goes through `locales/{zh-CN,en-US}.toml` plus error codes. **DO NOT** write literal user-facing strings in code.
- Backend-generated content (emails, invoices, notifications) renders in the **recipient's** locale, not the operator's UI language.
- New keys must exist in both languages; CI checks that the key sets match.

## 13. Testing

| Tier | Dependencies | When |
|---|---|---|
| Unit | None (the standalone deployment mode's in-memory implementations act as test doubles) | Every commit |
| Integration | testcontainers for PostgreSQL / Redis | Before merge |
| Contract | None | Every commit |

### File and directory layout — not optional

This is a hard convention, not a preference, because it is what makes `go test ./...` fast and `go test -tags=integration ./...` meaningful:

- **Unit test files are named `<target_file>_test.go`**, co-located with the file they test — `registry.go` is tested by `registry_test.go`, `kv.go` by `kv_test.go`. This is standard Go idiom; follow it exactly, one test file per source file being verified.
- **One test file per source file, full stop.** Every test for `registry.go`, however many scenarios or sub-behaviours it covers (concurrency, edge cases, a specific serialization format, whatever), goes in the one `registry_test.go`. **Do not split by theme or behaviour** — a target with 30 test functions covering ten different scenarios is still one file, not ten. Splitting by semantic grouping is a habit to actively resist, not a style choice to make freely.
  - The only legitimate reasons to add a second file for the same target are mechanical, not organizational: Go's own constraints force it (e.g. `example_test.go` for godoc `Example*` functions must be a separate, specially-named file; a case needing `package x_test` — external test package — can't share a file with cases needing `package x`), or the single file has grown so large it is genuinely difficult to navigate (a high bar — well past a thousand lines, not a soft 600–800 line trigger; when in doubt, don't split).
  - If you do split for one of those mechanical reasons, the new file name still starts with the target's name (`registry_bootstrap_test.go`, not bare `bootstrap_test.go`) — never a generic word (`extra_test.go`, `misc_test.go`, `independent_test.go`).
  - This applies retroactively: if you notice an existing target already has multiple theme-split test files with no mechanical justification, consolidate them into one the next time you touch that area, rather than adding a fourth file to an already-fragmented set.
- **`Example*` functions live in `example_test.go`** — this is Go's own idiomatic name for godoc-rendered runnable examples and is the one recognized exception to the "name after the target" rule; do not rename it.
- **Shared test helpers, fakes, builders and assertion utilities go in a dedicated `internal/testutil` package** (e.g. `go/pkgcore/internal/testutil/`), never duplicated across `_test.go` files and never defined inline in a `_test.go` file that another package's tests need to import — Go's `_test.go` files are not importable across packages, so anything meant to be shared has to live in a regular `.go` file in its own package. If a module has no cross-file-shared test helpers yet, it does not need this directory — do not create it speculatively.
- **Integration tests are physically separate from unit tests**, in a package-level `integration_test/` subdirectory (e.g. `go/dbkit/integration_test/postgres_repository_test.go`), guarded by `//go:build integration`, so a plain `go test ./...` never touches them and CI invokes them explicitly with `-tags=integration`. Name each integration test file for what it exercises against a real dependency (`postgres_repository_test.go`, `redis_kvstore_test.go`), not `integration_test.go` alone if a package has more than one.

**Mandatory suites:**
- `tenancytest.AssertIsolated` / `AssertNotTenantScoped` — every Repository.
- Deployment-mode consistency — the same cases must produce identical results under standalone and distributed deployment modes.
- Dual-dialect matrix — `dbtest.NewPostgres(t)` and `dbtest.NewSQLite(t)`.

**Requirements:**
- Table-driven tests; case names describe behaviour: `TestSubscribe_QuotaExceeded_ReturnsBlocked`.
- Concurrency hot spots (metering counters, queues, caches, credit deduction) require `-race` tests.
- **Every bug fix ships with a test that reproduces the bug** — it must fail before the fix.
- **DO NOT** ignore warnings of any kind (compiler, vet, lint, race detector). Anything that cannot be fixed now must be called out explicitly with a follow-up.

## 14. Go Style

- Naming: no `I` prefix on interfaces; constructors are `New<Type>`; error values are `Err<Reason>`; avoid stutter (`billing.Service`, not `billing.BillingService`).
- **DO NOT** use `any` / `interface{}` when a concrete type exists.
- `context.Context` is always the first parameter. **DO NOT** store it in a struct field.
- Exported types and functions need doc comments **in English**, starting with the identifier name.
- All code comments are **English** — see the language rule in `docs/internal/13-documentation-standards.md`.
- Format with `gofumpt`; lint with `golangci-lint` using the repository-root config (no per-module overrides).

## 15. Pre-Commit Checklist

- [ ] Dependency direction respected; no cross-module struct imports
- [ ] Repository embeds `dbkit.Repository[T]`; no hand-written tenant filters
- [ ] New tables assigned to the correct data domain with the matching isolation assertion
- [ ] New infrastructure dependency ships both standalone and distributed implementations
- [ ] API change started from the spec; generated artifacts committed together
- [ ] New user-facing text present in both zh-CN and en-US
- [ ] New public API has docs, a compilable example, and an `AGENTS.md` entry
- [ ] Bug fix includes a reproducing test
- [ ] Logs are structured: constant message, `snake_case` keys, logger taken from context
- [ ] No new warnings; `-race` passes
- [ ] Migrations run from zero on both dialects
