// Package audit is the persistence and declarative-collection home of the
// audit trail: the AuditEvent model, its dual-dialect migrations,
// Repository (the append-only accessor that stores and reads events back),
// Emit (the explicit collection mechanism), and Module (the persister that
// turns published events into stored rows, carrying the module contract). The
// complementary automatic-collection mechanism -- the GORM write-capture
// plugin -- lives one level up, in go/dbkit itself (audit_capture.go),
// since it has to be wired into dbkit.Open.
//
// What this package ships:
//
//   - AuditEvent (model.go) and its migrations (migrations/), including
//     the database-level append-only backstop
//     (migrations/{postgres,sqlite}/0002_append_only_enforcement.sql -- a
//     BEFORE UPDATE/DELETE trigger pair on audit_events, proven against a
//     raw *sql.DB bypassing Repository entirely in repository_test.go
//     and, for PostgreSQL, integration_test/postgres_append_only_test.go).
//   - Repository's Insert/Get/ListByTenant (repository.go) -- the minimal
//     read path its own tests need. ListByTenant is not a query surface:
//     there is no actor/resource/action/time-range/result search, no
//     retention/archival and no hash chain over the table (see
//     go/compliance for the closest read surface, AuditQuery, and its
//     formatted CSV/JSON report export).
//   - The explicit collection mechanism Emit (emit.go) and the
//     module-contract persister (module.go) that subscribes to both
//     collection mechanisms' events -- dbkit's own automatic GORM
//     write-capture plugin (go/dbkit/audit_capture.go, one level up) and
//     this package's own Emit -- plus tenancy's
//     EventSystemContextEntered, normalizing each into an AuditEvent and
//     calling Repository.Insert.
//
// Module home: this package lives inside go/dbkit, not as its own
// go.work module and not inside go/compliance. go/dbkit already owns the
// one GORM-callback plugin precedent (tenant_scope.go), the migration
// machinery (MigrationRegistry), and the "real tenant_id column that is
// not TenantScoped" precedent this table needs (go/jobs's jobRecord,
// go/config's row) -- and every module that emits an audit event already
// depends on dbkit transitively, so hosting the persister here adds zero
// new import edges for them.
package audit
