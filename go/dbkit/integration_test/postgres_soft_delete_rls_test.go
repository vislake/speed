//go:build integration

package dbkit_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/internal/testutil"
)

// TestRepository_PostgresRLS_SoftDeletedRowStillTenantFilteredCorrectly is
// this round's RLS proof, per
// docs/internal/04-data-and-tenancy.md's "删除语义" §4: "软删除行对
// PostgreSQL RLS 而言就是普通行，tenant_id 过滤照常生效" — a claim this test
// verifies against a real restricted role and a real RLS policy, rather
// than merely asserting it in a doc comment. No new policy is needed for a
// soft-deleted row: it is exactly the same UNIQUE(tenant_id, id) row the
// tenant_isolation policy already covers, whether or not deleted_at is set.
//
// This reuses postgres_repository_rls_test.go's exact restricted-role /
// policy shape and helpers (mustExec, openRole, queryIDs, startPostgresContainer,
// ctxFor, tenantA, tenantB — all package-level in dbkit_test), applied to
// testutil.SoftDeletableWidgetTableSQL instead of the plain widgets table —
// the same DDL string soft_delete_unique_index_test.go already exercises
// against SQLite, now proven against real PostgreSQL too (the dual-dialect
// half of that proof: "CREATE UNIQUE INDEX ... WHERE deleted_at IS NULL" is
// standard SQL, not PostgreSQL-specific, but only running it against a real
// server confirms that).
func TestRepository_PostgresRLS_SoftDeletedRowStillTenantFilteredCorrectly(t *testing.T) {
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

	mustExec(t, adminDB, ctx, testutil.SoftDeletableWidgetTableSQL)

	const restrictedRole = "dbkit_soft_delete_role"
	const restrictedPassword = "dbkit-soft-delete-role-password" // throwaway, disposable container only
	mustExec(t, adminDB, ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, restrictedRole, restrictedPassword))
	mustExec(t, adminDB, ctx, fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, restrictedRole))
	mustExec(t, adminDB, ctx, fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON public.soft_deletable_widgets TO %s`, restrictedRole))
	mustExec(t, adminDB, ctx, `ALTER TABLE public.soft_deletable_widgets ENABLE ROW LEVEL SECURITY`)
	mustExec(t, adminDB, ctx, `CREATE POLICY tenant_isolation ON public.soft_deletable_widgets
		USING (tenant_id = current_setting('app.current_tenant', true))
		WITH CHECK (tenant_id = current_setting('app.current_tenant', true))`)

	restrictedDSN := fmt.Sprintf("postgres://%s:%s@%s:%s/dbkit?sslmode=disable", restrictedRole, restrictedPassword, host, mappedPort.Port())

	db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectPostgres, DSN: restrictedDSN})
	if err != nil {
		t.Fatalf("dbkit.Open(restricted role) error = %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get underlying sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	repo := dbkit.NewRepository[testutil.SoftDeletableWidget](db)

	w := &testutil.SoftDeletableWidget{ID: "rls-sd-a", Name: "a-secret"}
	if err := repo.Create(ctxFor(tenantA), w); err != nil {
		t.Fatalf("Create(tenant-a) through the restricted role error = %v", err)
	}
	if err := repo.Delete(ctxFor(tenantA), w.ID); err != nil {
		t.Fatalf("Delete (soft-delete) through the restricted role error = %v", err)
	}

	// Ground truth via the admin (RLS-exempt) connection: the row is still
	// physically present, with deleted_at now populated -- confirming this
	// really was a mark-delete, not a physical DELETE, before anything else
	// in this test is meaningful.
	t.Run("GroundTruth_AdminConnectionSeesTheSoftDeletedRowStillPresent", func(t *testing.T) {
		var deletedAt sql.NullTime
		if err := adminDB.QueryRowContext(ctx,
			`SELECT deleted_at FROM public.soft_deletable_widgets WHERE id = $1`, w.ID,
		).Scan(&deletedAt); err != nil {
			t.Fatalf("admin SELECT deleted_at: %v", err)
		}
		if !deletedAt.Valid {
			t.Fatal("deleted_at = NULL on the admin connection, want a populated timestamp -- the Delete above did not really soft-delete the row")
		}
	})

	// The real assertion: a raw connection authenticated as the SAME
	// restricted role, with app.current_tenant set to the row's OWNING
	// tenant, sees the soft-deleted row through plain SQL -- RLS filters by
	// tenant_id only, exactly as docs/internal/04-data-and-tenancy.md's
	// "对 PostgreSQL RLS 而言就是普通行" claim says, with no special-casing
	// for deleted_at needed or present in the policy. This is what "no
	// special handling needed" actually means operationally: the row is
	// still reachable through RLS by its rightful tenant at the raw-SQL
	// layer; it is dbkit's Go-side soft-delete auto-scope plugin (query-only,
	// see soft_delete.go), not RLS, whose job is hiding it from ordinary
	// application reads -- proven separately by
	// TestSoftDeleteScopePlugin_Query_AppendsDeletedAtIsNull in the unit tier.
	t.Run("RawConnection_OwningTenantGUC_SeesTheSoftDeletedRowThroughRLS", func(t *testing.T) {
		rawConn := openRole(t, ctx, restrictedDSN)
		mustExec(t, rawConn, ctx, fmt.Sprintf(`SET app.current_tenant = '%s'`, string(tenantA)))
		ids := queryIDs(t, rawConn, ctx, `SELECT id FROM public.soft_deletable_widgets`)
		if len(ids) != 1 || ids[0] != w.ID {
			t.Errorf("raw connection (owning tenant GUC) SELECT * = %v, want exactly [%s] -- RLS must still admit a soft-deleted row to its own tenant", ids, w.ID)
		}
	})

	// The other half of "tenant_id filtering still applies normally": a raw
	// connection with a DIFFERENT tenant's GUC set must see nothing at all
	// for this row, soft-deleted or not -- RLS denying cross-tenant access
	// to a soft-deleted row exactly as it would to a live one.
	t.Run("RawConnection_DifferentTenantGUC_SeesNothing", func(t *testing.T) {
		rawConn := openRole(t, ctx, restrictedDSN)
		mustExec(t, rawConn, ctx, fmt.Sprintf(`SET app.current_tenant = '%s'`, string(tenantB)))
		ids := queryIDs(t, rawConn, ctx, `SELECT id FROM public.soft_deletable_widgets`)
		if len(ids) != 0 {
			t.Errorf("raw connection (tenant-b GUC, tenant-a's soft-deleted row) SELECT * = %v, want no rows -- RLS must deny cross-tenant access to a soft-deleted row exactly as it would a live one", ids)
		}
	})

	// And Repository's own public API, through the normal soft-delete
	// auto-scope plugin AND RLS both engaged: the owning tenant's FindByID
	// must still report the row gone (Go-side hiding), while a cross-tenant
	// FindByID attempt is denied for the ordinary reason (ErrRecordNotFound,
	// collapsed with "no such id" per Repository's own documented contract).
	t.Run("Repository_OwningTenantFindByID_StillHiddenByTheGoSideAutoScope", func(t *testing.T) {
		if _, err := repo.FindByID(ctxFor(tenantA), w.ID); !isRecordNotFound(err) {
			t.Errorf("FindByID(tenant-a, its own soft-deleted row) error = %v, want ErrRecordNotFound", err)
		}
	})
	t.Run("Repository_CrossTenantFindByID_StillDenied", func(t *testing.T) {
		if _, err := repo.FindByID(ctxFor(tenantB), w.ID); !isRecordNotFound(err) {
			t.Errorf("FindByID(tenant-b, tenant-a's soft-deleted row) error = %v, want ErrRecordNotFound", err)
		}
	})
}
