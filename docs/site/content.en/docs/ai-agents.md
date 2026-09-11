---
title: For AI Agents
weight: 4
---

# For AI Agents

If you are a coding agent helping someone integrate speed — or being
pointed at this site as context — this page is for you. This
documentation site treats agents and humans as first-class readers
equally; this page and [/llms.txt](/llms.txt) at the site root are its
answer to that.

## Read this first

1. **[Root `AGENTS.md`](https://github.com/vislake/speed/blob/main/AGENTS.md)**
   — orientation for any AI coding tool: the top-level shape, the
   module dependency direction, and the rules that most often burn
   first-time integrations. Written to be read start to finish in a
   couple of minutes.
2. **Choose your side of this site, then read its section:**
   - **Integrating speed into a product?** The
     [user guides](/docs/user-guide/) walk each product domain end to
     end; for the module you are wiring, read its page in the
     [module reference](/docs/user-guide/modules/) — what it is for,
     when to choose it, how to wire it, examples.
   - **Extending or debugging speed itself?** The
     [developer docs](/docs/developer-docs/) explain the architecture,
     the design principles, and a per-module design deep dive (with
     the rationale each module was shaped by). The
     [architecture](/docs/developer-docs/architecture/) page carries
     the module dependency graph.
3. **The module's own `AGENTS.md`** (`go/<name>/AGENTS.md` or
   `web/packages/<name>/AGENTS.md`) — module-specific discipline,
   wiring requirements, known limitations, testing setup. Each module
   page on this site links the module's `AGENTS.md` from its Source
   section; treat it as the authoritative description of that module.

## Architecture rules that most often matter

### Module dependency direction

Dependencies flow strictly bottom-up:

```
pkgcore -> dbkit / observability / ratelimit -> tenancy -> config / jobs -> storage / notification / pki
        -> authn / rbac / org / metering -> billing / ai-gateway / sharing / integration
        -> compliance -> admin
```

This is a coarse ordering — the [architecture](/docs/developer-docs/architecture/)
page carries the full picture. The two rules that catch most
first-time mistakes: `rbac` must never import `authn` (authorization
only ever sees `Subject{TenantID, UserID}`, assembled by the
authenticating side), and a module must never import another business
module's struct for a database relation — cross-module relations are
ID references plus domain events (`org` subscribes to `authn`'s
`user.created` event by name and JSON-shaped payload probe; it never
imports `authn.User`).

### API contract: spec-first, non-negotiable order

Edit `api/openapi.yaml` → run `task api:gen` → the resulting
compilation failures reveal every handler to fix → implement → update
the frontend → commit everything together. The generated Go server
interface participates in compilation, so drift between spec and
implementation cannot compile. The frontend side mirrors this:
hand-written `fetch`/`axios` calls are permitted only inside
`@speed/api-client`; every other package calls the generated
`@speed/api-sdk` hooks, never HTTP directly.

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
codebase's own history records as a mistake it used to make:

- **Deployment mode** — how many replicas this runs as, and therefore
  which implementations are *permissible*.
- **Implementation composition** — which implementation each
  infrastructure module (`EventBus`, `KVStore`, `Mailer`, `ObjectStore`)
  actually uses.

The deployment mode does not select an implementation — it only
constrains one. Each implementation declares capabilities
(`MultiReplicaSafe`, `SurvivesRestart`, `Stateless`); each deployment
mode declares what it requires; assembly fails at startup, naming the
component, the missing capability bits and the mode, when the
composition cannot satisfy the declared mode. A single-process
deployment talking to real PostgreSQL,
real Stripe and real SMTP is the ordinary shape of a small-customer
production install, not a misuse — the constraint runs one direction
only. Business code must never branch on the mode
(`if mode == "standalone"` is a code-review rejection, not a style nit)
— mode differences belong exclusively to kernel wiring.

## Rules worth knowing before you write code

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
  the fix, passing after).

The enforceable discipline list lives in the
[developer docs](/docs/developer-docs/design-principles/), and each
module's `AGENTS.md` states the module-specific rules.

## Machine-readable entry point

[/llms.txt](/llms.txt) at this site's root lists every section and page
in the [llms.txt](https://llmstxt.org/) convention, for a crawler or
agent fetching this domain directly.
