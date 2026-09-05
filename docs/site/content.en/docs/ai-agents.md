---
title: For AI Agents
weight: 4
---

# For AI Agents

If you are a coding agent helping someone integrate speed, or being
pointed at this site as context, this page is for you. speed's own
documentation standard treats agents and humans as first-class readers
equally (see
[docs/internal/13](https://github.com/vislake/speed/blob/main/docs/internal/13-documentation-standards.md),
Chinese-language design rationale) — this page and
[llms.txt](/llms.txt) at the site root are this site's own answer to
that.

## Read this first, in this order

1. **[Root `AGENTS.md`](https://github.com/vislake/speed/blob/main/AGENTS.md)**
   — orientation for any AI coding tool: top-level shape, the module
   dependency direction, the rules that will burn you fastest, and
   where everything else lives. Written to be read start to finish in a
   couple of minutes.
2. **The target module's own `AGENTS.md`** (`go/<name>/AGENTS.md` or
   `web/packages/<name>/AGENTS.md`) — module-specific discipline, file
   layout, known limitations, testing setup. See the
   [module index](../modules/) for the full list with direct links.
3. **[Root `CLAUDE.md`](https://github.com/vislake/speed/blob/main/CLAUDE.md)'s
   *Repository Status* section** — see below, this is the one you must
   not let go stale in your own head.

## The single source of truth for "is this actually implemented"

> [!WARNING]
> Root `CLAUDE.md`'s **Repository Status** section is the single,
> authoritative, current answer to "what genuinely runs and passes in CI
> today." It names, per module, exactly what is implemented, what CI
> workflow proves it (and on what trigger — every PR, a
> `full-ci`-labeled PR, a manual dispatch), and what is still a
> placeholder stub. This static site cannot update itself as fast as that
> section changes — **do not treat anything on this site as a substitute
> for reading it**, and do not trust a status claim (including this
> site's own [Status](../status/) page) that you have not cross-checked
> against it or against the repository itself.

The practical rule that section itself states and this page repeats: a
module directory existing is not evidence it has real code behind it —
some are still a `go.mod` plus a one-line `doc.go` plus an `AGENTS.md`
pointing at a design doc. Check for more than a stub before relying on
one.

## Architecture rules that most often matter

### Module dependency direction

Dependencies flow strictly bottom-up:

```
pkgcore -> dbkit / observability / ratelimit -> tenancy -> config / jobs -> storage / notification / pki
        -> authn / rbac / org / metering -> billing / ai-gateway / sharing / integration
        -> compliance -> admin
```

This is a coarse ordering, not the full edge list —
[docs/internal/01-architecture.md](https://github.com/vislake/speed/blob/main/docs/internal/01-architecture.md)'s
own diagram is the authority for exactly which module imports which.
The two rules that catch most first-time mistakes: `rbac` must never
import `authn` (authorization only ever sees
`Subject{TenantID, UserID}`, assembled by the authenticating side), and
a module must never import another business module's struct for a
database relation — cross-module relations are ID references plus
domain events (`org` subscribes to `authn`'s `user.created` event by
name and JSON-shaped payload probe; it never imports `authn.User`).

### API contract: spec-first, non-negotiable order

Edit `api/openapi.yaml` → run `task api:gen` → the resulting compilation
failures reveal every handler to fix → implement → update the frontend
→ commit everything together. The generated Go server interface
participates in compilation, so drift between spec and implementation
cannot compile. The frontend side mirrors this: hand-written
`fetch`/`axios` calls are permitted only inside `@speed/api-client`;
every other package calls the generated `@speed/api-sdk` hooks, never
HTTP directly.

### The four data domains

Every table is classified before it is designed, not after:

| Domain | Definition | `TenantScoped`? | Example |
|---|---|---|---|
| Tenant data | Belongs to one tenant, never visible across tenants | Yes | org nodes, memberships, subscriptions, media, business data |
| Identity data | Belongs to a natural person, who may belong to several tenants | No | `users`, `user_identities`, `sessions`, login logs |
| Platform data | Globally shared, tenants read only | No | platform-wide Plan definitions, social login provider config, system config |
| Link data | Bridges identity and tenant | Yes (by `tenant_id`) | `memberships` |

The rule this table exists to enforce: `users` is deliberately **not**
tenant-scoped (a person can belong to several tenants, and social
sign-in succeeds before any tenant exists), and platform-wide
definitions like a billing Plan must stay visible to every tenant's
fallback lookup. Getting this classification wrong is, per this
codebase's own experience, the earliest place a multi-tenant
implementation gets stuck.

### Deployment mode vs. implementation composition

Two orthogonal axes, and conflating them is the design error this
codebase's own docs record as a mistake it used to make:

- **Deployment mode** — how many replicas this runs as, and therefore
  which implementations are *permissible*.
- **Implementation composition** — which implementation each
  infrastructure seam (`EventBus`, `KVStore`, `Mailer`, `ObjectStore`)
  actually uses.

The deployment mode does not select an implementation — it only
constrains one. Each implementation declares capabilities
(`MultiReplicaSafe`, `SurvivesRestart`, `Stateless`); each deployment
mode declares what it requires; assembly fails at startup, naming the
seam and the implementation, when the composition cannot satisfy the
declared mode. A single-process deployment talking to real PostgreSQL,
real Stripe and real SMTP is the ordinary shape of a small-customer
production install, not a misuse — the constraint runs one direction
only. Business code must never branch on the mode
(`if mode == "standalone"` is a code-review rejection, not a style nit)
— mode differences belong exclusively to kernel wiring.

## Other rules worth knowing before you write code

- Tenant-owned repositories must embed `dbkit.Repository[T]` — never
  hold a raw `*gorm.DB` and hand-write `WHERE tenant_id = ?`, and never
  accept a caller-supplied `tenant_id` at the API layer (it comes from
  the access-token claims only).
- Workers do not inherit tenant context — rebuild it explicitly
  (`pkgcore.WithTenant(ctx, job.TenantID)`) or the Repository fails
  closed.
- Notifications are event-driven: business modules publish domain
  events, `notification` subscribes. The sole exception is synchronous
  verification codes. External (non-user) recipients require consent
  verification before anything is sent.
- Every bug fix ships with a test that reproduces it (failing before
  the fix, passing after) — this repository treats a missing regression
  test as an incomplete fix, not an optional nicety.

The full, enforceable list — with which parts are code-review-only
versus genuinely CI-enforced today — is root `CLAUDE.md`'s
*Architecture Discipline* section.

## Machine-readable entry point

[/llms.txt](/llms.txt) at this site's root lists the same pages plus the
repository files above in the [llms.txt](https://llmstxt.org/)
convention, for a crawler or agent fetching this domain directly.
