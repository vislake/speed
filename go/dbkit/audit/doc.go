// Package audit is the M1 audit-infrastructure round's persistence and
// declarative-collection home: the AuditEvent model, its dual-dialect
// migrations, Repository (the append-only accessor that stores and reads
// events back), Emit (the explicit collection mechanism), and Module (the
// pkgcore.Module persister that turns published events into stored rows).
// The complementary automatic-collection mechanism -- the GORM
// write-capture plugin -- lives one level up, in go/dbkit itself
// (audit_capture.go), since it has to be wired into dbkit.Open.
//
// Scope of this package, as of this milestone (docs/internal/15-roadmap.md's
// M1 audit-infrastructure item; docs/internal/10-compliance-and-audit.md's
// full design):
//
//   - Shipped here: AuditEvent (model.go), its migrations (migrations/),
//     Repository's Insert/Get/ListByTenant (repository.go), the explicit
//     collection mechanism Emit (emit.go), and the pkgcore.Module
//     persister (module.go) that subscribes to both collection
//     mechanisms' events -- dbkit's own automatic GORM write-capture
//     plugin (go/dbkit/audit_capture.go, one level up) and this package's
//     own Emit -- plus tenancy's already-shipped
//     EventSystemContextEntered, normalizing each into an AuditEvent and
//     calling Repository.Insert.
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
