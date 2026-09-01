//go:build integration

package dbkit_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// TestWithTenantSession_PostgresRLS_RealPolicyHonorsTheGUCItSets closes the
// exact gap postgres_rls_test.go's own doc comment names: that test proves
// PostgreSQL's RLS mechanism works when app.current_tenant is set by hand,
// raw SQL, no dbkit code involved. This test proves dbkit.WithTenantSession
// itself — the real, shipped helper — is what makes that happen for an
// actual caller, through the exact SET step it actually runs: a
// "SELECT set_config(...)" function call, not a literal
// "SET LOCAL app.current_tenant = $1", which PostgreSQL rejects outright
// with a syntax error (see WithTenantSession's doc comment in
// tenant_session.go for why). Before that fix, this test's very first
// sub-test would fail with exactly that syntax error instead of asserting
// anything about row visibility.
//
// The query below deliberately never touches dbkit.Repository or any model
// implementing dbkit.TenantScoped, and its connection is opened with a
// plain gorm.Open — not dbkit.Open — so dbkit's Go-side layers 1 (the
// tenant-scoping plugin) and 2 (Repository) are entirely out of the
// picture: the only thing that can possibly hide tenant B's row from a
// query issued as tenant A here is PostgreSQL's own RLS policy, engaged
// purely by WithTenantSession's session-GUC step.
func TestWithTenantSession_PostgresRLS_RealPolicyHonorsTheGUCItSets(t *testing.T) {
	ctx := context.Background()
	pgContainer := startPostgresContainer(t, ctx)
	adminDSN, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres testcontainer connection string: %v", err)
	}
	host, err := pgContainer.Host(ctx)
	if err != nil {
		t.Fatalf("container Host(): %v", err)
	}
	mappedPort, err := pgContainer.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("container MappedPort(): %v", err)
	}

	adminDB, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("sql.Open(admin) error = %v", err)
	}
	t.Cleanup(func() { _ = adminDB.Close() })
	if err = adminDB.PingContext(ctx); err != nil {
		t.Fatalf("admin connection ping: %v", err)
	}

	const schema = "wts_rls"
	const table = schema + ".probe"
	mustExec(t, adminDB, ctx, "CREATE SCHEMA "+schema)
	mustExec(t, adminDB, ctx, fmt.Sprintf(`CREATE TABLE %s (id text PRIMARY KEY, tenant_id text NOT NULL)`, table))
	mustExec(t, adminDB, ctx, fmt.Sprintf(
		`INSERT INTO %s (id, tenant_id) VALUES ('row-a', 'tenant-a'), ('row-b', 'tenant-b')`, table))

	// A normal, unprivileged, non-BYPASSRLS role — the shape a real
	// production application role is expected to have (see
	// docs/internal/04-data-and-tenancy.md).
	const role = "wts_rls_role"
	const password = "wts-rls-role-password" // throwaway, disposable container only
	mustExec(t, adminDB, ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, role, password))
	mustExec(t, adminDB, ctx, fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, schema, role))
	mustExec(t, adminDB, ctx, fmt.Sprintf(`GRANT SELECT ON %s TO %s`, table, role))
	mustExec(t, adminDB, ctx, fmt.Sprintf(`ALTER TABLE %s ENABLE ROW LEVEL SECURITY`, table))
	mustExec(t, adminDB, ctx, fmt.Sprintf(
		`CREATE POLICY tenant_isolation ON %s USING (tenant_id = current_setting('app.current_tenant', true))`, table))

	roleDSN := fmt.Sprintf("postgres://%s:%s@%s:%s/dbkit?sslmode=disable", role, password, host, mappedPort.Port())
	roleDB, err := gorm.Open(postgres.Open(roleDSN), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("gorm.Open(restricted role) error = %v", err)
	}
	sqlRoleDB, err := roleDB.DB()
	if err != nil {
		t.Fatalf("get underlying sql.DB for the restricted role connection: %v", err)
	}
	t.Cleanup(func() { _ = sqlRoleDB.Close() })

	type probeRow struct {
		ID string
	}
	queryProbe := func(t *testing.T, tenant pkgcore.TenantID) []probeRow {
		t.Helper()
		var rows []probeRow
		if err := dbkit.WithTenantSession(ctxFor(tenant), roleDB, func(tx *gorm.DB) error {
			return tx.Table(table).Order("id").Find(&rows).Error
		}); err != nil {
			t.Fatalf("WithTenantSession() error = %v", err)
		}
		return rows
	}

	t.Run("TenantA_SeesOnlyItsOwnRow", func(t *testing.T) {
		rows := queryProbe(t, tenantA)
		if len(rows) != 1 || rows[0].ID != "row-a" {
			t.Errorf("rows for tenant A = %+v, want exactly [{row-a}] (RLS, engaged by WithTenantSession, must hide tenant B's row)", rows)
		}
	})

	t.Run("TenantB_SeesOnlyItsOwnRow", func(t *testing.T) {
		rows := queryProbe(t, tenantB)
		if len(rows) != 1 || rows[0].ID != "row-b" {
			t.Errorf("rows for tenant B = %+v, want exactly [{row-b}]", rows)
		}
	})

	t.Run("SettingDoesNotLeakAcrossCallsOnAPooledConnection", func(t *testing.T) {
		// Force a single physical connection, so this sub-test can only
		// pass if the GUC WithTenantSession set for tenant A's call truly
		// reverted at COMMIT, rather than surviving on the pooled
		// connection into tenant B's later, unrelated call — the exact
		// production risk WithTenantSession exists to prevent (see its doc
		// comment).
		sqlRoleDB.SetMaxOpenConns(1)

		rowsA := queryProbe(t, tenantA)
		if len(rowsA) != 1 || rowsA[0].ID != "row-a" {
			t.Fatalf("rows for tenant A = %+v, want exactly [{row-a}]", rowsA)
		}

		rowsB := queryProbe(t, tenantB)
		if len(rowsB) != 1 || rowsB[0].ID != "row-b" {
			t.Errorf("rows for tenant B, on the same pooled connection right after tenant A's call, = %+v, want exactly [{row-b}] (a leaked SET would show row-a here too, or in place of it)", rowsB)
		}
	})
}
