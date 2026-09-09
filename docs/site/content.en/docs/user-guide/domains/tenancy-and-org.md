---
title: Tenancy and organizations
weight: 2
description: How a request gets its tenant, and how to shape your product's organization tree, memberships and invitations with the org module.
---

# Tenancy and organizations

Every request in a speed-based product runs as *some* tenant — that is
the isolation model, and it is guarded three ways (a GORM plugin that
injects the tenant filter, the mandatory `dbkit.Repository[T]` base,
and PostgreSQL row-level security in distributed deployments). The
`tenancy` module resolves which tenant a request belongs to; the `org`
module gives each tenant its organization tree, the memberships bound
to it, and the invitations that create them.

```mermaid
flowchart TD
    Req[Incoming request] --> Res[tenancy resolver\ncustom domain, then subdomain,\nthen platform default]
    Res -->|tenant id| MW[tenancy.Middleware]
    MW -->|tenant context| H[Your handler]
    H --> R[dbkit.Repository[T] queries\nfiltered to ctx tenant]
    Org[org module] -->|nodes, memberships, invitations| ODB[(tenant-scoped rows)]
    MW -.->|allowlisted pre-auth paths skip| Pub[login page, public config]
```

The resolver is a host-supplied function: `tenancy.NewDomainResolver`
maps a request to a tenant by host, and the reference app resolves
custom domain first, subdomain second, platform defaults last — a
resolution that fails still serves platform defaults with a 200 for the
login-page endpoints, never an error. Where a request's tenant comes
from is the framework's job: your handler reads it out of the context
with `pkgcore.TenantFromContext(ctx)` and never accepts one from a
header, parameter or body.

## Minimal integration steps

1. **Mount the middleware.** `tenancy.Middleware(resolver, opts...)`
   wraps your mux; pre-auth paths (login page, `/api/config/public`)
   go on the allowlist, everything else fails closed when the tenant
   cannot be resolved.
2. **Order it after authentication.** In the composed chain the
   tenant layer sits downstream of `authn.Middleware` and turns a
   verified principal into tenant context — see the identity domain
   page for the full order.
3. **Wire the org module.** `org.NewModule(db, opts...)` needs its two
   required wirings at boot (the email blind-indexer and the
   invitation-link builder — `WithInvitationEmailDisabled` exists for
   hosts that send no email); `Tree()`, `Members()` and `Invitations()`
   are the three runtimes.
4. **Shape the tree.** Create nodes under a parent; every node carries
   a materialized path and depth. A move updates every descendant's
   path in one operation — subscribers of `org.node.moved` (rbac's
   subtree grants among them) converge on the event.
5. **Add people by invitation.** Invitations are the tenant's own
   flow: the raw token is never stored (only its hash), the invitee's
   address is encrypted at rest under a blind index, and delivery is
   rate limited per tenant and per recipient. Accepting creates the
   membership; members can then be listed subtree-scoped or removed.
6. **Prove isolation in your own module.** Every repository of
   tenant-owned data must run `tenancytest.AssertIsolated` — the suite
   that would catch a missing filter. Identity and platform tables run
   `AssertNotTenantScoped` instead.

## Boundaries worth knowing

- `users` are deliberately **not** tenant-scoped: a person can belong
  to several tenants; `memberships` is the link table. Classify every
  table into one of the four data domains (tenant / identity /
  platform / link) before designing it.
- The one legitimate cross-tenant path is the audited
  `WithSystemContext` escape hatch, and its use is restricted to the
  platform's own widening purposes (admin, compliance, jobs, authn) —
  business code never widens.
- Soft-deleted nodes keep their names out of the way of a deleted
  sibling: unique indexes are partial (`WHERE deleted_at IS NULL`), so
  a deleted sibling's name or a removed member's seat is reusable.

## Next steps

See the `tenancy` and `org` module pages in the module reference for
the full API; the data-and-config domain page covers how tenant-scoped
models are declared and migrated.

## Source

- [tenancy AGENTS.md](https://github.com/vislake/speed/blob/main/go/tenancy/AGENTS.md)
- [org AGENTS.md](https://github.com/vislake/speed/blob/main/go/org/AGENTS.md)
- [dbkit AGENTS.md](https://github.com/vislake/speed/blob/main/go/dbkit/AGENTS.md)
