//go:build integration

package dbkit_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/vislake/speed/go/dbkit/internal/testutil"
)

// TestPostgresRowLevelSecurity_DefenseInDepthBelowDbkit is a proof of
// concept for the THIRD isolation layer dbkit.Repository's own doc comment
// names but explicitly does not implement or exercise itself: "PostgreSQL
// row-level security in production, as a final backstop below the Go layer
// entirely; not exercised by this package" (repository.go).
//
// Everything here is done with raw SQL over database/sql (via pgx's stdlib
// driver), deliberately never touching GORM or dbkit's own Go types for the
// role/policy setup or for the queries that prove the boundary — this test
// exists specifically to prove the database enforces tenant isolation on
// its own, independent of anything Go-side ever running correctly. It:
//
//  1. Creates the widgets table (the same fixture schema dbkit's own tests
//     use) and seeds one row each for tenant A and tenant B directly.
//  2. Creates a normal, unprivileged role and enables row-level security on
//     the table with a policy keyed on current_setting('app.current_tenant').
//  3. Proves that role, querying AS ITSELF over its own authenticated
//     connection, cannot see the other tenant's row even with a bare
//     "SELECT * FROM widgets" carrying no WHERE clause at all — and that it
//     sees NOTHING when app.current_tenant is never set (fail closed, not
//     fail open, at the database layer too).
//  4. Creates a second role with the BYPASSRLS attribute and proves it sees
//     every row regardless of app.current_tenant.
//
// IMPORTANT GAP THIS TEST DOCUMENTS: dbkit has no helper anywhere (in Open,
// Repository, or otherwise) that sets app.current_tenant — or switches
// database role — per request/transaction. Grep the package: there is no
// "current_setting", "SET LOCAL", "SET ROLE", or similar in open.go,
// repository.go, or tenant_scope.go. This test's own restricted-role
// connection sets app.current_tenant itself, by hand, because dbkit
// provides nothing that would do it for a real caller. In other words: this
// proves the DATABASE's RLS mechanism works correctly in isolation, but
// dbkit today has no wiring that would ever cause a production connection
// to run AS a restricted role with app.current_tenant set to anything — the
// "layer 3" backstop the doc comment describes is not yet connected to
// anything upstream of it. See this test's summary in the review report for
// the concrete follow-up this implies (a per-request/per-transaction
// SET LOCAL app.current_tenant + a restricted, non-BYPASSRLS application
// database role, wired into dbkit.Open or a transaction-scoped helper).
func TestPostgresRowLevelSecurity_DefenseInDepthBelowDbkit(t *testing.T) {
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
	if err := adminDB.PingContext(ctx); err != nil {
		t.Fatalf("admin connection ping: %v", err)
	}

	const schema = "rls_poc"
	const table = schema + ".widgets"

	mustExec(t, adminDB, ctx, "CREATE SCHEMA "+schema)

	// The exact same fixture schema dbkit's own unit and integration tests
	// use, qualified into this test's own throwaway schema so it can never
	// collide with anything else running against this (disposable, single
	// use) container.
	widgetDDL := testutil.WidgetPostgresMigrationSQL(t)
	mustExec(t, adminDB, ctx, qualifyCreateTable(t, widgetDDL, table))

	mustExec(t, adminDB, ctx, fmt.Sprintf(
		`INSERT INTO %s (id, tenant_id, name, value) VALUES ('widget-a', 'tenant-a', 'a-secret', 1)`, table))
	mustExec(t, adminDB, ctx, fmt.Sprintf(
		`INSERT INTO %s (id, tenant_id, name, value) VALUES ('widget-b', 'tenant-b', 'b-secret', 2)`, table))

	// --- Set up the RLS-restricted application role. ---
	const restrictedRole = "rls_app_role"
	const restrictedPassword = "rls-app-role-password" // throwaway, disposable container only
	mustExec(t, adminDB, ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, restrictedRole, restrictedPassword))
	mustExec(t, adminDB, ctx, fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, schema, restrictedRole))
	mustExec(t, adminDB, ctx, fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON %s TO %s`, table, restrictedRole))

	mustExec(t, adminDB, ctx, fmt.Sprintf(`ALTER TABLE %s ENABLE ROW LEVEL SECURITY`, table))
	mustExec(t, adminDB, ctx, fmt.Sprintf(
		`CREATE POLICY tenant_isolation ON %s
		   USING (tenant_id = current_setting('app.current_tenant', true))
		   WITH CHECK (tenant_id = current_setting('app.current_tenant', true))`, table))

	// --- Set up the BYPASSRLS role. ---
	const bypassRole = "rls_bypass_role"
	const bypassPassword = "rls-bypass-role-password" // throwaway, disposable container only
	mustExec(t, adminDB, ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' BYPASSRLS`, bypassRole, bypassPassword))
	mustExec(t, adminDB, ctx, fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, schema, bypassRole))
	mustExec(t, adminDB, ctx, fmt.Sprintf(`GRANT SELECT ON %s TO %s`, table, bypassRole))

	roleDSN := func(role, password string) string {
		return fmt.Sprintf("postgres://%s:%s@%s:%s/dbkit?sslmode=disable", role, password, host, mappedPort.Port())
	}

	t.Run("RestrictedRole_TenantA_SeesOnlyTenantARow_EvenWithNoWhereClause", func(t *testing.T) {
		conn := openRole(t, ctx, roleDSN(restrictedRole, restrictedPassword))
		mustExec(t, conn, ctx, `SET app.current_tenant = 'tenant-a'`)

		ids := queryIDs(t, conn, ctx, fmt.Sprintf(`SELECT id FROM %s`, table)) // deliberately no WHERE clause
		if len(ids) != 1 || ids[0] != "widget-a" {
			t.Errorf("restricted role as tenant-a, bare SELECT * = %v, want exactly [widget-a] (RLS must hide tenant-b's row even with no WHERE clause)", ids)
		}
	})

	t.Run("RestrictedRole_TenantB_SeesOnlyTenantBRow_EvenWithNoWhereClause", func(t *testing.T) {
		conn := openRole(t, ctx, roleDSN(restrictedRole, restrictedPassword))
		mustExec(t, conn, ctx, `SET app.current_tenant = 'tenant-b'`)

		ids := queryIDs(t, conn, ctx, fmt.Sprintf(`SELECT id FROM %s`, table))
		if len(ids) != 1 || ids[0] != "widget-b" {
			t.Errorf("restricted role as tenant-b, bare SELECT * = %v, want exactly [widget-b]", ids)
		}
	})

	t.Run("RestrictedRole_NoTenantSettingAtAll_SeesNothing_FailsClosedAtDBLayer", func(t *testing.T) {
		// A fresh connection: app.current_tenant was never set on it at
		// all, so current_setting(..., true) returns NULL, and
		// "tenant_id = NULL" is never true in SQL — the policy hides every
		// row, rather than exposing them all. This is the database-level
		// analogue of dbkit's own Go-side fail-closed default
		// (ErrMissingTenantContext) and matters precisely because dbkit
		// itself never sets this session variable (see this test's own doc
		// comment): if a real caller ever reached this restricted role
		// without remembering to set it, this is the behavior it would get.
		conn := openRole(t, ctx, roleDSN(restrictedRole, restrictedPassword))

		ids := queryIDs(t, conn, ctx, fmt.Sprintf(`SELECT id FROM %s`, table))
		if len(ids) != 0 {
			t.Errorf("restricted role with app.current_tenant never set = %v, want no rows (fail closed, not fail open)", ids)
		}
	})

	t.Run("RestrictedRole_CrossTenantWrite_AffectsNoRows", func(t *testing.T) {
		conn := openRole(t, ctx, roleDSN(restrictedRole, restrictedPassword))
		mustExec(t, conn, ctx, `SET app.current_tenant = 'tenant-a'`)

		res, err := conn.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET value = 9999 WHERE id = 'widget-b'`, table))
		if err != nil {
			t.Fatalf("cross-tenant UPDATE as tenant-a error = %v", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			t.Fatalf("RowsAffected() error = %v", err)
		}
		if n != 0 {
			t.Errorf("cross-tenant UPDATE (tenant-a targeting tenant-b's row) affected %d rows, want 0", n)
		}

		// Confirm via the admin connection (BYPASSRLS-equivalent by
		// ownership) that tenant B's row is genuinely untouched.
		var value int
		if err := adminDB.QueryRowContext(ctx, fmt.Sprintf(`SELECT value FROM %s WHERE id = 'widget-b'`, table)).Scan(&value); err != nil {
			t.Fatalf("admin verification query error = %v", err)
		}
		if value != 2 {
			t.Errorf("tenant-b row value after cross-tenant UPDATE attempt = %d, want unchanged 2", value)
		}
	})

	t.Run("BypassRLSRole_SeesEveryRow_RegardlessOfCurrentTenantSetting", func(t *testing.T) {
		conn := openRole(t, ctx, roleDSN(bypassRole, bypassPassword))
		// Deliberately not setting app.current_tenant at all, and even
		// deliberately setting it to a tenant that owns neither row, to
		// make the point unambiguous: BYPASSRLS ignores the policy
		// entirely, it does not merely default-allow when the setting is
		// absent.
		mustExec(t, conn, ctx, `SET app.current_tenant = 'tenant-nonexistent'`)

		ids := queryIDs(t, conn, ctx, fmt.Sprintf(`SELECT id FROM %s ORDER BY id`, table))
		if len(ids) != 2 || ids[0] != "widget-a" || ids[1] != "widget-b" {
			t.Errorf("BYPASSRLS role SELECT * = %v, want exactly [widget-a widget-b] (BYPASSRLS must see every tenant's rows unconditionally)", ids)
		}
	})

	// No SQL-level cleanup (DROP ROLE / DROP SCHEMA): this container is
	// single-use and terminated in its entirety by startPostgresContainer's
	// t.Cleanup, so there is nothing left for any other test to see.
}

// mustExec runs a statement with no result rows and fails t on error,
// naming the statement in the failure message for a fast diagnosis.
func mustExec(t *testing.T, db *sql.DB, ctx context.Context, stmt string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}

// openRole opens a fresh *sql.DB against dsn and closes it via t.Cleanup.
// Each RLS subtest gets its own fresh connection deliberately, rather than
// reusing one across subtests, so a SET app.current_tenant left over from
// an earlier subtest can never leak into a later one and mask a real
// failure.
func openRole(t *testing.T, ctx context.Context, dsn string) *sql.DB {
	t.Helper()
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.PingContext(ctx); err != nil {
		t.Fatalf("ping as role via dsn: %v", err)
	}
	return conn
}

// queryIDs runs query (expected to select a single "id" column) and returns
// the ids it found, in result order.
func queryIDs(t *testing.T, db *sql.DB, ctx context.Context, query string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan row for %q: %v", query, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err() for %q: %v", query, err)
	}
	return ids
}

// qualifyCreateTable rewrites the fixture's "CREATE TABLE widgets (...)"
// into "CREATE TABLE <table> (...)" so this test never depends on
// search_path for any statement — every reference to the table anywhere in
// this file is fully schema-qualified instead, which is simpler to reason
// about than session-scoped search_path once several separately-pooled
// connections (admin, restricted role, bypass role) are all in play at
// once.
func qualifyCreateTable(t *testing.T, ddl, qualifiedTable string) string {
	t.Helper()
	const marker = "CREATE TABLE widgets"
	if !strings.Contains(ddl, marker) {
		t.Fatalf("fixture widget migration does not contain %q; update qualifyCreateTable to match its current text", marker)
	}
	return strings.Replace(ddl, marker, "CREATE TABLE "+qualifiedTable, 1)
}
