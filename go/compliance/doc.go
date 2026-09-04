// Package compliance provides the governance layer over the deletion,
// audit and export primitives that already ship lower in the module
// graph: dbkit's mark-delete/hard-delete pair, dbkit/audit's append-only
// audit trail, and tenancy's audited WithSystemContext escape hatch.
// docs/internal/10-compliance-and-audit.md is explicit about this
// phasing -- field encryption, blind indexes, soft delete and hard delete
// are dbkit (M0-M1), audit collection and persistence are dbkit/audit
// (M1), and compliance's own job is the M4 governance layer built on top:
// retention-window scheduling, right-to-erasure orchestration, data-export
// gathering and a read-only audit query surface. This module invents no
// new deletion mechanism -- every physical delete it ever causes runs
// through a business module's own dbkit.Repository[T].HardDelete, already
// tenant-bound and system-context-gated (go/dbkit/hard_delete.go).
//
// # Orchestration, not ownership
//
// compliance never imports a business module's repository or model type.
// Instead, pkgcore.Registry.Retention (pkgcore.RetentionRegistrar) is the
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
// contracts in full, and this module's AGENTS.md for the worked example
// every business module follows to register one.
//
// # Round 1 of an unbounded number
//
// This is a deliberately bounded foundation round, proved end to end
// against a fake participant registered by this module's own test suite
// (internal/testutil) rather than against a real business module -- no
// existing business module is modified to opt in this round; that is each
// owning module's own, later decision. It ships:
//
//   - RetentionService: a per-tenant retention-window sweep
//     (SweepTenant), a periodic jobs.Handler wrapping it
//     (EnqueueRetentionSweep, the host-facing schedule point), and an
//     optional TenantLister seam (SweepAllTenants) so a host that wants
//     one scheduled task to cover every tenant can supply one without
//     compliance importing org.
//   - ErasureService: Erase, the right-to-erasure entry point, bypassing
//     the retention window and calling every participant's Erase callback
//     under an audited system context.
//   - ExportService: Export, the data-portability gathering half only --
//     every participant's Export data is aggregated into one JSON
//     document and stored through the pkgcore.ObjectStore seam. Delivery
//     to the requesting subject (go/sharing) is an explicit, named
//     follow-up: go/sharing has not landed in this round's module graph.
//   - AuditQuery: a read-only query layer over dbkit/audit.Repository's
//     existing, deliberately thin ListByTenant/Get surface, adding
//     actor/resource/action/time-range/result filtering and (under a
//     system context only) a caller-supplied multi-tenant scan --
//     dbkit/audit.Repository itself is untouched, per this round's own
//     scope boundary; see AuditQuery's doc comment for exactly what that
//     means for performance.
//
// It does NOT ship append-only enforcement via database role privilege
// revocation or triggers, the optional hash chain, time-partitioned audit
// archival, formatted CSV/JSON report export, an HTTP surface, or
// go/sharing-based export delivery. See AGENTS.md's Known limitations for
// the complete boundary and the compensating godoc Example this round
// ships in its place.
package compliance
