---
title: rbac
weight: 2
description: "Role-based access control — whether a Subject may perform an action on a resource, and over which slice of the organization tree: deny by default, exact resource:action grants, cached decisions invalidated by events."
---

# rbac

rbac is speed's role-based access-control engine: it decides whether a
subject may perform an action on a resource, and over which slice of a
tenant's organization tree. Its defining property is what it does
**not** depend on — it never imports `authn`, `org` or `config`.
Authorization knows one thing about identity, `Subject{TenantID,
UserID}`, assembled by whoever authenticates the request.

## What it is for

Three tenant-scoped tables — roles, the permissions each role grants,
and the bindings that give a user a role — plus a **frozen in-memory
catalog** of every `resource:action` string the host's modules
declared. Evaluation is subject → bindings → role → permissions,
narrowed by a materialized-path prefix test for subtree-scoped
bindings.

- **Deny by default.** No matching grant is `(false, nil)` — a denial,
  never an error. Matching is exact: no wildcards; `notes:read` does
  not imply `notes:write`. An *undeclared* permission denies at check
  time and is rejected at grant time (`ErrUnknownPermission`).
- **Two questions, deliberately separate.** `Can` answers the coarse
  gate — "holds it anywhere in its own tenant". `DataScope` answers
  the row filter — "over which slice of the tree" — which a handler
  returning tenant rows must apply. An unresolvable node-scoped
  binding narrows, never widens: `Can` may stay true while
  `DataScope` denies.
- **Node paths are resolved, never stored.** A binding stores the
  node's id; the host's `SubtreeResolver` resolves the path at
  decision time, so a move changes scope immediately.
- **Decisions are cached per subject**, invalidated by the module's
  own events, so a revoke on one replica converges the others through
  the bus; the anti-loss TTL (30 s default) bounds a lost event.
- **Built-in roles** — `owner` (every declared permission), `admin`
  (everything except `rbac:manage`), `member` (nothing) — their
  permission sets derived from the frozen catalog, never a literal
  list; `EnsureBuiltinRoles` reconciles them at boot.

It is natively implemented over `dbkit` rather than with Casbin — a
recorded deviation: `casbin_rule` has no `tenant_id` column, which
would opt the most security-critical table out of all three isolation
layers at once. It is **not** authentication, the org tree or
messaging, and mounts no HTTP routes (`Module.OpenAPISpec()` returns
nil): role management is served by the operations-console surface, and
rbac does not authorize its own writes — `rbac:read`/`rbac:manage`
are the caller's vocabulary for gating role administration.

## When to choose it

When your modules declare permissions and routes must be gated on
them: the coarse per-operation check, a row-level "which rows may they
see" filter, platform grants on the `"system"` pseudo-tenant — an
ordinary tenant id to every layer, never a wildcard into customer
data. A product without an organization tree still uses it
tenant-wide: `SubtreeResolver` is optional, and a missing resolver
denies a node-scoped grant rather than widening it. Only a product
that gates nothing can skip it.

## Wiring and minimal use

rbac is two-phase because modules register in bootstrap order: a
permission snapshot taken during `Register` would be partial.

```go
rbacMod := rbac.NewModule(db) // options: WithSubtreeResolver, WithCacheTTL, WithQueue
// rbacMod joins the Kernel.Bootstrap module set; after Bootstrap
// returns, exactly once:
az, err := rbacMod.Attach(reg) // freezes every module's declared permissions
if err != nil {
    return err // a second Attach fails: ErrAlreadyAttached
}
```

The returned `*Service` implements the `Authorizer` interface every
consumer programs against. Seed built-in roles per tenant at boot
(`EnsureBuiltinRoles(ctx)`, idempotent), then gate routes:

```go
mux.Handle("/api/v1/notes",
    rbac.RequirePermission(az, "notes:read")(notesHandler),
)
```

`RequirePermission` (and `RequirePermissionFunc`, for a permission
derived from the request) is the chain's post-authentication gate. It
fails closed and indistinguishably: no usable
subject, an unparseable permission and a plain denial all answer
`403 rbac.permission_denied`; only a check that could not be
*performed* differs (`500 rbac.storage_error`). A host that
authenticates differently reaches the gate through its
`WithSubjectResolver` option. Grants are service calls:
`AssignRole(ctx, sub, role, scope)` is idempotent, `RevokeRole`
strict, and an empty `Scope` is tenant-wide.

Mounted routes declare their decisions once, in one table: `rbac.GuardRoutes`
takes the routes a host's modules registered plus one `rbac.RouteRule` per
route -- each either public or naming the permission it requires, with
optional per-route subject resolver, exemption (a request the handler gates
itself) and inside-gate layer -- and refuses to serve a route the table does
not name, so a route added later cannot be served undecided.
`rbac.SplitPermission` (the gate's own splitter, cutting at the last
separator, so a three-segment `integration:apikey:read` works) and
`rbac.WriteAuthzError` (the gate's refusal envelope) are exported for a host
composing its own gate glue.

## Core concepts and API essentials

- **Every method takes the `Subject` explicitly** — never from
  context — and the reads use the subject's own tenant, so an ambient
  context cannot redirect the lookup.
- **Grant lifecycle.** `DefineRole` creates a custom role
  (create-only); `RestoreRole` undoes a revoke's mark-delete —
  bindings implement `dbkit.SoftDeletable`, and the partial unique
  index keeps a revoked scope reusable.
- **The decision cache** is keyed `(tenant, user)`, holding grants
  already flattened through roles; assign/revoke/restore events drop
  the one affected subject, role changes drop the whole tenant.
- **Scoped by the tree, learned without importing it.** Bindings are
  reaped when `org` removes a member or deletes a node and reinstated
  when org restores them — events subscribed by name, never declared;
  queue-backed through `WithQueue` when the host has one.
- **Every role-management write is audited** under
  `rbac.role.define`/`assign`/`revoke` with the acting operator —
  and, under impersonation, the dual-identity pair.

## Boundaries and pitfalls

- Do not import `authn` or `org` to learn facts about a user or a
  node — the seams exist so the engine stays reusable by any host
  that authenticates differently.
- No wildcard grammar, no role-to-role inheritance, no editing a role
  after creation; the cache TTL is an option, deliberately not a
  dynamic config item.
- A revoke is immediate on this replica and converges the others by
  event; the TTL only bounds a lost event.
- Coded errors: see the [error code
  index](/docs/user-guide/error-codes/#rbac).

## Source

- [go/rbac/AGENTS.md](https://github.com/vislake/speed/blob/main/go/rbac/AGENTS.md) — the authoritative document (decision surface, cache, seams, rules)
- Related: the domain guide [Identity and access](/docs/user-guide/domains/identity-access/), and the group pages [authn](/docs/user-guide/modules/identity/authn/) and [org](/docs/user-guide/modules/identity/org/)

