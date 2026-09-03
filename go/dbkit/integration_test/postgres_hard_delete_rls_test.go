//go:build integration

package dbkit_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
)

// hardDeleteRLSTestPurpose is the SystemPurpose the hard-delete RLS tests
// grant themselves, registered from the test process exactly as the unit
// tier's HardDelete tests do (tests exercise the gate, not the grant's
// legitimacy).
const hardDeleteRLSTestPurpose pkgcore.SystemPurpose = "dbkit.test.hard_delete_rls"

// hardDeleteRLSCtx returns a context for tid that passes HardDelete's
// system-context gate: the tenant ctxFor(tid) already carries, an actor, and
// a granted system context whose reason names hardDeleteRLSTestPurpose.
// RegisterSystemPurpose is idempotent and mutex-guarded, so the registration
// here is a no-op from the second grant on.
func hardDeleteRLSCtx(t *testing.T, tid pkgcore.TenantID, actor string) context.Context {
	t.Helper()
	pkgcore.RegisterSystemPurpose(hardDeleteRLSTestPurpose)
	ctx := pkgcore.WithActor(ctxFor(tid), pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: actor})
	elevated, err := pkgcore.WithSystemContext(ctx, pkgcore.SystemReason{
		Actor:   actor,
		Purpose: hardDeleteRLSTestPurpose,
		Ticket:  "dbkit-hard-delete-rls-test",
	})
	if err != nil {
		t.Fatalf("WithSystemContext() error = %v", err)
	}
	return elevated
}

// startHardDeleteRLSDB brings up the restricted-role world the hard-delete
// RLS tests run in, mirroring postgres_soft_delete_rls_test.go's setup
// exactly: one disposable container per test, the soft_deletable_widgets
// table created by an RLS-exempt admin connection, a restricted role granted
// SELECT, INSERT, UPDATE, DELETE on it (no BYPASSRLS anywhere), the
// tenant_isolation policy keyed on app.current_tenant — the session GUC
// WithTenantSession sets from the ctx tenant — and a single-connection
// dbkit.Open session as that restricted role for Repository to run through.
// It returns the admin connection (ground truth, plus the DDL/GRANT
// authority), the restricted role's DSN (for raw connections), and the
// repository bound to the restricted session. Two tests share this helper;
// each of them gets its own container.
func startHardDeleteRLSDB(t *testing.T, ctx context.Context) (*sql.DB, string, *dbkit.Repository[testutil.SoftDeletableWidget]) {
	t.Helper()
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

	const restrictedRole = "dbkit_hard_delete_role"
	const restrictedPassword = "dbkit-hard-delete-role-password" // throwaway, disposable container only
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

	return adminDB, restrictedDSN, dbkit.NewRepository[testutil.SoftDeletableWidget](db)
}

// adminSoftDeletableWidgetState reads id's raw row state through the admin
// connection — RLS-exempt by superuser ownership, so it is the ground truth
// no restricted-role session can see past the policy. found reports whether
// the row physically exists at all; when it does, deletedAt is the row's
// deleted_at value, nil for a live row.
func adminSoftDeletableWidgetState(t *testing.T, adminDB *sql.DB, ctx context.Context, id string) (found bool, deletedAt *time.Time) {
	t.Helper()
	var nullDeletedAt sql.NullTime
	err := adminDB.QueryRowContext(ctx,
		`SELECT deleted_at FROM public.soft_deletable_widgets WHERE id = $1`, id,
	).Scan(&nullDeletedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		t.Fatalf("admin SELECT deleted_at for id %q: %v", id, err)
	}
	if !nullDeletedAt.Valid {
		return true, nil
	}
	return true, &nullDeletedAt.Time
}

// rawDeleteThroughRLS issues a physical DELETE for id on conn with no
// tenant_id WHERE clause of its own — deliberately bypassing every Go-side
// filter so that PostgreSQL RLS is the only layer between the statement and
// the row — and returns the number of rows affected. It exists purely as the
// negative control that isolates RLS as the denying (or admitting) layer.
func rawDeleteThroughRLS(t *testing.T, conn *sql.DB, ctx context.Context, id string) int64 {
	t.Helper()
	res, err := conn.ExecContext(ctx, `DELETE FROM public.soft_deletable_widgets WHERE id = $1`, id)
	if err != nil {
		t.Fatalf("raw DELETE for id %q: %v", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("raw DELETE RowsAffected() for id %q: %v", id, err)
	}
	return affected
}

// TestRepository_HardDelete_RLS_TenantScopedPhysicalDeleteAllowed is the RLS
// leg of HardDelete's proof, per docs/internal/04-data-and-tenancy.md's
// delete-semantics section (§4): the physical DELETE HardDelete issues is, to
// PostgreSQL Row-Level Security, just an ordinary tenant-scoped DELETE — no
// new policy, no BYPASSRLS, nothing special-casing it — and it therefore
// passes through the same tenant_isolation policy every other statement on
// the table passes through. The system context riding in the caller's ctx
// contributes nothing at the database layer: RLS sees only app.current_tenant,
// the session GUC WithTenantSession sets from the ctx tenant inside
// HardDelete's own transaction, and the restricted role here is NOT
// BYPASSRLS, so nothing but that tenant match can admit the statement. The
// test drives Repository's public API — Create, Delete (mark-delete), then
// HardDelete — through the restricted role's single-connection dbkit.Open
// session, with the RLS-exempt admin connection supplying ground truth and
// a raw restricted-role connection proving what the policy alone still
// admits after the row is gone.
func TestRepository_HardDelete_RLS_TenantScopedPhysicalDeleteAllowed(t *testing.T) {
	ctx := context.Background()
	adminDB, restrictedDSN, repo := startHardDeleteRLSDB(t, ctx)

	// A live row — never soft-deleted — physically erased through the
	// restricted role: RLS must admit the owning tenant's DELETE, and the
	// row must be gone not merely marked.
	t.Run("LiveRow_HardDeleteThroughRestrictedRole_PhysicallyRemovesIt", func(t *testing.T) {
		w := &testutil.SoftDeletableWidget{ID: "rls-hd-live-1", Name: "a-live-secret"}
		if err := repo.Create(ctxFor(tenantA), w); err != nil {
			t.Fatalf("Create(tenant-a) through the restricted role error = %v", err)
		}
		if found, deletedAt := adminSoftDeletableWidgetState(t, adminDB, ctx, w.ID); !found || deletedAt != nil {
			t.Fatalf("admin pre-check: found = %v, deletedAt = %v, want a live row physically present", found, deletedAt)
		}

		if err := repo.HardDelete(hardDeleteRLSCtx(t, tenantA, "retention-job"), w.ID); err != nil {
			t.Fatalf("HardDelete() through the restricted role error = %v, want nil -- RLS must admit the owning tenant's physical DELETE", err)
		}
		if found, _ := adminSoftDeletableWidgetState(t, adminDB, ctx, w.ID); found {
			t.Fatal("row still physically present after HardDelete, per the RLS-exempt admin connection -- the DELETE must have really erased it")
		}

		// The restricted role's own session, GUC set to the owning tenant,
		// sees nothing either: the row is gone at the database layer, not
		// merely hidden behind Go-side filtering.
		rawConn := openRole(t, ctx, restrictedDSN)
		mustExec(t, rawConn, ctx, fmt.Sprintf(`SET app.current_tenant = '%s'`, string(tenantA)))
		if ids := queryIDs(t, rawConn, ctx, `SELECT id FROM public.soft_deletable_widgets`); len(ids) != 0 {
			t.Errorf("restricted role (owning tenant GUC) still sees %v after HardDelete, want no rows", ids)
		}
	})

	// The soft-deleted-row flavor: a row that Delete() first marked (its
	// deleted_at set, proven through the admin connection) is just as
	// deletable as a live one — the mark is not a boundary — and the
	// physical DELETE passes through RLS exactly the same way.
	t.Run("SoftDeletedRow_HardDeleteThroughRestrictedRole_PhysicallyErasesIt", func(t *testing.T) {
		w := &testutil.SoftDeletableWidget{ID: "rls-hd-soft-1", Name: "a-soft-secret"}
		if err := repo.Create(ctxFor(tenantA), w); err != nil {
			t.Fatalf("Create(tenant-a) through the restricted role error = %v", err)
		}
		if err := repo.Delete(ctxFor(tenantA), w.ID); err != nil {
			t.Fatalf("Delete (mark-delete) through the restricted role error = %v", err)
		}
		found, deletedAt := adminSoftDeletableWidgetState(t, adminDB, ctx, w.ID)
		if !found || deletedAt == nil {
			t.Fatalf("admin pre-check after Delete(): found = %v, deletedAt = %v, want the row still present with deleted_at set (a mark-delete, not a physical one)", found, deletedAt)
		}

		if err := repo.HardDelete(hardDeleteRLSCtx(t, tenantA, "retention-job"), w.ID); err != nil {
			t.Fatalf("HardDelete() of a soft-deleted row through the restricted role error = %v", err)
		}
		if found, _ := adminSoftDeletableWidgetState(t, adminDB, ctx, w.ID); found {
			t.Fatal("soft-deleted row still physically present after HardDelete, per the RLS-exempt admin connection")
		}

		rawConn := openRole(t, ctx, restrictedDSN)
		mustExec(t, rawConn, ctx, fmt.Sprintf(`SET app.current_tenant = '%s'`, string(tenantA)))
		if ids := queryIDs(t, rawConn, ctx, `SELECT id FROM public.soft_deletable_widgets`); len(ids) != 0 {
			t.Errorf("restricted role (owning tenant GUC) still sees %v after HardDelete of a soft-deleted row, want no rows", ids)
		}
	})
}

// TestRepository_HardDelete_RLS_CrossTenant_SystemContextGetsNotFoundNoLeak
// pins the cross-tenant half of HardDelete against real PostgreSQL RLS: even
// a granted system context never makes HardDelete a cross-tenant eraser. The
// Repository resolves the ctx tenant (tenant-b here), and WithTenantSession
// sets app.current_tenant to tenant-b inside the transaction, so the
// tenant_isolation policy hides tenant-a's row from the DELETE entirely —
// RLS is what denies it, the Repository reports ErrRecordNotFound exactly as
// it reports a never-created id, and the row stays physically intact. The
// strongest leg is the raw-SQL negative control: a DELETE carrying no tenant
// clause at all — every Go-side filter bypassed — affects zero rows from
// tenant-b's session, isolating RLS as the denying layer, while the mirror
// image, the owning tenant's own raw DELETE, succeeds through the policy
// with no other layer engaged.
func TestRepository_HardDelete_RLS_CrossTenant_SystemContextGetsNotFoundNoLeak(t *testing.T) {
	ctx := context.Background()
	adminDB, restrictedDSN, repo := startHardDeleteRLSDB(t, ctx)

	wA := &testutil.SoftDeletableWidget{ID: "rls-hd-x1", Name: "a-secret"}
	if err := repo.Create(ctxFor(tenantA), wA); err != nil {
		t.Fatalf("Create(tenant-a) of %q error = %v", wA.ID, err)
	}
	wB := &testutil.SoftDeletableWidget{ID: "rls-hd-x2", Name: "b-secret"}
	if err := repo.Create(ctxFor(tenantA), wB); err != nil {
		t.Fatalf("Create(tenant-a) of %q error = %v", wB.ID, err)
	}

	// Repository level: a system context granted to a tenant-b session
	// cannot erase tenant-a's row. RLS admits only tenant-b's rows to the
	// DELETE, so nothing matches and ErrRecordNotFound comes back — the same
	// error a never-created id would produce, so the attempt leaks nothing.
	t.Run("Repository_SystemContextFromTenantB_GetsRecordNotFoundAndLeavesTheRow", func(t *testing.T) {
		err := repo.HardDelete(hardDeleteRLSCtx(t, tenantB, "platform-admin"), wA.ID)
		if !isRecordNotFound(err) {
			t.Fatalf("HardDelete() of tenant-a's row from a tenant-b system context error = %v, want ErrRecordNotFound", err)
		}
		found, deletedAt := adminSoftDeletableWidgetState(t, adminDB, ctx, wA.ID)
		if !found || deletedAt != nil {
			t.Fatalf("tenant-a's row after tenant-b's system-context HardDelete attempt: found = %v, deletedAt = %v, want it live and untouched", found, deletedAt)
		}
	})

	// Raw negative control: with every Go-side filter bypassed — no
	// tenant_id clause at all — tenant-b's session still cannot delete
	// tenant-a's row. Zero rows affected, proven against the admin ground
	// truth afterwards: the denial is PostgreSQL RLS's own, not a side
	// effect of Repository's WHERE clause.
	t.Run("RawConnection_CrossTenantGUC_PhysicalDeleteDeniedByRLS", func(t *testing.T) {
		rawConn := openRole(t, ctx, restrictedDSN)
		mustExec(t, rawConn, ctx, fmt.Sprintf(`SET app.current_tenant = '%s'`, string(tenantB)))
		if affected := rawDeleteThroughRLS(t, rawConn, ctx, wA.ID); affected != 0 {
			t.Fatalf("raw DELETE from tenant-b's GUC affected %d rows, want 0 -- RLS must deny the cross-tenant physical delete", affected)
		}
		if found, _ := adminSoftDeletableWidgetState(t, adminDB, ctx, wA.ID); !found {
			t.Fatal("tenant-a's row vanished under tenant-b's raw DELETE; RLS did not deny it")
		}
	})

	// The mirror image, proving the policy is what both sides hinge on: the
	// owning tenant's own raw DELETE — same SQL shape, no tenant clause —
	// affects exactly one row. RLS admits the owning tenant's physical
	// delete with no Go-side layer engaged at all.
	t.Run("RawConnection_OwningTenantGUC_PhysicalDeleteAllowedByRLS", func(t *testing.T) {
		rawConn := openRole(t, ctx, restrictedDSN)
		mustExec(t, rawConn, ctx, fmt.Sprintf(`SET app.current_tenant = '%s'`, string(tenantA)))
		if affected := rawDeleteThroughRLS(t, rawConn, ctx, wB.ID); affected != 1 {
			t.Fatalf("raw DELETE from the owning tenant's GUC affected %d rows, want 1 -- RLS must pass the owning tenant's physical DELETE through", affected)
		}
		if found, _ := adminSoftDeletableWidgetState(t, adminDB, ctx, wB.ID); found {
			t.Fatal("tenant-a's second row still present after the owning tenant's raw DELETE; RLS did not admit it")
		}
	})
}
