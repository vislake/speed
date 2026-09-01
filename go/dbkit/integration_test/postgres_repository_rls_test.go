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

// TestRepository_PostgresRLS_RestrictedRoleEnforcesIsolation closes the one
// gap the two other RLS integration tests in this package leave open by
// design, as their own doc comments say explicitly:
//
//   - postgres_rls_test.go proves the database mechanism works at all, with
//     raw SQL and a hand-set session variable — no dbkit code involved.
//   - postgres_tenant_session_test.go proves dbkit.WithTenantSession's own
//     GUC step is what a real policy responds to — but it calls
//     WithTenantSession directly against a bare "probe" table via
//     tx.Table(...), never through dbkit.Repository[T], and never against
//     the widgets fixture.
//
// Neither exercises the thing production code actually calls: a business
// module's dbkit.Repository[T], built on a connection opened the one
// sanctioned way (dbkit.Open), talking to the widgets fixture table with a
// real, restricted (non-BYPASSRLS, non-superuser) application role — the
// exact shape docs/internal/04-data-and-tenancy.md describes — and RLS
// actually enabled underneath it. This test drives every assertion through
// Repository's own public methods (Create, FindByID, List) and nothing
// lower-level, so a regression that quietly disconnected WithTenantSession
// from Repository (or from Open) would show up here even if
// postgres_tenant_session_test.go kept passing.
//
// The independent, dbkit-free half of the proof is the
// "RawConnection_NoGUCSetAtAll" subtest below: a second connection,
// authenticated as the SAME restricted role, that never sets
// app.current_tenant at all. If RLS were not really enabled on this table,
// or if the role could bypass it, that connection would see both seeded
// rows. Seeing zero is the database defaulting to deny, independent of
// anything dbkit does — the baseline that makes Repository's own results
// (seeing exactly its own tenant's row, no more and no less) mean what they
// appear to mean.
//
// sqlDB.SetMaxOpenConns(1) is set once, immediately after dbkit.Open and
// before a single query runs (including the seeding Creates below), so
// every Repository call for the rest of this test — across every
// subtest — is forced through one single physical connection. That
// deliberately doubles this test as the pool-leakage check the backend
// coding standard's testing rules call for: WithTenantSession's session GUC
// is set with is_local = true specifically so it reverts at COMMIT instead
// of surviving on a pooled connection into a later, unrelated request (see
// tenant_session.go's doc comment); pinning every call in this test to one
// connection means a leak would surface immediately as tenant B's calls
// seeing tenant A's row (or nothing at all), not as a rare flake under
// concurrent load. See also
// postgres_tenant_session_test.go's own
// "SettingDoesNotLeakAcrossCallsOnAPooledConnection" subtest, which proves
// the identical property one layer lower (WithTenantSession called
// directly); this test proves it again from Repository's public API, one
// layer up, where a real caller actually sits.
func TestRepository_PostgresRLS_RestrictedRoleEnforcesIsolation(t *testing.T) {
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

	// The widgets fixture, unqualified, directly in the default "public"
	// schema. dbkit.Repository[T] issues queries against T's bare GORM
	// table name with no schema qualifier of its own, so — unlike
	// openWidgetDB's dedicated schema in postgres_tenant_isolation_test.go,
	// which needs a session-scoped SET search_path and therefore pins the
	// table-owner connection to a single pooled connection just to create
	// the schema — the restricted role below needs "widgets" reachable
	// through its ordinary default search_path with no extra wiring at
	// connection-open time. "public" is safe to use unqualified here
	// because startPostgresContainer gives this test its own single-use
	// container that nothing else ever shares.
	mustExec(t, adminDB, ctx, testutil.WidgetPostgresMigrationSQL(t))

	const restrictedRole = "dbkit_app_role"
	const restrictedPassword = "dbkit-app-role-password" // throwaway, disposable container only
	mustExec(t, adminDB, ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, restrictedRole, restrictedPassword))
	mustExec(t, adminDB, ctx, fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, restrictedRole))
	mustExec(t, adminDB, ctx, fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON public.widgets TO %s`, restrictedRole))
	mustExec(t, adminDB, ctx, `ALTER TABLE public.widgets ENABLE ROW LEVEL SECURITY`)
	mustExec(t, adminDB, ctx, `CREATE POLICY tenant_isolation ON public.widgets
		USING (tenant_id = current_setting('app.current_tenant', true))
		WITH CHECK (tenant_id = current_setting('app.current_tenant', true))`)

	// Sanity check on the role itself: confirm it is genuinely not a
	// superuser and does not carry BYPASSRLS, the two attributes that would
	// make every assertion below pass for the wrong reason — the role
	// ignoring RLS altogether, rather than genuinely being subject to the
	// policy and satisfying it via the GUC.
	t.Run("Setup_RestrictedRoleIsNotSuperuserOrBypassRLS", func(t *testing.T) {
		var rolsuper, rolbypassrls bool
		if queryErr := adminDB.QueryRowContext(ctx,
			`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = $1`, restrictedRole,
		).Scan(&rolsuper, &rolbypassrls); queryErr != nil {
			t.Fatalf("query pg_roles for %s: %v", restrictedRole, queryErr)
		}
		if rolsuper || rolbypassrls {
			t.Fatalf("restricted role %s has rolsuper=%v rolbypassrls=%v, want both false — this test proves nothing if the role can bypass RLS", restrictedRole, rolsuper, rolbypassrls)
		}
	})

	restrictedDSN := fmt.Sprintf("postgres://%s:%s@%s:%s/dbkit?sslmode=disable", restrictedRole, restrictedPassword, host, mappedPort.Port())

	// This is the exact call sequence every real business module makes:
	// dbkit.Open followed by dbkit.NewRepository[T]. The only thing
	// unusual versus production is that the DSN authenticates as a role
	// this test itself just created and locked down — production's
	// equivalent role and policy are operated independently of dbkit (see
	// repository.go's doc comment: "dbkit supplies the session-variable
	// wiring, not the role or the policy").
	db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectPostgres, DSN: restrictedDSN})
	if err != nil {
		t.Fatalf("dbkit.Open(restricted role) error = %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get underlying sql.DB: %v", err)
	}
	// See the function doc comment above for why this is set here, before
	// any query at all, rather than scoped to one subtest.
	sqlDB.SetMaxOpenConns(1)

	repo := dbkit.NewRepository[testutil.Widget](db)

	// Seed one row per tenant through Repository.Create itself, not admin
	// raw SQL, so the INSERT / WITH CHECK half of the policy is exercised
	// by the real code path too, not only the SELECT / USING half.
	widgetA := &testutil.Widget{ID: "rls-repo-a", Name: "a-secret", Value: 1}
	if err := repo.Create(ctxFor(tenantA), widgetA); err != nil {
		t.Fatalf("Create(tenant-a) through the restricted role error = %v", err)
	}
	widgetB := &testutil.Widget{ID: "rls-repo-b", Name: "b-secret", Value: 2}
	if err := repo.Create(ctxFor(tenantB), widgetB); err != nil {
		t.Fatalf("Create(tenant-b) through the restricted role error = %v", err)
	}

	// Ground truth, via the admin (superuser, RLS-exempt) connection:
	// confirm both rows genuinely landed in the table. Without this check,
	// the "raw restricted connection sees zero rows" subtest below would
	// also trivially pass if Create had silently inserted nothing at all,
	// which would prove nothing about RLS.
	t.Run("GroundTruth_AdminConnectionSeesBothSeededRows", func(t *testing.T) {
		ids := queryIDs(t, adminDB, ctx, `SELECT id FROM public.widgets ORDER BY id`)
		if len(ids) != 2 || ids[0] != widgetA.ID || ids[1] != widgetB.ID {
			t.Fatalf("admin SELECT * = %v, want exactly [%s %s] — the seeding Creates above did not really persist both rows", ids, widgetA.ID, widgetB.ID)
		}
	})

	// The independent, dbkit-free half of the proof: a separate connection,
	// bypassing dbkit entirely (raw database/sql over pgx, no gorm),
	// authenticated as the SAME restricted role, with app.current_tenant
	// never set on it at all. Two rows genuinely exist (just confirmed
	// above); if RLS were not really enabled on this table, or if this
	// role could bypass it, this connection would see both. Seeing zero is
	// what "RLS default-denies" means at the database level, independent of
	// anything dbkit does or does not do correctly.
	t.Run("RawConnection_NoGUCSetAtAll_SeesNothing_DefaultDeny", func(t *testing.T) {
		rawConn := openRole(t, ctx, restrictedDSN)
		ids := queryIDs(t, rawConn, ctx, `SELECT id FROM public.widgets`) // deliberately no WHERE clause
		if len(ids) != 0 {
			t.Errorf("raw connection (bypassing dbkit, restricted role, app.current_tenant never set) SELECT * = %v, want no rows — RLS must default-deny, not default-allow, when no tenant GUC is set", ids)
		}
	})

	// The real assertion: Repository[T]'s own public methods, called
	// exactly as a business module would, correctly see only their own
	// tenant's row — proving WithTenantSession's GUC step is genuinely
	// reached from Create/FindByID/List through the normal API, not merely
	// present somewhere in dbkit's source.
	t.Run("Repository_TenantA_SeesOnlyItsOwnRowThroughRLS", func(t *testing.T) {
		got, err := repo.FindByID(ctxFor(tenantA), widgetA.ID)
		if err != nil {
			t.Fatalf("FindByID(tenant-a, own row) error = %v", err)
		}
		if got.ID != widgetA.ID {
			t.Errorf("FindByID(tenant-a, own row) = %+v, want ID %q", got, widgetA.ID)
		}

		list, err := repo.List(ctxFor(tenantA))
		if err != nil {
			t.Fatalf("List(tenant-a) error = %v", err)
		}
		if len(list) != 1 || list[0].ID != widgetA.ID {
			t.Errorf("List(tenant-a) = %+v, want exactly [%s] (RLS, engaged by Repository through WithTenantSession, must hide tenant B's row even though the Go-side plugin and Repository's own WHERE clause already would)", list, widgetA.ID)
		}
	})

	// Immediately follows tenant A's subtest above, on the same pinned
	// single physical connection (sqlDB.SetMaxOpenConns(1) above): if
	// tenant A's GUC had leaked past its COMMIT, tenant B's List here would
	// incorrectly include widget A, or the policy would incorrectly still
	// be scoped to tenant A and hide widget B instead.
	t.Run("Repository_TenantB_SeesOnlyItsOwnRowThroughRLS_NoLeakageFromTenantAOnTheSamePooledConnection", func(t *testing.T) {
		list, err := repo.List(ctxFor(tenantB))
		if err != nil {
			t.Fatalf("List(tenant-b) error = %v", err)
		}
		if len(list) != 1 || list[0].ID != widgetB.ID {
			t.Errorf("List(tenant-b), right after tenant A's calls on the same pooled connection, = %+v, want exactly [%s] (a leaked GUC would show widget-a here too, or in place of widget-b)", list, widgetB.ID)
		}
	})

	// A cross-tenant FindByID through Repository's own public API. Layers 1
	// and 2 (the Go-side plugin and Repository's own WHERE tenant_id = ?)
	// already guarantee ErrRecordNotFound here on their own (see
	// postgres_tenant_isolation_test.go's CrossTenantRead_Denied); this
	// subtest is not re-proving that in isolation. It exists so the full
	// picture is on record in one file: RLS is independently enforced (the
	// raw-connection subtest above) AND Repository's normal calls still
	// behave correctly, end to end, with RLS turned on underneath them.
	t.Run("Repository_CrossTenantFindByID_StillDeniedWithRLSEnabled", func(t *testing.T) {
		got, err := repo.FindByID(ctxFor(tenantA), widgetB.ID)
		if got != nil {
			t.Errorf("FindByID(tenant-a, tenant-b's id) = %+v, want nil", got)
		}
		if !isRecordNotFound(err) {
			t.Errorf("FindByID(tenant-a, tenant-b's id) error = %v, want ErrRecordNotFound", err)
		}
	})

	// Explicit, repeated alternation on the pinned single connection,
	// mirroring postgres_tenant_session_test.go's
	// "SettingDoesNotLeakAcrossCallsOnAPooledConnection" one layer up
	// (through Repository.List instead of WithTenantSession + tx.Table).
	// Three round trips rather than one: is_local = true reverting exactly
	// at COMMIT should make every single alternation clean, so a flake
	// appearing only on, say, the third round trip would itself be a
	// meaningful (and different) signal from a leak that shows up
	// immediately.
	t.Run("Repository_RepeatedAlternationOnOnePooledConnection_NeverLeaks", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			listA, err := repo.List(ctxFor(tenantA))
			if err != nil {
				t.Fatalf("round %d: List(tenant-a) error = %v", i, err)
			}
			if len(listA) != 1 || listA[0].ID != widgetA.ID {
				t.Errorf("round %d: List(tenant-a) = %+v, want exactly [%s]", i, listA, widgetA.ID)
			}

			listB, err := repo.List(ctxFor(tenantB))
			if err != nil {
				t.Fatalf("round %d: List(tenant-b) error = %v", i, err)
			}
			if len(listB) != 1 || listB[0].ID != widgetB.ID {
				t.Errorf("round %d: List(tenant-b) = %+v, want exactly [%s]", i, listB, widgetB.ID)
			}
		}
	})
}
