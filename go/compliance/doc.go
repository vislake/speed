// Package compliance provides the governance layer over the deletion,
// audit and export primitives that already ship lower in the module
// graph: dbkit's mark-delete/hard-delete pair, dbkit/audit's append-only
// audit trail, and tenancy's audited WithSystemContext escape hatch. The
// module invents no new deletion mechanism -- every physical delete it
// ever causes runs through a business module's own
// dbkit.Repository[T].HardDelete, already tenant-bound and
// system-context-gated (go/dbkit/hard_delete.go) -- and owns no table and
// ships no migrations: the durable record is dbkit/audit's existing table
// plus each participant's own already-migrated one.
//
// # Orchestration, not ownership
//
// compliance never imports a business module's repository or model type.
// Instead, pkgcore.ComponentRegistry.Retention (pkgcore.RetentionRegistrar) is the
// seam a business module registers a pkgcore.RetentionParticipant on
// during its own Register call: a Name plus three callbacks -- Sweep
// (retention-window cleanup for one tenant), Erase (immediate
// right-to-erasure for one subject) and an optional Export (data-export
// gathering) -- each implemented by the participant itself, over its own
// dbkit.Repository[T]. compliance's services (RetentionService,
// ErasureService, ExportService) do nothing but discover the registered
// participants at call time, invoke their callbacks under the right
// context, aggregate the results and audit the operation. See
// pkgcore/registry.go's RetentionParticipant doc comment for the callback
// contracts in full.
//
// # What the module ships
//
//   - RetentionService: a per-tenant retention-window sweep (SweepTenant),
//     a periodic jobs.Handler wrapping it (EnqueueRetentionSweep, the
//     manual entry point beside the schedule Register declares), and an
//     optional TenantLister seam (SweepAllTenants) so a host that wants
//     one call to cover every tenant can supply one without compliance
//     importing org.
//   - ErasureService: Erase, the right-to-erasure entry point, bypassing
//     the retention window and calling every participant's Erase callback
//     under an audited system context.
//   - ExportService: Export, the tenant-data export capability -- every
//     participant's Export data for one tenant is aggregated into one
//     JSON document, stored through the pkgcore.ObjectStore seam, and
//     delivered as a short-lived, single-view go/sharing.Share whose
//     one-time token Export returns to its caller to relay (see
//     ExportDelivery and SharingCreator's own doc comments). The bundle is
//     the whole tenant's data, never one subject's personal data; a
//     subject-scoped ("this is your data") export is not built.
//   - AuditQuery: a read-only query layer over dbkit/audit.Repository's
//     existing, deliberately thin ListByTenant/Get surface, adding
//     actor/on-behalf-of/resource/action/time-range/result filtering and
//     (under a system context only) a caller-supplied multi-tenant scan.
//     Filtering happens in application code over everything the repository
//     returns -- there is no SQL-level pushdown; see AuditQuery's doc
//     comment for exactly what that means for performance.
//   - RenderAuditReport (report.go): pure CSV/JSON rendering of an
//     []audit.AuditEvent, with no I/O, pagination or tenant-scope
//     enforcement of its own.
//   - ParticipantFailureReason (participants.go): the shared rendering of a
//     failed-participant set -- the sorted participant names after the
//     "participants failed: " prefix -- used as the participants parameter
//     of the three partial-failure errors and the FailureReason of the
//     sweep, erasure and export audit events, and consumed by go/admin's
//     audit-export leg for its own audit event.
//
// What is not shipped here is a boundary, not a gap: no HTTP surface of
// its own, no hash chain over the audit trail, no time-partitioned
// archival, and no subject-scoped export. Database-level append-only
// enforcement on audit_events lives in go/dbkit/audit's own migrations,
// not in this module.
package compliance
