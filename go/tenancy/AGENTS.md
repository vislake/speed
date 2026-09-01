# tenancy

The input side of speed's multi-tenant isolation story. Where `dbkit` enforces tenant isolation once a `context.Context` already carries a tenant (the GORM plugin and `Repository[T]`, both living entirely in `dbkit`), `tenancy` decides what tenant that context carries in the first place — the net/http middleware every HTTP entry point runs behind — and gives business code an audited way to deliberately step outside tenant filtering when one of a small, legitimate set of reasons applies. It sits directly above `dbkit` in the module dependency graph (`pkgcore -> dbkit -> tenancy -> ...`).

| Concern | Where |
|---|---|
| `Resolver` interface + `DomainResolver` (host/subdomain-based tenant resolution for unauthenticated requests) | `resolver.go` |
| `Middleware` + `WithAllowlist` (resolves the request's tenant and injects it into context; scoped exemptions for routes that must work before a tenant is known) | `middleware.go` |
| `WithSystemContext` (the audited wrapper around `pkgcore`'s tenant-filtering escape hatch) | `system_context.go` |
| `tenancytest.AssertIsolated[T]` / `AssertNotTenantScoped` (the mandatory isolation-assertion suite every module's `dbkit.Repository[T]` usage must run) | `tenancytest/` |

**Out of scope.** The GORM tenant-isolation plugin and `Repository[T]` itself — both live entirely in `dbkit` (`tenant_scope.go`, `repository.go`) and are already fully wired by `dbkit.Open` before `tenancy` ever enters the picture; `tenancy` does not install, wrap, or otherwise touch either. Verifying a JWT's signature and reading a tenant out of its claims — that is an authenticated-request `Resolver` implementation, which is `authn`'s responsibility once that module exists (see "Why there is no `JWTResolver` here" below). Provisioning the PostgreSQL role and RLS policy `dbkit`'s isolation layer 3 depends on remains a deployment-side responsibility neither package creates.

**Two dependencies, used unevenly across the module.** `tenancy`'s production code (`resolver.go`, `middleware.go`, `system_context.go`) imports only `pkgcore` (plus `pkgcore/apperr`) — it does not need `dbkit` or `gorm.io/gorm` at all, because it never touches a database. The `tenancytest` subpackage is the one that imports `dbkit` (for the `dbkit.TenantScoped` / `dbkit.Repository[T]` types its assertions are generic over, and `dbkit.ErrRecordNotFound` for its own error matching) and `gorm.io/gorm` (`AssertNotTenantScoped` takes a `*gorm.DB` directly, since identity/platform data is queried without a `Repository[T]`). That is also the entire reason the module depends on `dbkit` at all: not because the root package's `Resolver`/`Middleware`/`WithSystemContext` need it, but because `tenancytest` is the reusable-test-suite responsibility `dbkit/AGENTS.md`'s own "Known limitations" section had already named as `tenancy`'s to build.

## Public API

### `tenancy` — `resolver.go`

| Signature | Purpose |
|---|---|
| `type Resolver interface { Resolve(r *http.Request) (pkgcore.TenantID, error) }` | The contract `Middleware` consults exactly once per request and trusts completely. Every implementation must derive the tenant from a source the server itself controls, never from anything the client supplied on the request being resolved |
| `type DomainResolver struct { ... }` | Host/subdomain-based resolver for **unauthenticated** requests only (rendering the right brand on a login page). It grants no data access — it only decides what to display before anyone has proven who they are |
| `func NewDomainResolver(lookup func(host string) (pkgcore.TenantID, bool), defaultTenant pkgcore.TenantID) *DomainResolver` | `lookup` maps `(*http.Request).Host` to a tenant. Deciding whether a given Host is a tenant's own custom domain or a subdomain of the platform's domain — and stripping a subdomain label when it is one — is entirely `lookup`'s business; `DomainResolver` neither parses nor normalizes the Host header itself |
| `func (*DomainResolver) Resolve(r *http.Request) (pkgcore.TenantID, error)` | Never returns a non-nil error. Falls back to `defaultTenant` whenever `lookup` is nil, reports no match, or reports a match with an empty `TenantID` — a request that cannot be matched to a brand must still be able to render a login page |

**Why there is no `JWTResolver` here.** The module dependency graph runs `authn -> tenancy`, not the other way around: verifying a token's signature, managing signing keys, and validating claims is `authn`'s job, and `authn` depends on `tenancy` (for the middleware an authenticated route also runs behind), not the reverse. An authenticated-request `Resolver` living in this package would need `authn`'s key-management machinery, which would force exactly the import cycle the dependency graph is built to avoid. `DomainResolver` is the one resolution strategy that does not need any of that — it exists here because it is legitimately usable before a token has ever been verified. Once `authn` exists, it supplies its own type implementing `Resolver`, reading the tenant out of the already-verified token claims; nothing in this package needs to change for that to happen.

### `tenancy` — `middleware.go`

| Signature | Purpose |
|---|---|
| `var ErrTenantUnresolved = apperr.Forbidden("tenancy.tenant_unresolved")` | The structured error `Middleware` writes as a 403 JSON response when resolution fails and the request's (method, path) is not allowlisted. Safe to hold as a package-level sentinel — see the Rules section on matching it |
| `type MiddlewareOption func(*middlewareConfig)` | Option type for `Middleware` |
| `func WithAllowlist(method string, paths ...string) MiddlewareOption` | Exempts the given (method, paths) pairs from tenant resolution. Matching is an **exact string comparison** against both `Method` and `URL.Path` — no prefix, wildcard, case-folding, trailing-slash normalization, or GET-implies-HEAD convenience. The exemption is scoped to `method` as well as path: allowlisting `(POST, "/api/v1/orgs/invite")` does not exempt `GET` on that same literal path — each (method, path) pair needing an exemption gets its own |
| `func Middleware(resolver Resolver, opts ...MiddlewareOption) func(http.Handler) http.Handler` | Resolves the request's tenant via `resolver.Resolve` and injects it into the request context with `pkgcore.WithTenant`. On failure — including a `Resolver` that reports success with an empty `TenantID`, which is treated identically to a resolution error — responds `ErrTenantUnresolved` (403) unless the (method, path) is allowlisted, in which case the next handler still runs, just with **no tenant in its context** |

**Hard rule, not a description: `Middleware` never trusts a header, query parameter, or request body for tenant identity — under any option, in any configuration.** The resolved tenant is the only tenant source downstream code may trust. This is not a convenience default that a caller can opt out of; there is no option on `Middleware` or `MiddlewareOption` that reads a client-supplied tenant hint, and adding one would reintroduce the single most common way multi-tenant systems suffer a horizontal-privilege-escalation breach (`docs/internal/04-data-and-tenancy.md`'s trust-boundary section). A custom `Resolver` (including `authn`'s eventual authenticated one) must uphold the same rule: derive the tenant from a source the server itself controls — a verified token's claims, a database lookup — never from anything the client attached to the very request being resolved.

### `tenancy` — `system_context.go`

| Signature | Purpose |
|---|---|
| `const EventSystemContextEntered = "tenancy.system_context.entered"` | The `pkgcore.Event.Type` published every time `WithSystemContext` successfully grants the escape hatch |
| `type SystemContextEnteredEvent struct { Actor string; Purpose pkgcore.SystemPurpose; Ticket string; EnteredAt time.Time }` | The event's `Payload` — who entered, for what declared purpose, with what optional external reference, and when |
| `var ErrAuditPublishFailed = apperr.Internal("tenancy.system_context_audit_publish_failed")` | Returned when the underlying `pkgcore.WithSystemContext` call itself succeeded but publishing the resulting audit event failed |
| `func WithSystemContext(ctx context.Context, bus pkgcore.EventBus, reason pkgcore.SystemReason) (context.Context, error)` | Wraps `pkgcore.WithSystemContext`, additionally publishing a `SystemContextEnteredEvent` against the pre-elevation `ctx` before returning the elevated one. Business code — anything able to import `tenancy` — should call this instead of `pkgcore.WithSystemContext` directly; code at or below `tenancy` in the graph (`dbkit`, and `pkgcore` itself) has no choice but to use the raw primitive, since it cannot import `tenancy` without an import cycle |

**The escape hatch, audited before it is granted, fails closed if the audit itself fails.** Two distinct failure modes, both leaving `ctx` unelevated: (1) `pkgcore.WithSystemContext` rejects `reason` outright (empty `Actor`, or a `Purpose` never declared with `pkgcore.RegisterSystemPurpose`) — nothing was ever elevated, so there is nothing to audit, and the error propagates unchanged; (2) the underlying grant succeeds but `bus.Publish` fails — `WithSystemContext` returns the **original, non-elevated** `ctx` together with `ErrAuditPublishFailed.WithCause(err)`, never the context it briefly elevated. An escape hatch granted with no corresponding audit record is exactly the gap this wrapper exists to close, so a broken event bus must not silently produce an unaudited bypass.

**What this does NOT do, and must not be assumed to do: it currently does not compose with `dbkit.Repository[T]` in either direction.** `Repository[T]`'s methods never consult `pkgcore.SystemReasonFromContext`, so granting a system context on top of an already tenant-scoped context does not widen what `Repository[T]` returns beyond that same tenant, and granting one on a context with no tenant at all does not substitute for a tenant either — `Repository[T]` still fails closed with `pkgcore.ErrNoTenant`. This is not a safety hole (isolation is never weakened; if anything it is overly conservative), but it is a real functionality gap: no admin cross-tenant read or cross-tenant background job can be built on `Repository[T]` + `WithSystemContext` today. See the tracked-debt entry in `docs/internal/17-risks.md` for the two composition options still open and what closing either one must additionally satisfy — do not re-derive that decision here; extend the same entry if it changes.

### `tenancytest`

| Signature | Purpose |
|---|---|
| `func AssertIsolated[T dbkit.TenantScoped](t *testing.T, repo *dbkit.Repository[T], newRecord func(tenant pkgcore.TenantID) *T)` | The full cross-tenant isolation assertion suite against a real `dbkit.Repository[T]` |
| `func AssertNotTenantScoped(t *testing.T, db *gorm.DB, model any, createFn func(db *gorm.DB) error, findFn func(db *gorm.DB) (int64, error))` | The opposite, equally important assertion: that a model genuinely is **not** affected by `dbkit`'s tenant-scoping plugin |

**Which one to call is decided by data domain, never both** (`docs/internal/04-data-and-tenancy.md`'s data-domain table): tenant data and link data implement `dbkit.TenantScoped` and are used as `T` in a `dbkit.Repository[T]` — call `AssertIsolated`. Identity data and platform data must **not** implement `dbkit.TenantScoped`, and are queried through a plain `*gorm.DB` from `dbkit.Open` instead — call `AssertNotTenantScoped`. Getting this backwards either fails to compile (`AssertIsolated`'s constraint requires `TenantScoped`) or silently asserts the wrong property.

`AssertIsolated` creates two tenants' worth of data via `newRecord` (called repeatedly per tenant; every call must return a record with a distinct, non-empty exported `"ID"` field — the same field-name convention `dbkit.Repository[T]` itself relies on) and checks, as named subtests: `List` scopes to the calling tenant; same-tenant `FindByID` succeeds and cross-tenant `FindByID` returns `dbkit.ErrRecordNotFound`; same-tenant `Update`/`Delete` succeed; a cross-tenant `Update`/`Delete` attempt is denied without corrupting the real row or creating a phantom row under the attacking tenant; `Create` overwrites a forged `TenantID` on the struct with the context's real tenant; and `Create`/`List` on a context with no tenant at all fail closed with `pkgcore.ErrNoTenant`. The two tenant ids it generates are derived from `t.Name()` (bounded and hash-suffixed past 64 characters, so a long, descriptive subtest name can never silently produce a `tenant_id` PostgreSQL rejects while SQLite accepts unchecked), so concurrent or repeated test runs never collide and a failure message already names the test that produced it.

`AssertNotTenantScoped` first fails the test outright if `model` implements `dbkit.TenantScoped` at all (that is `AssertIsolated`'s job instead), then drives `createFn`/`findFn` against sessions carrying no tenant and two arbitrary tenants in turn, asserting `findFn`'s count moves in lockstep with `createFn` calls regardless of which tenant, if any, was current — proving visibility is not tied to any tenant context, not merely that it happens to work under the one or two contexts a less thorough check might try. Neither function opens or migrates a database itself; callers build the `*gorm.DB` with `github.com/vislake/speed/go/dbkit/dbtest` and apply their own model's schema first.

**Postgres-backed cases require Docker and live behind a build tag.** Both assertions' SQLite-backed cases run in the default `go test ./...` path. Their Postgres-backed cases live under `tenancytest/integration_test/` behind `//go:build integration` (run with `go test -tags=integration ./...`), use `dbkit/dbtest.NewPostgres` (which itself `t.Skip`s when no Docker, or Docker-API-compatible, daemon is reachable), and are not part of a plain `go test ./...` run — keeping a Docker dependency out of the default, everyday test path.

## Typical integration

Protecting an unauthenticated route (a login page, public branding) with `DomainResolver` and `Middleware`:

```go
resolver := tenancy.NewDomainResolver(lookupTenantByHost, "public") // "public" is the default tenant

mux := http.NewServeMux()
mux.HandleFunc("/login", loginPageHandler)
mux.HandleFunc("/healthz", healthCheckHandler)

protected := tenancy.Middleware(resolver, tenancy.WithAllowlist(http.MethodGet, "/healthz"))(mux)
```

A handler downstream of `Middleware` reads the tenant it already resolved — never a header, query parameter, or body:

```go
func loginPageHandler(w http.ResponseWriter, r *http.Request) {
	tenant, ok := pkgcore.TenantFromContext(r.Context())
	// render this tenant's brand; ok is false only for an allowlisted
	// request whose resolution failed, which /login itself is not.
}
```

Entering the audited system context from an allowlisted registration handler or a `jobs` task:

```go
ctx, err := tenancy.WithSystemContext(ctx, bus, pkgcore.SystemReason{
	Actor:   "authn.registration",
	Purpose: purposeNewAccountProvisioning, // pre-declared with pkgcore.RegisterSystemPurpose
	Ticket:  "",
})
if err != nil {
	return err // no escape hatch was granted -- pkgcore rejected reason, or the audit publish failed
}
```

This exact `DomainResolver` + `Middleware` pattern — including the never-trust-a-client-header property — is compiled and run under CI as `Example()` in [`example_test.go`](example_test.go), matching `pkgcore`'s and `dbkit`'s own `example_test.go` convention. `WithAllowlist`'s fail-closed-unless-allowlisted behavior and `WithSystemContext`'s audit trail each have their own runnable example alongside it.

## Rules

**Dependencies**

- Do not let this module import `authn`, or anything else above it in the graph (`pkgcore -> dbkit -> tenancy -> config / jobs -> storage / notification -> authn / rbac / org / metering -> ...`). An import running the other way is a cycle.
- Do not implement a `Resolver` that verifies a token's signature or manages signing keys here. That is `authn`'s responsibility precisely because `authn` depends on `tenancy`, not the reverse — see "Why there is no `JWTResolver` here" above.
- Do not import `dbkit` or `gorm.io/gorm` from the root `tenancy` package. Only `tenancytest` needs either, and only because its assertions are generic over `dbkit.Repository[T]` / `dbkit.TenantScoped` — adding either import to `resolver.go`, `middleware.go`, or `system_context.go` is a sign something has gone architecturally wrong, not a normal change.

**Resolver**

- Do not have a custom `Resolver` derive a tenant from anything the client controls on the request being resolved. Every implementation, including a future `authn`-supplied one, must use a source the server itself controls.
- Do not copy `DomainResolver`'s empty-Host-falls-back-to-default behavior into a general pattern. It is a deliberate, narrowly documented exception scoped to the unauthenticated case — a login page must still render something. Every other `Resolver` should return a non-nil error rather than invent or default a tenant.
- Do not have `DomainResolver` (or its `lookup` function) parse or strip anything beyond what `net/http` already populates in `(*http.Request).Host`. Distinguishing a tenant's own custom domain from a subdomain of the platform's domain, and stripping a subdomain label, is `lookup`'s job.

**Middleware**

- Do not read a tenant from a request header, query parameter, or body anywhere in the request path — in `Middleware`, in a custom `Resolver`, or downstream. See the hard rule under `middleware.go`'s API entry above; there is no configuration that relaxes it.
- Do not allowlist a path without pinning its method. `WithAllowlist(method, paths...)` scopes the exemption to that exact (method, path) pair; a public `POST` on a path does not exempt `GET` (or any other method) on that same literal path.
- Do not expect prefix, wildcard, case-folding, trailing-slash, or GET-implies-HEAD matching from `WithAllowlist`. It is an exact string comparison against both `Method` and `URL.Path` — allowlist `http.MethodHead` explicitly if a health check needs it too.
- Do not treat a `Resolver` returning `("", nil)` as a successful zero-tenant resolution, in `Middleware` or in any code inspecting a `Resolver`'s result. `Middleware` treats it exactly like a resolution failure; write a custom `Resolver` to preserve this rather than exploit it as a way to signal "no tenant needed."
- Do not include the `Resolver`'s own error in a response body. `Middleware` never does, because it may carry internal detail (once `authn` supplies the authenticated-request `Resolver`, that could be token-validation internals).

**System context**

- Do not call `pkgcore.WithSystemContext` directly from code that is able to import `tenancy`. Use `tenancy.WithSystemContext` so the grant is audited. The raw primitive remains correct only for code at or below `tenancy` in the graph, which cannot import `tenancy` at all.
- Do not assume `WithSystemContext` widens what `dbkit.Repository[T]` can see, in either direction. It does not compose with `Repository[T]` today — see the cross-reference above and the tracked-debt entry in `docs/internal/17-risks.md`.
- Do not treat a `WithSystemContext` failure as anything other than "no escape hatch was granted." Both failure modes return the original, unelevated `ctx`; never use the `context.Context` value from a call whose error was non-nil.
- Do not invent a `SystemPurpose` at the call site. It must already be declared with `pkgcore.RegisterSystemPurpose`, from the calling module's own registration.

**tenancytest**

- Do not call `AssertIsolated` for identity or platform data, or `AssertNotTenantScoped` for tenant or link data. `docs/internal/04-data-and-tenancy.md`'s data-domain table decides which; getting it backwards either fails to compile or asserts the wrong property.
- Do not skip `AssertNotTenantScoped` for identity/platform data because it "obviously" is not tenant-scoped. Its entire purpose is catching a table that got GORM-plugin-scoped by accident — which shows up in production as data that mysteriously stops appearing, not as an isolation bug, and is correspondingly harder to trace back to its cause.
- Do not run `AssertIsolated`'s or `AssertNotTenantScoped`'s Postgres-backed cases as part of a plain `go test ./...`. They belong under `tenancytest/integration_test/` behind `-tags=integration` specifically so a default test run never starts a Docker container.
- Do not have `newRecord` return a record whose exported `"ID"` field repeats across calls. `AssertIsolated` fails the test immediately the first time it sees a duplicate, since every assertion keyed on that id would otherwise be ambiguous.

**Documentation**

- Do not add an exported identifier to `tenancy` or `tenancytest` without a doc comment, an entry in the tables above, and — for a new public entry point, not every incidental type — a compiling `Example` in `example_test.go`, in the same pull request.

## Error index

| Sentinel | Triggered by | Handling |
|---|---|---|
| `ErrTenantUnresolved` (`tenancy.tenant_unresolved`) | `Middleware`, when `resolver.Resolve` fails (or reports success with an empty tenant) on a (method, path) not covered by `WithAllowlist` | The 403 response is already written. Check the `Resolver`'s own source of truth (the custom-domain lookup today; eventually the access token) |
| `ErrAuditPublishFailed` (`tenancy.system_context_audit_publish_failed`) | `WithSystemContext`, when the underlying `pkgcore.WithSystemContext` grant succeeded but `bus.Publish` failed | Fails closed: `ctx` comes back unelevated. Treat exactly like any other `WithSystemContext` failure — fix or retry whatever broke the event bus, then call again |
| `pkgcore.ErrSystemActorRequired` / `pkgcore.ErrSystemPurposeNotRegistered` | `WithSystemContext`, when the underlying `pkgcore.WithSystemContext` call rejects `reason` outright (empty `Actor` / undeclared `Purpose`) before anything is elevated | Propagated unchanged — nothing was granted, so nothing needed auditing. Fix `reason` at the call site; see `pkgcore/AGENTS.md`'s own error index |
| `dbkit.ErrRecordNotFound` (surfaces through `tenancytest.AssertIsolated`'s own failures, not returned by this module) | A cross-tenant `FindByID`/`Update`/`Delete` during `AssertIsolated`, exactly as `dbkit`'s own error index documents | Not this package's error to fix — a test failure here means the `Repository[T]` under test is not isolating correctly |

Every `*apperr.Error` sentinel above follows the same convention as `pkgcore`'s and `dbkit`'s own: `WithParam`/`WithCause` always derive a new instance rather than mutate the receiver, so match a *decorated* error with `apperr.As(err)` and compare `.Code`, not `errors.Is`/`==` against the package-level var.

Design rationale lives in `docs/internal/04-data-and-tenancy.md` (the data-domain table, the tenant-context trust boundary, the three-layer isolation defense, and the system-context escape hatch) and `docs/internal/01-architecture.md` (the module dependency graph). Tracked, not-yet-resolved design debt lives in `docs/internal/17-risks.md`.
