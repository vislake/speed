// Package tenancytest provides the mandatory isolation-assertion test suite
// every other module is required to run against its own dbkit.Repository[T]
// usage (backend coding standard §3.3 / §13, and
// docs/internal/04-data-and-tenancy.md's data-domain table): AssertIsolated
// for tenant data and link data, AssertNotTenantScoped for identity data and
// platform data.
//
// Both functions are reusable assertions built entirely on top of dbkit's
// already-finished public API (dbkit.Repository[T], dbkit.TenantScoped,
// dbkit.Open) — this package contains no isolation logic of its own. Their
// purpose is narrower and just as important: so that every OTHER module's
// tests (authn, org, billing, and everything else built on speed) get the
// same isolation assurance dbkit's own tenant_scope_test.go and
// repository_test.go already establish for dbkit itself, without each
// module re-deriving the same test cases — and its own tenant-data /
// identity-platform-data split — from scratch.
//
// # Which function to call
//
// A model's data domain decides which assertion applies, never both:
//
//   - Tenant data or link data (implements dbkit.TenantScoped, and is used
//     as T in a dbkit.Repository[T]) — call AssertIsolated.
//   - Identity data or platform data (must NOT implement dbkit.TenantScoped,
//     and is queried through a plain *gorm.DB from dbkit.Open instead of a
//     Repository[T]) — call AssertNotTenantScoped.
//
// A table assigned to the wrong domain is exactly the class of bug this
// package exists to catch before it ships: a tenant table missing isolation
// is a horizontal-privilege-escalation vulnerability, and a genuinely global
// table accidentally made tenant-scoped is data that "mysteriously
// disappears" in production. See docs/internal/04-data-and-tenancy.md for
// the full data-domain table and its rationale.
//
// # Test-database setup
//
// Neither function opens or migrates a database itself. Callers build a
// *gorm.DB with github.com/vislake/speed/go/dbkit/dbtest (NewSQLite at
// minimum; NewPostgres too where practical, since dbkit's own tests found
// dialect-specific behavior that matters — NewPostgres skips itself via
// t.Skip when no Docker daemon is reachable, so including it costs nothing
// where Docker is unavailable), apply their own model's schema against it
// (a MigrationRegistry, or a lightweight db.Exec of a fixture's DDL — see
// dbtest's own doc comment), and only then call AssertIsolated or
// AssertNotTenantScoped against the result.
//
// # Dependencies
//
// This package imports dbkit, pkgcore, gorm, and the standard library, and
// nothing else. It is a leaf every future module's tests import, so
// anything added here becomes a test-time dependency of every one of them;
// keeping the list short is deliberate, not an oversight.
package tenancytest
