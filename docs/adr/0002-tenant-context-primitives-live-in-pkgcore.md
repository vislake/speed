# ADR 0002: Tenant-context primitives live in package `pkgcore`, not `tenancy`

- **Status**: Accepted
- **Date**: 2026-09-01
- **Context of discovery**: found and fixed during Round 1 implementation of `go/pkgcore`, confirmed by independent gap-check audit.

## Context

Earlier drafts of the design (`docs/internal/04-data-and-tenancy.md`'s code samples, `CLAUDE.md`'s discipline bullets, and matching examples in `docs/internal/05-identity-and-access.md` and `docs/internal/18-cicd.md`) referred to the tenant-context primitives — `WithTenant`, `FromContext`, `MustFromContext`, `WithSystemContext`, `SystemReason`, `TenantID` — as living in a `tenancy` package, called as `tenancy.WithTenant(...)`, `tenancy.FromContext(...)`, and so on.

`docs/internal/01-architecture.md`'s dependency graph states:

- `tenancy --> dbkit` (the `tenancy` module depends on `dbkit`, for its GORM tenant-isolation plugin)
- `tenancy --> pkgcore`

`docs/internal/04-data-and-tenancy.md` itself specifies that `dbkit.Repository[T]`, on every read, must resolve the current tenant from context and fail closed if none is present (the doc states, in Chinese: "on read, if the tenant cannot be obtained, fail closed immediately"). This means `dbkit` needs direct access to the tenant-context read/write functions. If those functions lived in package `tenancy`, `dbkit` would have to import `tenancy` to use them — but `tenancy` already imports `dbkit`. That is a two-package import cycle: `dbkit -> tenancy -> dbkit`. This does not compile.

## Decision

The raw tenant-context primitives are defined directly in package `pkgcore` — the one module every other module in the dependency graph, including both `dbkit` and `tenancy`, already depends on:

```go
// package pkgcore
type TenantID string
func WithTenant(ctx context.Context, id TenantID) context.Context
func TenantFromContext(ctx context.Context) (TenantID, bool)
func MustTenantFromContext(ctx context.Context) (TenantID, error)
type SystemReason struct { Actor string; Purpose SystemPurpose; Ticket string }
func RegisterSystemPurpose(p SystemPurpose)
func WithSystemContext(ctx context.Context, reason SystemReason) (context.Context, error)
func SystemReasonFromContext(ctx context.Context) (SystemReason, bool)
```

`dbkit` calls these directly. The `tenancy` module (not yet built as of this writing) is expected to provide a thin, richer wrapper on top of the same primitives for business-module convenience — in particular, `tenancy.WithSystemContext` is expected to call `pkgcore.WithSystemContext` and additionally publish an audit event, since audit publication requires depending on machinery (the event bus, eventually the `compliance` module's consumers) that has no reason to live in `pkgcore`. Until `tenancy` exists, all call sites — including business-module code — use the `pkgcore` primitives directly; `CLAUDE.md` and the coding-standards skills have been updated to reflect this as the current, correct guidance, with a note to revisit once `tenancy` lands.

## Consequences

- `dbkit` can implement tenant-scoped, fail-closed repositories without depending on `tenancy`.
- `tenancy` remains free to depend on `dbkit` (for its GORM plugin) without creating a cycle.
- Business-module code written before `tenancy` exists will need a mechanical rename (`pkgcore.WithTenant` → `tenancy.WithTenant`, etc.) once `tenancy` ships its wrapper, if the project decides business code should route through the richer wrapper rather than continuing to call `pkgcore` directly. That decision is deferred to the round that implements `tenancy`.
- The original design documents (`docs/internal/04-data-and-tenancy.md`, `docs/internal/05-identity-and-access.md`, `docs/internal/18-cicd.md`, `CLAUDE.md`, and `.claude/skills/backend-coding-standards/SKILL.md`) have been corrected to match this decision as of the same change that produced this ADR.
