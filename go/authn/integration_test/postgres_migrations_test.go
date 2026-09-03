//go:build integration

// Package authn_test holds go/authn's integration tier: tests that exercise
// a real PostgreSQL server instead of SQLite, the standalone deployment
// mode's own dialect. It is physically separate from authn's unit tests
// (all of which live in package authn itself, one file per source file, per
// the backend coding standard's testing layout rule) and carries the
// "integration" build tag: a plain "go test ./..." never compiles or runs
// anything in this directory; it is invoked explicitly with "go test
// -tags=integration ./...". This mirrors the identical convention of
// go/dbkit/integration_test, go/config/integration_test and
// go/jobs/integration_test.
//
// There is no Redis tier here (unlike go/config's, which carries one to
// prove cross-replica convergence): authn touches pkgcore.KVStore only
// through go/ratelimit and the immediate-revocation list, and both of those
// are already the KVStore contract's own test responsibility, not this
// module's -- see the frozen round plan's §1.9 and go/ratelimit/AGENTS.md.
//
// Every test here spins up its own disposable PostgreSQL testcontainer
// (via testutil.NewPostgresDB, go/authn/internal/testutil) and requires a
// working Docker (or Docker-API-compatible) daemon; there is no fallback or
// skip-on-missing-Docker path here either -- dbtest.NewPostgres itself
// skips the individual test with t.Skip when no daemon is reachable,
// matching the other modules' tiers.
package authn_test

import (
	"testing"

	"github.com/vislake/speed/go/authn/internal/testutil"
)

// authnTables is every table authn's nine models map to (model.go,
// identity.go, oidc.go, verification.go, mfa.go's TableName overrides),
// enumerated once here so a table added to a migration without a matching
// model -- or vice versa -- is caught by name rather than silently ignored.
var authnTables = []string{
	"users", "sessions", "refresh_tokens", "login_attempts",
	"user_identities", "tenant_sso_configs", "verification_codes",
	"user_mfa_factors", "user_recovery_codes",
}

// TestMigrations_ZeroToHead_Postgres proves authn's nine migrations
// (0001..0009) apply cleanly from zero on real PostgreSQL and produce every
// table this module owns. testutil.NewPostgresDB itself already fails the
// test outright if dbkit.MigrationRegistry.Apply returns an error, so
// reaching the assertions below is already proof migration application
// succeeded; what this test adds is confirming the SPECIFIC schema that
// application produced -- the exact table set, not merely "something
// applied without erroring".
func TestMigrations_ZeroToHead_Postgres(t *testing.T) {
	t.Parallel()

	db := testutil.NewPostgresDB(t)
	migrator := db.Migrator()

	for _, table := range authnTables {
		if !migrator.HasTable(table) {
			t.Errorf("table %q does not exist after applying every migration from zero", table)
		}
	}

	// One column per table, so a migration that created its table but
	// left a column out -- or a later migration that never ran at all --
	// has a chance of being caught here rather than only a missing table.
	columnChecks := []struct{ table, column string }{
		{"users", "email_index"},
		{"sessions", "current_tenant_id"},
		{"refresh_tokens", "family_id"},
		{"login_attempts", "ip_region"},
		{"user_identities", "external_id"},
		{"tenant_sso_configs", "client_secret"},
		{"verification_codes", "target_index"},
		{"user_mfa_factors", "secret"},
		{"user_recovery_codes", "code_hash"},
	}
	for _, c := range columnChecks {
		if !migrator.HasColumn(c.table, c.column) {
			t.Errorf("table %q has no column %q after applying every migration from zero", c.table, c.column)
		}
	}
}
