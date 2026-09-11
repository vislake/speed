---
title: tenancy
weight: 3
description: "The input side of multi-tenant isolation — the resolver-and-middleware pair that decides which tenant a request carries, and the audited system-context wrapper for the rare legitimate step outside tenant filtering."
---

# tenancy

The input side of speed's multi-tenant isolation story: where `dbkit`
enforces isolation once a context already carries a tenant, `tenancy`
decides what tenant that context carries in the first place — the
`net/http` middleware every HTTP entry point runs behind — and gives
business code an audited way to step deliberately outside tenant
filtering for the small set of legitimate reasons. It sits directly
above `dbkit` in the dependency graph and imports only `pkgcore`
outside its test-support subpackage.

The middleware never trusts a request for its tenant. `Resolver` is
the contract consulted exactly once per request, and every
implementation must derive the tenant from a source the server itself
controls — a verified token's claims, a database lookup — never from a
header, query parameter or body the client supplied. This is a hard
rule of the module, not a default that can be configured away.

## When to choose it

Every request-bearing service uses the `Middleware`; the question is
which resolver. An **unauthenticated** entry (a login page, public
branding) uses the built-in `DomainResolver`, whose `lookup` function
maps `(*http.Request).Host` to a tenant and which falls back to a
default tenant — never an error — so a page can always render. An
**authenticated** request resolves its tenant from the verified access
token's claims; that resolver is `authn`'s own type implementing
`Resolver` — there is deliberately no `JWTResolver` here, because
verifying tokens is `authn`'s job and the dependency graph runs
`authn -> tenancy`, not the reverse.

## Wiring and minimal use

```go
resolver := tenancy.NewDomainResolver(lookupTenantByHost, "public") // default tenant

mux := http.NewServeMux()
mux.HandleFunc("/login", loginPageHandler)
mux.HandleFunc("/healthz", healthCheckHandler)

protected := tenancy.Middleware(resolver, tenancy.WithAllowlist(http.MethodGet, "/healthz"))(mux)
```

A handler downstream reads the tenant the middleware already resolved:

```go
tenant, ok := pkgcore.TenantFromContext(r.Context())
// ok is false only for an allowlisted request whose resolution failed.
```

Allowlisting is exact: `WithAllowlist(method, paths...)` exempts that
exact (method, path) pair from the 403 a failed resolution would
answer — never from tenant injection itself when resolution succeeds,
and with no prefix, wildcard or GET-implies-HEAD convenience. An
allowlisted request whose tenant does not resolve proceeds with *no
tenant in its context*, which is what lets pre-tenant routes such as
sign-in keep working. Failure answers `tenancy.tenant_unresolved`
(403), a structured `apperr` whose resolver-side detail is never
echoed into the response.

Optional and off by default: `WithTenantStatusResolver` wires a
`TenantStatusResolver` module so a resolved tenant's suspended status
actually refuses requests (`tenancy.tenant_suspended`), a failing
`Status` call refusing closed with `tenancy.tenant_status_unavailable`
— an unreachable status source is an outage, never "no news is good
news". The module interface is structurally typed; `admin`'s tenant ledger is its
first real implementer.

## The audited escape hatch

Cross-tenant work goes through `pkgcore.WithSystemContext` — but code
that can import `tenancy` calls `tenancy.WithSystemContext` instead,
which publishes a `tenancy.system_context.entered` audit event before
returning the elevated context, and fails closed if that publish
fails: a grant with no audit record is exactly the gap the wrapper
exists to close, so a broken bus returns the original, unelevated
context with `ErrAuditPublishFailed`. The reason must be pre-declared
(`RegisterSystemPurpose`) and carries an actor and an optional ticket.

```go
ctx, err := tenancy.WithSystemContext(ctx, bus, pkgcore.SystemReason{
    Actor:   "authn.registration",
    Purpose: purposeNewAccountProvisioning, // declared with pkgcore.RegisterSystemPurpose
    Ticket:  "",
})
if err != nil {
    return err // nothing was granted: reason rejected, or the audit publish failed
}
```

What the wrapper does **not** do: it does not widen what
`dbkit.Repository[T]` can see. Repositories never widen on a system
context's presence (the one exception is `HardDelete`, which *requires*
it as a gate), and a system context never substitutes for a tenant —
tenantless calls still fail closed with `pkgcore.ErrNoTenant`.

## Core concepts and API essentials

- **`Resolver`** (`Resolve(r *http.Request) (pkgcore.TenantID, error)`)
  and `DomainResolver`; `Middleware(resolver, opts...)` injects via
  `pkgcore.WithTenant`.
- **`WithSystemContext`** + `EventSystemContextEntered` +
  `SystemContextEnteredEvent{Actor, Purpose, Ticket, EnteredAt}` —
  the audited wrapper described above.
- **`tenancytest`** — `AssertIsolated[T]` (tenant/link data through a
  `dbkit.Repository[T]`: cross-tenant reads denied, forged tenant ids
  overwritten, no-tenant contexts fail closed) and
  `AssertNotTenantScoped` (identity/platform data: visibility provably
  independent of any tenant context). Every module with a repository
  runs one of the two — which one is decided by data domain, never by
  taste — with PostgreSQL-backed cases behind `-tags=integration`.

## Boundaries and pitfalls

- The GORM isolation plugin and `Repository[T]` live in `dbkit`, not
  here — `tenancy` neither installs nor wraps them.
- Do not copy `DomainResolver`'s fall-back-to-default behavior into an
  authenticated resolver: that exception exists so a login page can
  render; every other resolver returns a non-nil error rather than
  inventing a tenant.
- Do not treat `Resolver` answering `("", nil)` as success — the
  middleware refuses it exactly like a resolution failure.
- Suspension enforcement covers exactly the routes the host did not
  allowlist; a suspension takes effect on the very next
  non-allowlisted request because nothing is cached.
- Entering a system context from code that can import `tenancy` by
  calling the raw `pkgcore` primitive bypasses the audit record — the
  wrapper is the sanctioned path.

## Source

- [tenancy AGENTS.md](https://github.com/vislake/speed/blob/main/go/tenancy/AGENTS.md)
