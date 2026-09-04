//go:build integration

// Package dbkit_test holds dbkit's integration tier: tests that exercise a
// real external dependency instead of SQLite or a plugin-less connection.
// It is physically separate from dbkit's unit tests (repository_test.go,
// tenant_scope_test.go, and friends, all in package dbkit itself) and
// carries the "integration" build tag, per the backend coding standard's
// testing layout rule (section 13) and docs/internal/20-quality-and-security.md:
// a plain "go test ./..." never compiles or runs anything in this
// directory; it is invoked explicitly with "go test -tags=integration ./...".
//
// Every test here spins up its own disposable PostgreSQL container via
// testcontainers-go (github.com/testcontainers/testcontainers-go/modules/postgres)
// and requires a working Docker (or Docker-API-compatible) daemon; there is
// no fallback or skip-on-missing-Docker path, unlike the unit tier's
// opportunistic local-Postgres probe in migrations_test.go — this package
// IS the "authoritative Postgres check" that unit-tier file's own comment
// defers to.
package dbkit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	// Blank-imported for its init side effect: registers
	// dbkit.DialectPostgres so every dbkit.Open(DialectPostgres) call in
	// this package's test binary (this file and its siblings under
	// integration_test/) has a driver to build from. One package-wide
	// import suffices, since go test links every file in this directory
	// into a single binary.
	_ "github.com/vislake/speed/go/dbkit/dialect/postgres"
	"github.com/vislake/speed/go/dbkit/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// tenantA and tenantB are the two tenants every isolation scenario in this
// file exercises, matching the naming already established by
// tenant_scope_test.go in the parent package.
const (
	tenantA = pkgcore.TenantID("tenant-a")
	tenantB = pkgcore.TenantID("tenant-b")
)

// ctxFor returns a context carrying tid as the current tenant.
func ctxFor(tid pkgcore.TenantID) context.Context {
	return pkgcore.WithTenant(context.Background(), tid)
}

// isRecordNotFound reports whether err is dbkit.ErrRecordNotFound, matched
// by Code rather than identity (apperr.WithParam always returns a new
// *apperr.Error; see dbkit's own repository_test.go for the same pattern).
func isRecordNotFound(err error) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == dbkit.ErrRecordNotFound.Code
}

// startPostgresContainer starts a disposable PostgreSQL 16 container and
// returns it, already terminated via t.Cleanup on test completion (pass or
// fail), so no container ever leaks past its owning test.
func startPostgresContainer(t *testing.T, ctx context.Context) *postgres.PostgresContainer {
	t.Helper()

	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("dbkit"),
		postgres.WithUsername("dbkit"),
		postgres.WithPassword("dbkit"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(pgContainer); err != nil {
			t.Errorf("terminate postgres testcontainer: %v", err)
		}
	})
	return pgContainer
}

// openWidgetDB opens dsn through dbkit.Open with DialectPostgres — the same
// entry point every real caller in this codebase is required to use (see
// open.go's doc comment: "the ONLY sanctioned way to obtain a *gorm.DB") —
// inside its own freshly created schema (so this test can never collide
// with another test or with a long-lived database), and applies the widgets
// fixture migration used throughout dbkit's SQLite unit tests
// (internal/testutil.NewTestSQLite), so both dialects are provably running
// the exact same logical schema.
//
// The returned *gorm.DB is pinned to a single pooled connection: search_path
// is session-scoped, and a second pooled connection opened later by the pool
// would silently not see it, putting later statements back on "public" —
// the same reasoning dbkit's own migrations_test.go documents for its
// equivalent helper.
func openWidgetDB(t *testing.T, ctx context.Context, dsn string) *gorm.DB {
	t.Helper()

	db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectPostgres, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open(DialectPostgres) error = %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get underlying sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	schema := "dbkit_it_widgets"
	if err := db.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if err := db.Exec("DROP SCHEMA " + schema + " CASCADE").Error; err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
	})
	if err := db.Exec("SET search_path TO " + schema).Error; err != nil {
		t.Fatalf("set search_path to %s: %v", schema, err)
	}

	if err := db.Exec(testutil.WidgetPostgresMigrationSQL(t)).Error; err != nil {
		t.Fatalf("apply widgets fixture migration: %v", err)
	}

	return db
}

// openIDAndTenantOnlyMarkerDB mirrors openWidgetDB above — same
// dbkit.Open entry point, own freshly created schema so this test can
// never collide with another, connection pinned to a single pooled
// connection for the same search_path reason — but creates
// testutil.IDAndTenantOnlyMarker's own minimal table by hand instead of
// applying the widgets fixture migration, since
// testutil.IDAndTenantOnlyMarker is not part of that fixture. That type
// mirrors dbkit's own unit-test fixture (repository_test.go, parent
// package) — the two are in fact the same shared testutil type, not just
// look-alikes — so this file's Postgres-backed test can reproduce the
// exact Update-on-an-ID+TenantID-only-model bug against a real PostgreSQL
// server, not just SQLite. See
// TestPostgresRepository_Update_IDAndTenantIDOnlyModel_SucceedsAsNoOp's own
// doc comment below, and Update's doc comment in repository.go, for the
// full explanation.
func openIDAndTenantOnlyMarkerDB(t *testing.T, ctx context.Context, dsn string) *gorm.DB {
	t.Helper()

	db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectPostgres, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open(DialectPostgres) error = %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get underlying sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	schema := "dbkit_it_id_tenant_only_marker"
	if err := db.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if err := db.Exec("DROP SCHEMA " + schema + " CASCADE").Error; err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
	})
	if err := db.Exec("SET search_path TO " + schema).Error; err != nil {
		t.Fatalf("set search_path to %s: %v", schema, err)
	}

	if err := db.Exec(testutil.IDAndTenantOnlyMarkerTableSQL).Error; err != nil {
		t.Fatalf("create id_and_tenant_only_markers table: %v", err)
	}

	return db
}

// TestPostgresRepository_Update_IDAndTenantIDOnlyModel_SucceedsAsNoOp is the
// PostgreSQL counterpart of dbkit's own
// TestRepository_Update_IDAndTenantIDOnlyModel_SucceedsAsNoOp
// (repository_test.go, parent package): the regression test for the bug
// where Update on a TenantScoped model whose only fields are ID and
// TenantID silently returned dbkit.ErrRecordNotFound even though the row
// genuinely existed and was genuinely owned by the calling tenant. Root
// cause: gorm's Update callback computes its SET clause by excluding
// every primary-key column, so when T has no non-key column at all that
// clause comes back empty and gorm's own callback returns before
// executing any SQL, leaving RowsAffected at its zero value regardless of
// whether the row exists — confirmed identical on SQLite and PostgreSQL.
// This test proves the fix holds against a real PostgreSQL server, with
// PostgreSQL's own wire protocol, placeholder syntax, and
// error-translation path, not just SQLite's — the same reason
// TestPostgresTenantIsolation above exists alongside the parent package's
// SQLite-only isolation tests. Its DifferentTenant_ReturnsNotFound subtest
// proves the fix does not weaken isolation: a cross-tenant Update against
// the same shape of model must still collapse to ErrRecordNotFound.
func TestPostgresRepository_Update_IDAndTenantIDOnlyModel_SucceedsAsNoOp(t *testing.T) {
	ctx := context.Background()
	pgContainer := startPostgresContainer(t, ctx)
	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres testcontainer connection string: %v", err)
	}

	db := openIDAndTenantOnlyMarkerDB(t, ctx, dsn)
	repo := dbkit.NewRepository[testutil.IDAndTenantOnlyMarker](db)

	t.Run("SameTenant_SucceedsAsNoOp", func(t *testing.T) {
		m := &testutil.IDAndTenantOnlyMarker{ID: "pg-marker-1"}
		if err := repo.Create(ctxFor(tenantA), m); err != nil {
			t.Fatalf("Create() error = %v", err)
		}

		got, err := repo.FindByID(ctxFor(tenantA), m.ID)
		if err != nil {
			t.Fatalf("FindByID() after Create error = %v — the row must genuinely exist and be owned by tenant-a", err)
		}

		if err := repo.Update(ctxFor(tenantA), got); err != nil {
			t.Errorf("Update() on an existing, tenant-owned, ID+TenantID-only row error = %v, want nil", err)
		}

		if _, err := repo.FindByID(ctxFor(tenantA), m.ID); err != nil {
			t.Errorf("FindByID() after the no-op Update error = %v, want the row still present", err)
		}
	})

	t.Run("DifferentTenant_ReturnsNotFound", func(t *testing.T) {
		if err := repo.Create(ctxFor(tenantA), &testutil.IDAndTenantOnlyMarker{ID: "pg-marker-2"}); err != nil {
			t.Fatalf("Create() error = %v", err)
		}

		if err := repo.Update(ctxFor(tenantB), &testutil.IDAndTenantOnlyMarker{ID: "pg-marker-2"}); !isRecordNotFound(err) {
			t.Fatalf("Update() from a different tenant error = %v, want ErrRecordNotFound", err)
		}

		if _, err := repo.FindByID(ctxFor(tenantA), "pg-marker-2"); err != nil {
			t.Errorf("FindByID() by the real owner after the failed cross-tenant Update error = %v, want the row still present", err)
		}
		if _, err := repo.FindByID(ctxFor(tenantB), "pg-marker-2"); !isRecordNotFound(err) {
			t.Errorf("FindByID() under the attacking tenant after the failed Update error = %v, want ErrRecordNotFound (no phantom row)", err)
		}
	})
}

// TestPostgresTenantIsolation runs dbkit's core tenant-isolation scenario —
// two tenants, cross-tenant read/write/delete attempts denied — against a
// REAL PostgreSQL server instead of SQLite. Every one of dbkit's own
// isolation tests in the parent package (repository_test.go,
// tenant_scope_test.go) runs exclusively against SQLite (via
// internal/testutil.NewTestSQLite); this is the only place in the module
// that proves the exact same guarantees hold when the wire protocol,
// placeholder syntax ($1 vs ?), and error-translation path
// (postgres.Open's gorm.ErrorTranslator) are PostgreSQL's real ones instead
// of SQLite's.
func TestPostgresTenantIsolation(t *testing.T) {
	ctx := context.Background()
	pgContainer := startPostgresContainer(t, ctx)
	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres testcontainer connection string: %v", err)
	}

	db := openWidgetDB(t, ctx, dsn)
	repo := dbkit.NewRepository[testutil.Widget](db)

	t.Run("RepositoryCRUDLifecycle_SingleTenant", func(t *testing.T) {
		widget := &testutil.Widget{ID: "pg-crud-1", Name: "gadget", Value: 1}
		if err := repo.Create(ctxFor(tenantA), widget); err != nil {
			t.Fatalf("Create() error = %v", err)
		}

		got, err := repo.FindByID(ctxFor(tenantA), widget.ID)
		if err != nil {
			t.Fatalf("FindByID() after Create error = %v", err)
		}
		if got.Name != "gadget" || got.Value != 1 || got.TenantID != string(tenantA) {
			t.Errorf("FindByID() after Create = %+v, want {Name:gadget Value:1 TenantID:%s}", *got, tenantA)
		}

		got.Value = 2
		if err = repo.Update(ctxFor(tenantA), got); err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		updated, err := repo.FindByID(ctxFor(tenantA), widget.ID)
		if err != nil || updated.Value != 2 {
			t.Errorf("FindByID() after Update = %+v, err=%v, want Value=2", updated, err)
		}

		if err := repo.Delete(ctxFor(tenantA), widget.ID); err != nil {
			t.Fatalf("Delete() error = %v", err)
		}
		if _, err := repo.FindByID(ctxFor(tenantA), widget.ID); !isRecordNotFound(err) {
			t.Errorf("FindByID() after Delete error = %v, want ErrRecordNotFound", err)
		}
	})

	t.Run("CrossTenantRead_Denied", func(t *testing.T) {
		widget := &testutil.Widget{ID: "pg-cross-read-1", Name: "a-secret", Value: 1}
		if err := repo.Create(ctxFor(tenantA), widget); err != nil {
			t.Fatalf("Create() error = %v", err)
		}

		got, err := repo.FindByID(ctxFor(tenantB), widget.ID)
		if got != nil {
			t.Errorf("FindByID() from tenant B returned a row: %+v, want nil", got)
		}
		if !isRecordNotFound(err) {
			t.Errorf("FindByID() from tenant B error = %v, want ErrRecordNotFound", err)
		}

		// Layer 1 directly: the tenant-scoping plugin itself (not
		// Repository), exercised straight against the real PostgreSQL
		// connection dbkit.Open returned.
		var raw []testutil.Widget
		if err := db.WithContext(ctxFor(tenantB)).Where("id = ?", widget.ID).Find(&raw).Error; err != nil {
			t.Fatalf("raw plugin-scoped Find() as tenant B error = %v", err)
		}
		if len(raw) != 0 {
			t.Errorf("raw plugin-scoped Find() as tenant B returned %d rows, want 0: %+v", len(raw), raw)
		}
	})

	t.Run("CrossTenantUpdate_DeniedAndLeavesRowUnchanged", func(t *testing.T) {
		widget := &testutil.Widget{ID: "pg-cross-update-1", Name: "gadget", Value: 1}
		if err := repo.Create(ctxFor(tenantA), widget); err != nil {
			t.Fatalf("Create() error = %v", err)
		}

		attempt := &testutil.Widget{ID: widget.ID, Name: "hijacked", Value: 999}
		if err := repo.Update(ctxFor(tenantB), attempt); !isRecordNotFound(err) {
			t.Fatalf("Update() from tenant B error = %v, want ErrRecordNotFound", err)
		}

		got, err := repo.FindByID(ctxFor(tenantA), widget.ID)
		if err != nil {
			t.Fatalf("FindByID() by the real owner after the failed cross-tenant Update error = %v", err)
		}
		if got.Name != "gadget" || got.Value != 1 {
			t.Errorf("row after failed cross-tenant Update = %+v, want unchanged {Name:gadget Value:1}", *got)
		}

		// Layer 1 directly, using a raw bulk Update() with no primary key
		// in its own WHERE clause beyond what the plugin itself adds --
		// the shape a future reporting/bulk-maintenance query would use.
		res := db.WithContext(ctxFor(tenantB)).Model(&testutil.Widget{}).Where("id = ?", widget.ID).Update("value", 999)
		if res.Error != nil {
			t.Fatalf("raw plugin-scoped Update() as tenant B error = %v", res.Error)
		}
		if res.RowsAffected != 0 {
			t.Errorf("raw plugin-scoped Update() as tenant B affected %d rows, want 0", res.RowsAffected)
		}
	})

	t.Run("CrossTenantDelete_DeniedAndDoesNotDeleteRow", func(t *testing.T) {
		widget := &testutil.Widget{ID: "pg-cross-delete-1", Name: "gadget", Value: 1}
		if err := repo.Create(ctxFor(tenantA), widget); err != nil {
			t.Fatalf("Create() error = %v", err)
		}

		if err := repo.Delete(ctxFor(tenantB), widget.ID); !isRecordNotFound(err) {
			t.Fatalf("Delete() from tenant B error = %v, want ErrRecordNotFound", err)
		}
		if _, err := repo.FindByID(ctxFor(tenantA), widget.ID); err != nil {
			t.Errorf("FindByID() by the real owner after the failed cross-tenant Delete error = %v, want the row still present", err)
		}
	})

	t.Run("List_TwoTenants_ReturnsOnlyCallingTenantRows", func(t *testing.T) {
		for _, id := range []string{"pg-list-a-1", "pg-list-a-2"} {
			if err := repo.Create(ctxFor(tenantA), &testutil.Widget{ID: id, Name: "a-widget"}); err != nil {
				t.Fatalf("Create(tenant-a, %s) error = %v", id, err)
			}
		}
		if err := repo.Create(ctxFor(tenantB), &testutil.Widget{ID: "pg-list-b-1", Name: "b-widget"}); err != nil {
			t.Fatalf("Create(tenant-b) error = %v", err)
		}

		gotA, err := repo.List(ctxFor(tenantA))
		if err != nil {
			t.Fatalf("List(tenant-a) error = %v", err)
		}
		for _, w := range gotA {
			if w.TenantID != string(tenantA) {
				t.Errorf("List(tenant-a) returned tenant %q's row: %+v", w.TenantID, w)
			}
		}

		gotB, err := repo.List(ctxFor(tenantB))
		if err != nil {
			t.Fatalf("List(tenant-b) error = %v", err)
		}
		foundB := false
		for _, w := range gotB {
			if w.TenantID != string(tenantB) {
				t.Errorf("List(tenant-b) returned tenant %q's row: %+v", w.TenantID, w)
			}
			if w.ID == "pg-list-b-1" {
				foundB = true
			}
		}
		if !foundB {
			t.Errorf("List(tenant-b) = %+v, want to include pg-list-b-1", gotB)
		}
	})

	t.Run("NoTenantContext_FailsClosed", func(t *testing.T) {
		bg := context.Background()
		// Repository returns pkgcore's error unmodified (see Repository's
		// own doc comment), while the raw plugin path wraps it in
		// dbkit.ErrMissingTenantContext with pkgcore.ErrNoTenant as its
		// cause — apperr.Error implements Unwrap, so errors.Is sees through
		// both shapes uniformly, matching tenant_scope_test.go's own
		// assertion style in the parent package.
		if _, err := repo.List(bg); !errors.Is(err, pkgcore.ErrNoTenant) {
			t.Errorf("List() with no tenant context error = %v, want it to wrap pkgcore.ErrNoTenant", err)
		}
		var raw []testutil.Widget
		err := db.WithContext(bg).Find(&raw).Error
		if !errors.Is(err, pkgcore.ErrNoTenant) {
			t.Errorf("raw plugin-scoped Find() with no tenant context error = %v, want it to wrap pkgcore.ErrNoTenant", err)
		}
		if len(raw) != 0 {
			t.Errorf("raw plugin-scoped Find() with no tenant context returned %d rows, want 0", len(raw))
		}
	})
}
