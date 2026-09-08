# compliance

go/compliance is the governance layer over retention-window sweeping,
right-to-erasure orchestration, data-export gathering-and-delivery and
read-only audit querying. It invents no new deletion mechanism: every
physical delete it ever causes runs through a business module's own
`dbkit.Repository[T].HardDelete` (already tenant-bound and
system-context-gated), and every audit record it produces goes through
`dbkit/audit.Emit`. This file is the module-level discipline that ships
with `go/compliance` to consuming projects; the repository-wide rules are
the root `CLAUDE.md` plus `.claude/skills/backend-coding-standards`.

## Scope

- `RetentionService.SweepTenant` (one tenant's retention-window sweep,
  `retention.go`), `SweepAllTenants` (a host-supplied `TenantLister` seam
  covering every tenant from one scheduled task), `EnqueueRetentionSweep`
  plus `retentionSweepHandler` (the `jobs.Queue`/`jobs.Handler` schedule
  point).
- `ErasureService.Erase` (`erasure.go`): immediate,
  retention-window-bypassing erasure for one `pkgcore.SubjectRef`.
- `ExportService.Export` (`export.go`): gathers every participant's
  `Export` data into one `ExportManifest`, stores it through
  `pkgcore.ObjectStore`, and delivers it through a wired `SharingCreator`
  (typically a real `go/sharing.Service`), returning an `*ExportResult`
  carrying the object key, the manifest and the minted `ExportDelivery`
  (share id, one-time token, expiry).
- `AuditQuery` (`audit_query.go`): `Query` (tenant-scoped),
  `QueryAcrossTenants` (system-context-gated, caller-named tenant list),
  `Get` (by id) -- all read-only, filtered in application code by actor,
  on-behalf-of administrator, resource, action, time-range and result.
- `RenderAuditReport` (`report.go`): a pure, dependency-free rendering of
  any `[]audit.AuditEvent` -- typically an `AuditQuery.Query`/
  `QueryAcrossTenants` result, but the function itself neither imports nor
  calls `AuditQuery` -- as `ReportFormatCSV` or `ReportFormatJSON`. It
  does no I/O, no pagination and no tenant-scope enforcement of its own.
- `pkgcore.Module` wiring (`module.go`): two config items
  (`ConfigDefaultRetentionWindow`, `ConfigExportDeliveryExpiry`), four
  permissions, three audit actions, two audited `pkgcore.SystemPurpose`
  values, the retention-sweep job handler, and the `WithQueue`/
  `WithConfigService`/`WithTenantLister`/`WithSharing`/
  `WithExportConfigReader` construction-time options.
- The trigger-based database-level append-only backstop on `audit_events`
  lives in `go/dbkit/audit`'s own migrations (`0002_append_only_
  enforcement.sql`, both dialects), not in this module -- `audit_events`
  is `dbkit/audit`'s own table, and compliance owns no table to attach a
  trigger from (see "Why no table").

## Deliberately not in scope

| Not here | Why |
|---|---|
| Optional hash chain over `audit_events` | No consumer asks for it; it carries a real write-cost tradeoff |
| Time-partitioned archival, cold storage | Not built. Its stated prerequisite (append-only enforcement) is landed, so nothing external blocks it -- it is simply not shipped |
| An HTTP surface / OpenAPI fragment for compliance itself | compliance is a Go-level API, not a service reached over HTTP. `RenderAuditReport`'s bytes are what an HTTP handler would set as a response body, but no such handler exists. Serving a delivered export's actual bytes over HTTP is a separate `go/sharing`-side gap: sharing's `Access` resolves a share to its row, not to bytes |
| An erasure-request or sweep-run log table of its own | The mechanism's durable record is `dbkit/audit`'s existing `audit_events` table plus each participant's own already-migrated table (see "Why no table") |
| A public `ParseAuditReportCSV`/`ParseAuditReportJSON` reader | `report_test.go`'s round-trip parsing exists to verify `RenderAuditReport`'s own correctness; a render direction is the shipped shape, not a full codec. A caller that wants to re-read a report it did not just render would be asking for a new API |

## Why no table

`Module.Migrations()` returns the zero `embed.FS` -- compliance owns no
table of its own. This is a deliberate design choice, weighed against
adding a dedicated erasure-request (or sweep-run) log:

- **The durable record already exists.** Every `SweepTenant` and `Erase`
  call ends in exactly one `dbkit/audit.Emit` call
  (`AuditActionRetentionSweep`/`AuditActionErasureRequest`), carrying the
  per-participant reaped/erased breakdown in `Changes` -- and every
  `Export` call ends in one `AuditActionExportRequest` event of the same
  shape. A participant error is recorded there as a classification (the
  participant's name keyed to a "failed" marker --
  `erasureAuditErrorMarker` on the erasure path, `participantErrorMarker`
  on the sweep and export paths), never the error text itself (see
  "Error text classification" below). That is already an append-only,
  queryable (via `AuditQuery`), tenant-attributed record of every
  orchestration run -- a second, compliance-owned table would duplicate
  it.
- **Retry state does not need its own row.** `pkgcore.RetentionParticipant
  .Erase`'s own contract requires a participant to return `(0, nil)` for a
  subject it has already fully erased (see `erasure.go`'s `Erase` doc
  comment), so re-running `Erase` after a partial failure converges to
  completion by itself.
- **`dbkit.MigrationRegistry.Register` documents this as legal, not
  degraded**: "a module with no subdirectory at all for that dialect is
  treated as declaring zero migrations for it, not as an error"
  (`go/dbkit/migrations.go`). `Module.Migrations` returning `embed.FS{}`
  is the ordinary answer for a module whose whole job is orchestration
  over other modules' already-migrated tables.

## The `pkgcore.Registry.Retention` registrar

`RetentionParticipant` (declared in `go/pkgcore/registry.go`, alongside
`RetentionRegistrar` and the `SubjectRef` type `ErasureService.Erase`
takes) is the one change this module makes to a module below it in the
dependency graph, following the pattern "the `Registry` struct exists so
that adding a new cross-cutting mechanism does not change the `Module`
interface". It mirrors `AuditActionRegistrar`'s shape (a name-uniqueness
map plus an append-only, registration-ordered slice).

A participant is a `Name` plus three callbacks, `Sweep`/`Erase`/`Export`,
each optional except `Sweep` and `Erase` (a participant with neither is
legal but useless; `Export` alone is legal, for a participant that wants
export-gathering but has nothing meaningfully soft-deletable). Every
callback is expected to call the participant's own `dbkit.Repository[T]`
methods -- `Sweep`/`Erase` call `HardDelete`, `Export` calls a read method
-- so compliance's own code never imports, and never directly queries, a
business module's table. `internal/testutil`'s
`FakeNote`/`FakeRepository`/`NewParticipant` is the unit-tier proof that
the contract compiles and works end to end; the real business module is
the reference app's notes participant, registered onto the kernel's
`reg.Retention` seat in `cmd/server/server.go` and proved by
`compliance_flow_test.go` there.

`Retention` is available on a `Registry` built with `pkgcore.NewRegistry`
(the three-argument constructor), not only through `Kernel.Bootstrap` --
unlike `ObjectStore`/`Locales`, which are Bootstrap-only. This is why
this module's own unit tests build a hand-made `Registry` directly for
`RetentionService`/`ErasureService`/`AuditQuery` tests, reserving
`Bootstrap` for `module_test.go`'s own `Register` proofs, where
`ExportService`'s `ObjectStore` dependency genuinely requires it.

## Partial failure across independent transactions

Every one of `SweepTenant`, `Erase` and `Export` calls into N
independently registered participants, each backed by its own
`dbkit.Repository[T]` and, deployment-mode permitting, its own physical
database connection -- there is no way to compose N participants'
`HardDelete` calls into one cross-module transaction (independently
released modules cannot share a commit). All three services therefore
share one policy, applied consistently:

1. One participant's callback failing never stops the others -- every
   registered participant runs, regardless of an earlier failure.
2. The failure is recorded per-participant, keyed by participant `Name`:
   the raw error in the returned in-process results where one exists
   (`SweepResult.Errors`, `ErasureResult.Errors`), and the
   `participantErrorMarker` classification in `ExportManifest.Errors` and
   in every audit record's `Changes["errors"]` entry -- never the error
   text on a surface that is permanent or delivered.
3. The operation still audits itself (`emitSweepAudit`/`emitErasureAudit`/
   `emitExportAudit`), with the full per-participant breakdown in
   `Changes`, whether or not any participant failed.
4. The method returns a non-nil, specifically coded error
   (`ErrSweepPartialFailure`/`ErrErasurePartialFailure`/
   `ErrExportPartialFailure`) alongside the full result whenever any
   participant failed -- never a bare `nil` result and never a silent
   `nil` error that would let a careless caller mistake a partial pass
   for a clean one.
5. Recovery is retry, not rollback: every `Sweep`/`Erase` callback is
   documented to return `(0, nil)` for work it has already completed, so
   calling the same operation again converges the remaining participants
   to completion without re-processing (or re-auditing as a duplicate
   fact) what already succeeded.
6. A participant's reported count survives its own error: a callback that
   failed part-way through has already hard-deleted the rows it reports,
   and that count is recorded (in the result and the audit record)
   whenever the callback reports one, error or not -- an irreversible
   operation's destroyed rows are never understated.

## Error text classification

No error text ever reaches a permanent or delivered surface. The audit
events' changes column is effectively permanent (`dbkit/audit/emit.go`'s
Diff contract: "Anything written here is effectively permanent"), and
participant failure text can carry identifiers and internal details no
audit reader was ever promised; `ExportManifest.Errors` is serialized
into the manifest Export stores and delivers over an unauthenticated,
single-view go/sharing link, whose holder is the export's recipient --
entitled to the export's data, not platform-internal failure text that can
name other subjects, internal object keys or infrastructure details (two
audiences conflated into one). So every `Errors`-family entry this module
writes carries a classification (`erasureAuditErrorMarker`/
`participantErrorMarker`, the value `"failed"`), keyed by participant
name: the record says WHO failed and THAT it failed, never how. The
error text's legitimate homes are the failure-site structured logs
(behind go/observability's redaction layer) and the in-process results
(`SweepResult.Errors`, `ErasureResult.Errors`) and returned errors
(`ErrExportDeliveryFailed`'s wrapped cause). The contract is written into
the seam: `pkgcore.RetentionParticipant`'s type doc comment states it in
full, and the `Sweep` and `Export` field doc comments point participant
authors at it.

## Export: gathering and delivery

`ExportService.Export` gathers every participant's data into one
`ExportManifest`, marshals it to JSON and stores it through
`pkgcore.ObjectStore` under a fresh key, then hands that stored object
off to `go/sharing` for delivery: it mints a short-lived, single-view
share and returns its id and one-time token to the caller in
`ExportResult.Delivery`, rather than stopping at "stored, here is an
internal key".

**Dependency choice: a direct `go/sharing` import, not a structurally-
typed seam.** `go/compliance` sits above `go/sharing` in the module
dependency graph, so an import edge is architecturally sound. What
`Export` does declare as a small interface -- `SharingCreator`
(`export.go`), exactly `sharing.Service.Create`'s own shape -- is not
about avoiding the import (the package already imports `go/sharing` for
its parameter and result types, and `module.go` carries the compile-time
assertion `var _ SharingCreator = (*sharing.Service)(nil)`), but about
keeping `ExportService`'s own unit tests independent of a real
`*sharing.Service`'s `gorm.DB`, migrations and registry wiring.

**Wiring: `Module.WithSharing`, and a call-time refusal.** `SharingCreator`
is not one of the registry's resolved infrastructure seams; a host wires
it directly at `Module` construction time through `WithSharing`,
typically passing a real `sharing.Module`'s own `Service()`. Unlike
`WithQueue`, whose absence is checked once, at `Register`
(`ErrQueueRequired`) -- a registered job handler with no queue can never
run at all -- `WithSharing`'s absence is checked only when `Export` is
actually called (`ErrSharingRequired`), because `RetentionService` and
`ErasureService` have nothing to do with export delivery: a host that
only wants retention sweeping and right-to-erasure, never export, can
still boot `compliance.Module` with no `SharingCreator` wired at all.

**Expiry, view limit and no password.** An export bundles one tenant's
*complete* data -- potentially many subjects' records -- into one
downloadable package, so the delivery link does not default to sharing's
own general-purpose 30-day `defaultShareExpiry`:
`defaultExportDeliveryExpiry` is 24 hours -- long enough for a relayed
delivery link to reach its recipient and be used, without leaving a
leaked or intercepted link usable for weeks. The declared config item,
`ConfigExportDeliveryExpiry` (`compliance.export_delivery_expiry`, `Min`
1 hour, `Max` 72 hours), is tenant-overridable like any config item. A
tenant's configured value is read live through `ExportDeliveryExpiryReader`
(`export.go`) -- the same `(d, ok, err)` shape sharing's own
`TenantConfigReader` uses -- wired through the construction-time
`Module.WithExportConfigReader` option; without one (the default), `Export`
falls back to `defaultExportDeliveryExpiry`. A non-positive reader answer
cannot mint a dead link: `exportDeliveryExpiry` clamps a wired reader's
`d <= 0` to `defaultExportDeliveryExpiry` (the identical guard
`RetentionWindow` applies to a configured retention window), and a
duration longer than sharing's own explicit-expiry ceiling is clamped
down to that ceiling. This interface exists even though compliance
already imports go/config directly elsewhere (`RetentionService.cfg`):
a construction-time Option cannot capture `config.Module.Attach`'s
`*config.Service`, which is only produced strictly after
`Kernel.Bootstrap` returns, so a host wires a lazy adapter over a
later-filled `**config.Service`. The reference app wires such an adapter
(`server.go`'s `complianceConfigReader`) and proves both the configured
and the unconfigured tenant's answers end to end. The created share is
single-view (`MaxViews` = `exportDeliveryMaxViews` = 1) rather than
password-protected: delivery is a one-time credentialed handoff, not an
open link -- a 256-bit bearer token already is that credential, and
`MaxViews=1` means the link is spent the moment it is actually used. A
password was considered and rejected: it needs its own delivery channel
(the caller would have to relay the password to the link's recipient
separately), which is more moving parts than a single-view, high-entropy
token needs to achieve the same property. Every export share is created
with `Sensitive: true`, which independently fires sharing's own
`sharing.share.create_sensitive` audit action -- a second,
sharing-owned audit trail alongside compliance's own.

**Delivery is audited as part of the same `AuditActionExportRequest`
event, not a second compliance-owned action.** Gathering, storing and
delivering an export are one governance operation from the caller's
perspective, so `emitExportAudit`'s `Changes` map carries `share_id` and
`share_expires_at` on a successful delivery (and, on a failed one,
`Result.Success` flips to false with the classification `"delivery
failed"` as `FailureReason`).

**A delivery failure is reported distinctly from a participant gathering
failure.** `Export` returns `ErrExportDeliveryFailed` (never
`ErrExportPartialFailure`) when the already-gathered-and-stored manifest
could not be handed off -- `ExportResult.ObjectKey` and `.Manifest` are
still populated, but `.Delivery` is the zero value. A delivery failure
takes precedence over a participant partial failure in the returned error
code (one `apperr` code cannot represent both at once), but
`ExportManifest.Errors` is unaffected either way. A manifest that could
not be delivered is deleted before `Export` returns: an un-shareable copy
of the tenant's complete data has no legitimate consumer path, and admin
retries must not accumulate one dump per attempt.

Unlike `SweepTenant`/`Erase`, `Export` never enters a system context:
every participant's `Export` callback reads the SAME tenant the caller's
own `ctx` is already scoped to (never bypassing tenant isolation, never
touching another tenant's rows), so there is no escape hatch to audit the
entry of. That scoping is enforced, not assumed: `Export` refuses a `ctx`
carrying no tenant (`pkgcore.ErrNoTenant`) and refuses a `tenant`
argument naming any other tenant than the `ctx` carries
(`ErrExportTenantMismatch`), before anything is gathered, stored or
delivered. The `tenant` argument exists only so a caller that rebuilt
`ctx` from a stored tenant id -- a `jobs.Handler` whose worker rebuilt it
from `job.TenantID` -- can pass that same id through; it can never name a
wider scope.

**Export scope is one whole tenant, never one data subject.** `Export`
gathers every participant's data for the tenant `ctx` carries into one
tenant-level bundle -- a package that may well contain many subjects'
records, not any single subject's data; a manifest is a tenant-wide
bundle, never a single subject's rows. A subject-scoped export --
gathering one subject's data and delivering it to that subject, the
GDPR-shaped "this is your data" delivery -- is NOT built.

**`Erase` enforces the same tenant boundary.** `Erase` refuses a `ctx`
carrying no tenant (`pkgcore.ErrNoTenant`) and refuses a `SubjectRef`
whose `TenantID` differs from the `ctx` tenant
(`ErrErasureTenantMismatch`), both before any participant is called and
before any system context is entered: an irreversible, cross-tenant
destruction must never be reachable from a caller-supplied tenant that
does not echo the ctx tenant back.

**A delivered manifest is reaped once its delivery share has expired
past the tenant's retention window.** `Module.Register` registers the
module's own `compliance.export_manifests` retention participant
(`export_cleanup.go`), whose `Sweep` -- running on the module's own
per-tenant retention sweep -- reaps every stored manifest whose delivery
share's expiry has fallen past the sweep's cutoff. The `pkgcore.ObjectStore`
seam has no listing primitive, so the participant finds its objects by
reading the tenant's own `AuditActionExportRequest` events back from the
append-only audit trail (`Changes.After` carries `object_key` and
`share_expires_at`), probing each candidate with `GetObject` before
deleting so a re-run converges to 0, and confining every deletion to the
swept tenant's own `compliance/exports/<tenant>/` prefix so a malformed
event inside one tenant's trail can never reach another tenant's objects.
The candidate gate is the delivered share's own expiry, deliberately not
the event's `Result.Success` flag: a PARTIAL export is still gathered,
stored and delivered, its event records `Success` false while carrying
the same `object_key` and `share_expires_at` -- it must be reaped the
same way, or every partial export leaves one stored bundle behind
forever. A sweep reads the tenant's whole trail per pass, the same
honest O(rows) cost `AuditQuery` documents.

## `AuditQuery`: filtering runs in application code

`AuditQuery` (`audit_query.go`) adds no method to `audit.Repository` --
it holds only the Repository's existing exported methods
(`ListByTenant`, `Get`) and filters what they return, in Go, via
`QueryFilter.matches` and `filterAndSort`. It therefore cannot add an
Update or a Delete.

**This is an honest, not a hidden, limitation.** `Query` and
`QueryAcrossTenants` fetch every row `ListByTenant` would return for the
tenant(s) named, then filter in memory -- there is no SQL `WHERE actor_id
= ?` pushed down, since `Repository` exposes no such method. For a tenant
with a very large audit trail, this is O(all rows for that tenant) per
call, not O(matching rows). SQL-level filtering would live either on
`dbkit/audit.Repository` or in a compliance-owned type holding its own
`*gorm.DB` -- which `AuditQuery` deliberately does not hold.

`QueryAcrossTenants` composes its "every tenant" answer from one
`ListByTenant` call per caller-named tenant, because `audit.Repository`
has no single "every tenant, all at once" read. The caller supplies the
tenant list -- typically every tenant a `TenantLister` returned -- so the
cost is visible at the call site. The on-behalf-of dimension
(`QueryFilter.OnBehalfOf`) exists because an administrator never appears
as `Actor` on an impersonation-era row (the impersonated user does): an
attribute that can only be written and never queried does not exist for
accountability.

## Formatted audit report export

`RenderAuditReport(events []audit.AuditEvent, format ReportFormat)
([]byte, error)` (`report.go`) is a pure function taking exactly the
`[]audit.AuditEvent` an `AuditQuery.Query`/`QueryAcrossTenants` call
already returns and a `ReportFormat` (`ReportFormatCSV` or
`ReportFormatJSON`; any other value, including the zero value, is
`ErrUnsupportedReportFormat`). It does no I/O, no pagination and no
tenant-scope enforcement of its own -- all three are `AuditQuery`'s job,
upstream of this call -- so it renders exactly the events it is given, in
the order given.

- **JSON** is a direct `encoding/json.Marshal` of the
  `[]audit.AuditEvent` slice, in the identical exported-field shape
  `AuditEvent` already carries everywhere else -- no report-specific
  struct, no custom `MarshalJSON`.
- **CSV** carries an explicit `has_on_behalf_of` column, because CSV
  cannot express nullability on its own: `OnBehalfOf` is a genuinely
  nullable `*pkgcore.Actor` (NULL means "no impersonation", never an
  empty-string sentinel), and three blank CSV cells cannot distinguish
  "no impersonation" from "impersonated by an actor whose three fields
  happen to be empty". Escaping is delegated to `encoding/csv.Writer`,
  which quotes per RFC 4180.
- **CSV cells are protected against spreadsheet formula injection.**
  Report cells carry content whose provenance is the very subjects and
  requests the report records, so a cell beginning with a
  spreadsheet-formula character (=, +, -, @, tab or carriage return:
  OWASP's CSV-injection list) would execute as a formula when the file is
  opened in a spreadsheet. `auditEventCSVRow` single-quote-prefixes
  exactly those cells -- inert text in every mainstream spreadsheet. The
  protection is uniform across every column rather than whitelisted per
  "user-influenced" column (a provenance whitelist is a maintenance
  liability); refusing a hostile cell outright was considered and
  rejected (one attacker-planted field would fail the whole export --
  denial of service over a document's content); and the one honest cost
  is documented: a protected cell does not round-trip byte-identically
  (a machine reader following this documented convention strips the
  prefixing quote exactly once).
- **`OccurredAt` renders as UTC RFC3339Nano** in both formats --
  nanosecond precision round-trips exactly, and UTC removes the ambiguity
  a local offset could introduce.

## Append-only enforcement lives in `go/dbkit/audit`, not here

The database-level backstop refusing any `UPDATE`/`DELETE` against
`audit_events` is not a `go/compliance` change at all: `audit_events` is
`dbkit/audit`'s own table, migrated by `dbkit/audit`'s own
`migrations/{postgres,sqlite}` set, so the new migration
(`0002_append_only_enforcement.sql`, both dialects) and its proof tests
live there, exactly where the table's creation migration already does.
compliance neither owns nor migrates that table and has no mechanism of
its own to add a trigger to.

The mechanism: a `BEFORE UPDATE`/`BEFORE DELETE`
trigger pair on `audit_events`, on both dialects, each unconditionally
raising before a write reaches a row -- `RAISE EXCEPTION` (PostgreSQL,
via a small `plpgsql` trigger function) / `RAISE(ABORT, ...)` (SQLite,
inline in the trigger body). A trigger, not a REVOKE-based restricted
role, is what this codebase can actually guarantee: `dbkit.Open`
establishes exactly one connection/role per `*gorm.DB`, and provisioning
a second, more restricted role would mean this codebase starting to own
role/credential provisioning it has always declined to own even for
PostgreSQL RLS's own role and policy. A trigger enforces the guarantee
against *any* role that connects, with no such provisioning prerequisite
-- INSERT is completely unaffected.

## Known limitations

- **`RetentionService.SweepAllTenants` needs a host-supplied
  `TenantLister`; there is no built-in tenant directory.** compliance
  sits above every business module including `org` (the module that would
  normally own "list every tenant"), and importing `org` from compliance
  would be an upward-pointing dependency. `TenantLister` is a
  structurally-typed, no-import seam, and a host wires a real
  implementation (backed by `org`, or by its own tenant table) when it
  wants `SweepAllTenants`. `EnqueueRetentionSweep`/`SweepTenant` need no
  lister at all: they operate on one tenant a caller already names.
- **The retention sweep has no schedule point in the module.** go/
  compliance runs no timer of its own: soft-deleted rows past their
  retention window are physically reaped only when a host enqueues a
  tenant's sweep (`EnqueueRetentionSweep`) or calls `SweepTenant`
  directly. The reference app schedules one sweep per unique host tenant
  per tick (`cmd/server/periodic_scheduler.go`), with the same cadence
  and worker gate as storage's expiry sweep; every other host that wires
  compliance must add its own schedule point or soft-deleted rows are
  retained indefinitely. The schedule a host adds is bounded by
  `EnqueueRetentionSweep`'s window-scoped idempotency key
  (`retentionSweepIdempotencyKey`, one `retentionSweepWindowSize` window
  per key): the ticks inside one window merge into the window's one job,
  the first tick of every later window sweeps again (at most one sweep
  per tenant per hour), and a dead-lettered sweep poisons only its own
  window. `retention_sweep_window_test.go` pins all three window
  properties against a real `jobs.StandaloneQueue`.
- **One `Erase` run removes exactly the `reg.Retention` participants
  registered when it runs.** In the current reference-app composition
  that is the notes module's participant plus this module's own
  `compliance.export_manifests` cleanup leg (whose callback is the
  explicit `(0, nil)` non-answer -- nothing subject-shaped lives in a
  delivered manifest) -- and is therefore never an erasure of a
  subject's tenant data as a whole. Owners that declare tenant-scoped
  models yet register no participant include org, rbac, storage,
  notification, sharing, integration, billing, metering, ai-gateway,
  authn, pki, and the reference app's own cases and smilesim rows. The
  enumeration ages as owners register; a host whose obligation reaches a
  subject's rows in any of those tables must register the participant
  that erases them. The boundary closes only by a registration, never by
  this documentation. Recompute the owner list with the key used here:
  a model file that embeds `dbkit.TenantModel`, declares
  `GetTenantID()`, or carries a `var _ dbkit.TenantScoped` assertion
  (`grep` over `go/` and `examples/reference-app` for those shapes,
  minus `_test.go` files and `go/dbkit`).
- **`RenderAuditReport` has no real business-module consumer.** `go/admin`'s
  audit-query shell is a real consumer of `AuditQuery.Query` but serves
  raw JSON events, never a rendered report, and the reference app wires
  no report-download endpoint. A genuine consumer would mean an HTTP
  surface -- not built. The API's usage is pinned by the compiled-and-run
  `ExampleRenderAuditReport` (`example_test.go`, executed by every
  `go test` in this module's CI leg).
- **No HTTP surface / OpenAPI fragment.** `Module.OpenAPISpec` returns
  `nil` -- compliance is a Go-level API, not a service reached over HTTP.
- **`ExportService.Export`'s delivery through `go/sharing` has no live
  consumer proving the actual bytes reach a real subject over HTTP.**
  `sharing.Service.Access` resolves a token to its `Share` row and does
  not itself resolve `ResourceRef` into bytes, so no caller can yet
  retrieve an export's actual content this way.
- **No PostgreSQL integration tier for `go/compliance` itself.** The
  whole suite is unit-level, backed by SQLite and an in-memory
  `pkgcore.EventBus`/`KVStore`. Nothing in the module's own logic is
  dialect-sensitive (every SQL it issues is composed by participants'
  own repositories or by `dbkit/audit.Repository`, both already
  dual-dialect-proven in their own modules), so the value of one would be
  modest, but it is not shipped. The append-only trigger's own
  PostgreSQL proof lives in `go/dbkit/audit/integration_test/`, where
  the migrated table and the trigger actually live.
- **No hash chain, no partitioned archival** -- see "Deliberately not in
  scope".

## Testing

- **Unit tests**: SQLite only, no Docker required. `internal/testutil`
  builds migrated fixture databases via `dbtest.NewSQLite` plus a
  portable, dual-dialect-safe `CREATE TABLE` string; `module_test.go`'s
  and `example_test.go`'s own `*audit.Repository` setup applies
  `dbkit/audit`'s real migrations through `dbkit.MigrationRegistry`, so
  the audit half of every fixture is proven against the module's actual,
  versioned SQL, not an `AutoMigrate`.
- `retention_test.go` covers the cutoff itself, the cross-tenant
  isolation proof, the idempotency proof, partial-participant-failure
  handling (including the count-survives-error accounting and the
  classification-only audit record), an empty-registry clean pass,
  `SweepAllTenants`'s multi-tenant aggregation and its no-lister refusal,
  `EnqueueRetentionSweep`'s task shape and its no-tenant/no-queue
  refusals, and `retentionSweepHandler`'s happy path plus its
  payload-shape refusal.
- `erasure_test.go` covers erasing a live (never soft-deleted) row -- the
  property that distinguishes erasure from a sweep -- the cross-tenant
  non-erasure proof (the SAME subject id in two tenants), the audited
  proof (asserting the action, resource, result and requester
  attribution), the empty-`SubjectRef` refusal, the no-tenant and
  tenant-mismatch refusals (a recorder participant proves zero `Erase`
  callbacks ran and the other tenant's rows survive), the partial-
  failure-then-retry-converges proof, the empty-actor fallback
  attribution, and the partial-count-survives-error accounting.
- `export_test.go` covers gathering a live row's data and round-tripping
  the stored object through a real `pkgcore.NewLocalObjectStore`, a
  participant that opted out contributing nothing,
  partial-participant-failure handling (and that delivery still happens
  for what was gathered), and, against a scripted `fakeSharingCreator`:
  the delivered share's `ResourceRef`/`Sensitive`/`MaxViews`/`Password`/
  `ExpiresAt`, `ErrSharingRequired` when no `SharingCreator` is wired,
  and `ErrExportDeliveryFailed` -- with the manifest and object key still
  returned and the stored object deleted, three failing attempts leaving
  zero objects behind. The tenant-gate refusals (`pkgcore.ErrNoTenant`,
  `ErrExportTenantMismatch`) and the error-classification regressions
  (manifest bytes, audit `Changes["errors"]` and delivery-failure
  `FailureReason` all classify, never carry text) are pinned here too.
  `TestExportService_Export_DeliversThroughRealSharingService` is the
  one test that swaps `fakeSharingCreator` for a real
  `sharing.NewModule(db)` (real migrations, attached through the real
  `Module.Register`), proving the minted token round-trips through the
  real `Service.Access` back to a `Share` naming the export's own stored
  object key.
- `export_cleanup_test.go` proves the manifest retention story end to end
  through `RetentionService.SweepTenant`'s public surface: exactly the
  swept tenant's expired-live-share manifest is reaped while live-share,
  other-tenant, out-of-prefix and already-reaped objects all survive and
  a second pass converges to 0.
- `audit_query_test.go` covers tenant-scoped `Query` (including the
  no-tenant-in-context refusal), every `QueryFilter` field independently
  (the on-behalf-of dimension returns exactly one administrator's
  impersonation-era rows and excludes a second administrator's and the
  ordinary non-impersonation rows), newest-first ordering with the
  same-timestamp ID-descending tiebreak, `QueryAcrossTenants`'s
  system-context gate and its multi-tenant merge, and `Get`'s not-found
  passthrough.
- `module_test.go` covers the queueless-boot refusal, `Register`'s full
  declared surface (both config items, all four permissions, all three
  audit actions) bootstrapped through a real `pkgcore.Kernel`, the
  retention-sweep job handler landing on `reg.Jobs`, the
  `compliance.export_manifests` participant landing on `reg.Retention`,
  every service's registry-derived seams being non-nil after `Bootstrap`,
  and `WithSharing` wiring `ExportService.sharing` directly at
  construction time, with no `Bootstrap` needed to observe it.
- `report_test.go` covers `RenderAuditReport`: a JSON round trip and a
  CSV round trip over the same deliberately awkward fixture set (a plain
  event, an impersonated one, and one whose Action/FailureReason/Changes
  all carry commas, double quotes and an embedded newline), an explicit
  assertion on the raw rendered CSV bytes that quoting/escaping actually
  happened, the formula-injection protection (cells beginning with =, +,
  -, @, tab or carriage return come out single-quote-prefixed, asserted
  cell by cell on the parsed report), the empty-events case for both
  formats, and `ErrUnsupportedReportFormat`. The CSV-parsing half of the
  round trip is this test file's own private helper, not a package
  export.
- `example_test.go`'s `Example()` is runnable documentation of the whole
  mechanism in one self-contained pass: it wires `compliance.Module`
  alongside a fake business module implementing `pkgcore.Module` in full,
  seeds one expired soft-deleted row, sweeps it, and checks the reaped
  count via `// Output:`. The same file's `ExampleRenderAuditReport` is
  the runnable documentation of `RenderAuditReport`. The real-consumer
  proof lives in the host app's suite: `examples/reference-app/cmd/
  server/compliance_flow_test.go` drives all three orchestrations against
  real notes rows through the wired `*compliance.Module`, because that
  proof exercises host wiring no unit test inside `go/compliance` can
  reach.
- **PostgreSQL integration leg**: not applicable to `go/compliance`
  itself -- see Known limitations.

## Error index

| Code | `apperr` kind | Raised by |
|---|---|---|
| `compliance.queue_required` | `Internal` | `Module.Register`, when no `jobs.Queue` was wired through `WithQueue` |
| `compliance.tenant_lister_required` | `Internal` | `RetentionService.SweepAllTenants`, when no `TenantLister` was wired through `WithTenantLister` |
| `compliance.config_service_required` | `Internal` | Declared for a caller of `RetentionWindow` that wants the error instead of the fallback; `SweepTenant` itself never returns it (see `retention.go`'s `RetentionWindow` doc comment) |
| `compliance.audit_record_failed` | `Internal` | `SweepTenant`/`Erase`/`Export`, when the operation itself completed but its `dbkit/audit.Emit` call failed -- an audit-write failure must alert, never silently drop |
| `compliance.audit_query_requires_system_context` | `Invalid` | `AuditQuery.QueryAcrossTenants`, when `ctx` carries no system context |
| `compliance.empty_subject_ref` | `Invalid` | `ErasureService.Erase`, when the given `pkgcore.SubjectRef` has an empty `TenantID` or `SubjectID` |
| `compliance.sweep_partial_failure` | `Internal` | `RetentionService.SweepTenant`, when at least one participant's `Sweep` callback failed |
| `compliance.erasure_partial_failure` | `Internal` | `ErasureService.Erase`, when at least one participant's `Erase` callback failed |
| `compliance.export_partial_failure` | `Internal` | `ExportService.Export`, when at least one participant's `Export` callback failed |
| `compliance.sharing_required` | `Internal` | `ExportService.Export`, when no `SharingCreator` was wired through `WithSharing` |
| `compliance.export_delivery_failed` | `Internal` | `ExportService.Export`, when the manifest was gathered and stored but the wired `SharingCreator`'s `Create` call failed |
| `compliance.export_tenant_mismatch` | `Invalid` | `ExportService.Export`, when the `tenant` argument differs from the tenant `ctx` carries -- refused before anything is gathered, stored or delivered |
| `compliance.erasure_tenant_mismatch` | `Invalid` | `ErasureService.Erase`, when `subject.TenantID` differs from the tenant `ctx` carries -- refused before any participant is called |
| `compliance.unsupported_report_format` | `Invalid` | `RenderAuditReport`, when `format` is neither `ReportFormatCSV` nor `ReportFormatJSON` |

Every code above has a matching description entry in
`locales/{zh-CN,en-US}.toml` under the identical id.

## Deliberate design decisions

These shapes are decisions, not oversights -- a change to any of them is
a real design change, not a cleanup.

**compliance owns no table and ships no migrations.** The durable record
is `dbkit/audit`'s existing table plus each participant's own.
`Module.Migrations()` returning the zero `embed.FS` is documented as
legal by `dbkit.MigrationRegistry.Register` itself.

**`Erase`/`SweepTenant`/`Export` all return a specifically coded
partial-failure error even though the underlying work already partly
succeeded.** A caller checking only `err != nil` must still learn that
something needs attention, never silently treat a partial, still-
important-to-know-about outcome as a clean success.

**`ExportService.Export` requires a `SharingCreator` to complete at all
(`ErrSharingRequired`), while `RetentionService`/`ErasureService` need no
such thing.** An export this module cannot deliver is not a completed
export (the capability is gathering *and* delivery), while a sweep or an
erasure is complete the moment the underlying rows are gone. This is why
the check is `Export`'s own, call-time responsibility, not a
`Register`-time one the way `WithQueue`'s is.

**`go/compliance` imports `go/sharing` directly, rather than through a
structurally-typed no-import seam.** sharing sits strictly below
compliance in the module dependency graph, so an import edge is
architecturally legal. `SharingCreator` is still declared as a small
interface -- for test isolation, not to avoid the import.

**`AuditQuery` filters in Go, not SQL.** `dbkit/audit.Repository`'s own
doc comment assigns the rich query API to compliance's read side, and
compliance adds no method to the Repository. The honest performance
tradeoff this implies is stated in `AuditQuery`'s own doc comment and
this file's "AuditQuery" section.

**`pkgcore.RetentionParticipant`/`RetentionRegistrar`/`SubjectRef` live
in `go/pkgcore/registry.go`, not in this module.** The registrar is
genuinely multi-consumer -- every business module that wants
retention/erasure/export participation registers through it -- which is
exactly why the `Registry` gains the seat rather than the mechanism
living in a single consumer's module.

**The append-only trigger migration lives in `go/dbkit/audit`, not in
`go/compliance`.** `audit_events` is `dbkit/audit`'s own table, created
and versioned by `dbkit/audit`'s own migration set, and compliance owns
no table at all -- there is no compliance-owned place a trigger on
someone else's table could legally attach.

**The append-only mechanism is a trigger pair, not a REVOKE-based
restricted database role.** `dbkit.Open` hands every table's traffic
through exactly one connection/role per `*gorm.DB`, and this codebase
does not provision a second, more restricted role even for PostgreSQL
RLS's own role and policy -- role/credential provisioning is a
deployment-side responsibility the dbkit package assumes but does not
create. A trigger enforces the guarantee against any role that ever
connects; a REVOKE grant on a role this codebase does not create or name
would not bind to anything real.

**`RenderAuditReport`'s CSV encoding carries an explicit
`has_on_behalf_of` boolean column rather than leaving `OnBehalfOf`'s
three fields to speak for themselves.** `OnBehalfOf` is a genuinely
nullable `*pkgcore.Actor`, and three blank CSV cells cannot distinguish
"no impersonation" from "impersonated by an actor whose fields happen to
be empty".

**`RenderAuditReport` ships no public CSV/JSON parser, even though its
own test proves a full round trip.** The shipped shape is a render
direction; the round-trip parsing exists to verify the renderer's own
correctness, not to establish a second, symmetric read API nothing has
asked for.

**`ExportService.Export` keeps its `tenant` argument even though the `ctx`
tenant is the only tenant it ever reads.** The argument exists for the
job-handler-shaped caller that rebuilt `ctx` from a stored tenant id
(`pkgcore.WithTenant(ctx, job.TenantID)`) and hands that same id through.
The `ctx` tenant is the single enforced data boundary; the argument is
validated (`ErrExportTenantMismatch`) to echo it back exactly, never to
widen it.
