---
title: admin
description: "The platform-staff operations console: tenant ledger and suspension, impersonation, cross-tenant search, audit query and export, role management and usage dashboards — over capabilities every other module already provides."
weight: 5
---

# admin

admin is the operations-console backend: the platform-staff-facing
operations surface over capabilities that already live in every other
module. It sits at the very top of the module dependency graph and is
the one module explicitly permitted to import the concrete packages of
every module below it directly — it is the operator's seat, composed
from `authn`, `org`, `rbac`, `tenancy`, `compliance`, `notification`,
`metering` and `billing`, never a parallel implementation of any of
them.

## What it is for

Seven surfaces, each a thin, real composition over a lower module:

- **Tenant ledger and suspension.** `admin_tenants` (platform data)
  records which tenants the platform believes exist — populated from
  `org`'s `org.node.created` event plus manual CRUD — an operator
  convenience, never the authoritative source. PATCHing a tenant's
  status to `suspended` has real teeth: `TenantService` implements
  `tenancy.TenantStatusResolver` structurally, and once a host wires it
  into its own `tenancy.Middleware`, requests touching the suspended
  tenant answer `tenancy.tenant_suspended` on the very next call, on
  every non-allowlisted route.
- **Impersonation.** A short-lived (30 minutes), explicitly revocable
  `ImpersonationGrant` — never a real token minted for the target
  user. `ImpersonationMiddleware` slots between `authn.Middleware` and
  `tenancy.Middleware` and substitutes the target's identity for the
  rest of one request while the administrator's own still-verified
  token rides along; every audit record from that request carries the
  dual identity (`Actor` = impersonated user, `OnBehalfOf` = the
  administrator), and the impersonated user receives a mandatory,
  non-unsubscribable security notification whose copy leaks nothing
  about the operator.
- **Cross-tenant user search.** `authn.Service.SearchUsers` for the
  identity lookup, `org.MemberService.Get` once per candidate tenant
  for membership — every cross-tenant read under the audited
  `tenancy.WithSystemContext` wrapper naming the operator.
- **Audit query and export.** A thin HTTP shell over
  `compliance.AuditQuery` (with the `onBehalfOf` filter dimension),
  plus an asynchronous export leg: enqueue one `jobs` task, the worker
  runs `compliance.ExportService.Export`, and the completed export is
  delivered as a `go/sharing` single-view link — whose one-time token
  deliberately never lands in the persisted job record.
- **Role management.** A thin wrapper over `rbac.Service`
  (`DefineRole`/`AssignRole`/`RevokeRole`/`RestoreRole`) wired after
  bootstrap through `AttachRBAC`; tenant-naming writes refuse the
  `rbac.SystemDomain` pseudo-tenant outright, so a caller holding only
  `admin:roles_manage` cannot delegate platform-operator authority to
  itself.
- **Usage/billing dashboard.** `GET /api/v1/admin/usage-summary` —
  per-tenant stitch of metering summaries and billing balances and
  subscriptions, with no table of its own.
- **Send-record search.** Notification delivery records, filtered per
  tenant or across the ledger.

The HTTP surface is the module's own fragment under `/api/v1/admin`;
every route is gated by the host on an `rbac.SystemDomain` permission
(`admin:impersonate`, `admin:audit_read`, ...) — the module performs no
authorization of its own, and its routes never sit behind ordinary
tenant resolution: they are about tenants, not scoped to one.

## When to choose it

You operate the platform, not just a tenant: staff need a ledger of
tenants, the power to suspend one, to see and act as a user under full
audit, to search across tenants, to read and export the audit trail,
to manage roles, and to glance at usage across tenants. Every surface
is optional wiring — admin composes what you give it and refuses
bootstrap loudly for what its declared surface requires.

## Wiring it in

```go
a := admin.NewModule(db,
    admin.WithAuthn(authnModule),           // mandatory: each missing
    admin.WithOrg(orgModule),               //   option fails Bootstrap
    admin.WithCompliance(complianceModule), //   with its own named error
    admin.WithNotification(notificationModule),
    admin.WithQueue(queue),                 // audit export must never run in-request
    admin.WithMetering(meteringModule),     // optional: usage-summary dimensions
    admin.WithBilling(billingModule),       // optional, independent of metering
)
// in your the assembly set. Then, after Bootstrap — rbac.Service only
// exists once rbac's own post-Bootstrap Attach has frozen the catalog:
if err := a.AttachRBAC(rbacService); err != nil { /* handle */ }
```

Suspension and impersonation take effect through your own pipeline
construction, not module routes — impersonation sits between `authn`
and `tenancy`, and the tenant-status resolver rides the tenancy layer:

```go
handler := authn.Middleware(verifier)(
    admin.ImpersonationMiddleware(a.Impersonation())(
        tenancy.Middleware(authn.NewPrincipalResolver(),
            tenancy.WithTenantStatusResolver(a.Tenants()),
        )(mux),
    ),
)
```

A request with no `X-Admin-Impersonation` header passes through
unmodified; a valid grant substitutes the target's `Principal` for the
rest of that one request and stamps the dual identity onto its context.

## Core concepts and API surface

- **Module accessors:** `Tenants()`, `Impersonation()`, `Search()`,
  `Roles()`, `Usage()`, `Export()` — plus the post-Bootstrap
  `AttachRBAC(svc)`. `Roles()` and `Impersonation()` fail closed with
  `ErrRBACServiceRequired` until `AttachRBAC` runs, so no grant is born
  before the machinery that must be able to cut it off exists.
- **A live grant ends the moment its administrator's permission is
  revoked** — `ImpersonationService` subscribes to rbac's revocation
  events and re-verifies `Can` before ending affected grants; the
  request path itself carries no per-request permission check.
- **Two platform-data tables** (`admin_tenants`,
  `admin_impersonation_grants`), never tenant-scoped, with guarded
  compare-and-set writes: a suspend racing a resume or two racing
  grant-ends — the second is refused, never a silent stale overwrite.
- **The usage-summary GET performs one documented write** — billing's
  materialize-on-first-read contract creates a zero balance row per
  ledger tenant that has none; nothing on any other table.
- **Coded errors** — `admin.roles_system_domain_forbidden` among them
  — are indexed in the [error code index](/docs/user-guide/error-codes/#admin).

## Limitations and links

- The ledger is deliberately non-authoritative: no tenant-creation
  enforcement rides on it, and an absent row reads as `active`, never
  a false suspension.
- No dual-approval workflows, and no rate limiting or anomaly detection
  on admin actions (impersonation included) — recorded exclusions.
- The impersonation TTL is a fixed 30-minute constant; audit-query and
  cross-tenant send-record pagination inherit the Go-side slicing of
  the compliance query underneath.
- `RestoreRole`/`EnsureBuiltinRoles` exist at the `RoleService` level
  with no HTTP route.
- admin has no Docker-backed integration tier of its own; the composed
  wiring is proven by its unit suite over real downstream modules and
  by the reference app's end-to-end flow tests.

### Source

- [go/admin/AGENTS.md](https://github.com/vislake/speed/blob/main/go/admin/AGENTS.md) — the authoritative document (surfaces, wiring contract, impersonation pipeline, limitations)
- Related pages: [compliance](/docs/user-guide/modules/capabilities/compliance/), [tenancy](/docs/user-guide/modules/core/tenancy/), [notification](/docs/user-guide/modules/services/notification/), [metering](/docs/user-guide/modules/services/metering/), [billing](/docs/user-guide/modules/capabilities/billing/)
