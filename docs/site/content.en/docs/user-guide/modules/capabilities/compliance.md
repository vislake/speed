---
title: compliance
description: "Governance: retention-window sweeping, right-to-erasure orchestration, export gathering-and-delivery and read-only audit querying — an orchestrator that invents no deletion or audit mechanism of its own."
weight: 4
---

# compliance

compliance is speed's governance layer: retention-window sweeping,
right-to-erasure orchestration, data-export gathering *and* delivery,
and read-only audit querying. It invents no mechanism of its own —
every physical delete it causes runs through a business module's own
`dbkit.Repository[T].HardDelete` (tenant-bound and system-context-gated
in `go/dbkit`), and every audit record it produces goes through
`go/dbkit/audit`'s `Emit`. It is a pure Go-level API: no HTTP surface,
no table of its own.

## What it is for

Three orchestrations plus one query API, all built over the
`pkgcore.RetentionParticipant` registrations business modules make on
the kernel's `reg.Retention` seat:

- **`RetentionService`** — `SweepTenant(tenant)` hard-deletes each
  registered participant's soft-deleted rows past the tenant's
  retention window; `EnqueueRetentionSweep` (a `jobs` task, window-scoped
  idempotency key) is the schedule point a host drives; `SweepAllTenants`
  covers every tenant from a host-supplied `TenantLister`.
- **`ErasureService.Erase(subject)`** — immediate, retention-window-
  bypassing erasure of one subject's rows: it refuses a no-tenant
  context and a `SubjectRef` whose tenant differs from the context
  tenant, before any participant runs and before any system context is
  entered. A re-run converges: participants report `(0, nil)` for work
  they already completed.
- **`ExportService.Export(tenant)`** — gathers every participant's
  `Export` data into one storable `ExportManifest`, stores it through
  the `ObjectStore` seam, and delivers it by minting a 24-hour,
  single-view, no-password `go/sharing` share (`Sensitive: true`),
  returning the object key, manifest and the share's token.
- **`AuditQuery`** — tenant-scoped `Query` (or system-context-gated
  `QueryAcrossTenants`), `Get` by id: read-only filtering over
  `dbkit/audit`'s existing table by actor, on-behalf-of administrator,
  resource, action, time range and result; `RenderAuditReport` turns
  any event slice into CSV or JSON.

What it is **not**: no tables and no migrations (the durable record is
`audit_events` plus each participant's own already-migrated table); no
HTTP surface (its bytes are what a future handler would serve); no hash
chain and no time-partitioned archival; no schedule of its own — a
retention sweep runs only when a host enqueues or calls one.

## When to choose it

Your product stores data a regulator or a contract can ask about: rows
that soft-delete and must eventually be physically reaped, subjects
who may demand erasure, tenants who may demand their data out, and an
audit trail that must be queryable. compliance is the orchestrator —
each participating module still owns its rows, and the participants
that do not exist are the erasure boundary, stated precisely in the
module's `AGENTS.md`. If you only need the audit trail itself (not the
governance operations), `dbkit/audit`'s repository is the lower-level
seat; compliance adds the query and the orchestration on top.

## Wiring it in

```go
c := compliance.NewModule(auditRepo, // the same *audit.Repository your audit wiring uses
    compliance.WithQueue(queue),                 // arms the retention-sweep task
    compliance.WithSharing(sharingModule.Service()), // arms export delivery
    // optional: compliance.WithTenantLister(lister), WithConfigService(cfg)
)
// in your Kernel.Bootstrap set. After Bootstrap, register the business
// modules whose rows the orchestrations may touch — the reference app's
// notes module registers exactly this participant shape, its callbacks
// backed by its own repository's HardDelete and read methods:
if err := reg.Retention.Add(
    notes.NewRetentionParticipant(notes.NewRepository(db)),
); err != nil { /* handle */ }

// a host schedule point (periodic tick, per tenant):
if err := c.Retention().EnqueueRetentionSweep(ctx); err != nil { /* handle */ }

// right to erasure, from an operator or compliance workflow:
_, err := c.Erasure().Erase(ctx, pkgcore.SubjectRef{
    TenantID: tenantID, SubjectID: userID,
}, requestedBy)
```

`RetentionParticipant` (declared in `pkgcore`, registered through
`Registry.Retention`) is a `Name` plus `Sweep`/`Erase`/`Export`
callbacks; `Sweep` and `Erase` are mandatory at registration, `Export`
optional, and every callback is expected to call the participant's own
`dbkit.Repository[T]` methods — compliance never imports or queries a
business module's table. The reference app's notes module is the real
consumer; the module's own `AGENTS.md` enumerates the tenant-scoped
owners that register no participant yet.

## Core concepts and API surface

- **Partial failure is first-class, never silent.** One participant's
  failing callback never stops the others; the failure is recorded
  per-participant in the returned `Errors` maps and in the audit
  record as a classification — never the error text, which can carry a
  subject's identifier and would be carved into the one table nothing
  can delete from. The call still audits itself and returns a coded
  partial-failure error (`compliance.sweep_partial_failure`, ...)
  alongside the full result. Recovery is retry, not rollback.
- **`Erase` never erases across tenants** — the context gate plus the
  tenant-bound `HardDelete` underneath make the cross-tenant
  non-erasure property provable, and each participant's reported count
  survives its own error so the audit never understates what an
  irreversible operation destroyed.
- **An export is one whole tenant, never one subject**, delivered as a
  credentialed one-time handoff: single-view, no password, 24-hour
  expiry (tenant-tunable through the export-delivery reader seam,
  clamped to the `go/sharing` ceiling). A failed delivery deletes the
  stored manifest; a delivered one is reaped by the module's own
  `compliance.export_manifests` participant once past the retention
  window. Every export leaves two trails: compliance's request-and-
  delivery record and sharing's sensitive-create record.
- **`AuditQuery` filters in Go, not SQL** — deliberately, over
  `ListByTenant`'s rows, with deterministic ordering (occurred-at then
  id, descending); the honest cost is O(all rows for the tenant) per
  query. The `OnBehalfOf` filter dimension is what makes impersonation
  accountability queryable.
- **Coded errors** — `compliance.erasure_partial_failure`,
  `compliance.export_delivery_failed`, `compliance.audit_record_failed`,
  `compliance.erasure_tenant_mismatch` and the rest — are indexed in
  the [error code index](/docs/user-guide/error-codes/#compliance).

## Limitations and links

- The retention sweep has no schedule point in the module: every host
  that wires compliance adds its own periodic enqueue (the reference
  app's scheduler is the shipped shape) or soft-deleted rows are
  retained until someone does.
- `SweepAllTenants` needs a host-supplied `TenantLister` — compliance
  sits above `org` and will not import it; there is no built-in tenant
  directory.
- One `Erase` removes exactly the rows of the participants registered
  when it runs — never a subject's tenant data as a whole. Owners that
  register no participant (org, rbac, storage, notification, billing,
  ...) keep their rows; the boundary closes only by a registration.
- No PostgreSQL integration tier for compliance itself: its logic is
  dialect-insensitive by design (participants' repositories and
  `dbkit/audit` carry their own dual-dialect proofs), and the
  append-only trigger enforcement on `audit_events` lives in
  `go/dbkit/audit`, proven against a real server there.

### Source

- [go/compliance/AGENTS.md](https://github.com/vislake/speed/blob/main/go/compliance/AGENTS.md) — the authoritative document (participant contract, partial-failure semantics, export delivery, limitations)
- Related pages: [sharing](/docs/user-guide/modules/capabilities/sharing/), [admin](/docs/user-guide/modules/capabilities/admin/), [dbkit](/docs/user-guide/modules/core/dbkit/) (audit trail and hard delete)
