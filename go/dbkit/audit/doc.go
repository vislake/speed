// Package audit is the M1 audit-infrastructure round's persistence half:
// the AuditEvent model, its dual-dialect migrations, and Repository, the
// append-only accessor that stores and reads it back.
//
// Scope of this package, as of this milestone (docs/internal/15-roadmap.md's
// M1 audit-infrastructure item; docs/internal/10-compliance-and-audit.md's
// full design):
//
//   - Shipped here: AuditEvent (model.go), its migrations (migrations/),
//     and Repository's Insert/Get/ListByTenant (repository.go).
//   - Shipped elsewhere in this same round, once landed: automatic
//     write-capture and explicit Emit (the collection mechanisms), and the
//     pkgcore.Module persister that subscribes to their published events
//     and calls Repository.Insert -- both layer directly on the types this
//     package exports and add no new table of their own.
//   - Deferred to M4 (go/compliance, per docs/internal/10-compliance-and-
//     audit.md's own delivery-phase correction): immutability enforcement
//     at the database-role level, the optional hash chain,
//     retention/archival, and the actor/resource/action/time-range/result
//     query and report API. This package's ListByTenant is only the
//     minimal read path its own tests (and this round's reference-app
//     proof test) need -- not that query surface.
//
// Module home: this package lives inside go/dbkit, not as its own
// go.work module and not inside go/compliance (a stub until M4). See this
// round's scope-freeze report for the full evidence chain; in short,
// go/dbkit already owns the one GORM-callback plugin precedent
// (tenant_scope.go), the migration machinery (MigrationRegistry), and the
// "real tenant_id column that is not TenantScoped" precedent this table
// needs (go/jobs's jobRecord, go/config's row) -- and every module that
// will eventually want to emit an audit event already depends on dbkit
// transitively, so this costs the rest of the M1 modules zero new import
// edges.
package audit
