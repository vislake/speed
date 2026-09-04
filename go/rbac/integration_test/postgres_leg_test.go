//go:build integration

// Package rbac_test holds go/rbac's integration tier: the module's proof
// surface re-run against real servers -- PostgreSQL for the three tables'
// schema and isolation semantics (migrations zero to head, the unique
// indexes, tenancytest.AssertIsolated against a real server) and Redis for
// the cross-replica cache invalidation that a stale authorization decision
// makes a security problem rather than a performance one. It is physically
// separate from go/rbac's unit tests (all of which live in package rbac
// itself, one file per source file, per the backend coding standard's
// testing layout rule) and carries the "integration" build tag: a plain
// "go test ./..." never compiles or runs anything in this directory; it is
// invoked explicitly with "go test -tags=integration ./...". This mirrors
// the identical convention of go/dbkit, go/jobs, go/pkgcore and go/config.
//
// Every test here spins up its own disposable container and requires a
// working Docker (or Docker-API-compatible) daemon; there is no fallback
// or skip-on-missing-Docker path, matching the other modules' tiers.
//
// Why PostgreSQL earns a leg of its own: root CLAUDE.md requires
// migrations to run from zero on BOTH dialects, and the unit tier can only
// run the SQLite half. The PostgreSQL half is not a formality here -- the
// bindings table's node_id column is NOT NULL with an empty-string
// default, and carries a sentinel rather than a NULL,
// precisely because PostgreSQL treats NULLs as distinct inside a unique
// index, so a nullable column would let the same tenant-wide grant be
// written twice. That divergence is invisible on SQLite and is pinned
// below.
package rbac_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	// Blank-imported for its init side effect: registers
	// dbkit.DialectPostgres so the dbkit.Open(DialectPostgres) call below
	// has a driver to build from.
	_ "github.com/vislake/speed/go/dbkit/dialect/postgres"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// hostPermissions is what the host's OTHER modules declare in this tier,
// so the frozen catalog under test is a mixed one rather than just rbac's
// own two entries -- the realistic case, and the only one in which "a
// permission this module did not declare" is distinguishable.
var hostPermissions = []string{"notes:read", "notes:write", "billing:manage"}

// startPostgresContainer starts a disposable PostgreSQL 16 container for
// one test, already registered for termination via t.Cleanup. It follows
// go/config/integration_test's helper, which follows go/dbkit's.
func startPostgresContainer(t *testing.T, ctx context.Context) *postgres.PostgresContainer {
	t.Helper()

	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("rbac"),
		postgres.WithUsername("rbac"),
		postgres.WithPassword("rbac"),
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
		if terminateErr := testcontainers.TerminateContainer(pgContainer); terminateErr != nil {
			t.Errorf("terminate postgres testcontainer: %v", terminateErr)
		}
	})
	return pgContainer
}

// openRBACPostgres opens the container's database through dbkit.Open --
// the only sanctioned way to obtain a *gorm.DB -- and applies rbac's own
// migrations to it with the dialect they ship for, exactly the way a host
// applies them at startup. Nothing here calls AutoMigrate; the versioned
// SQL under migrations/postgres is what creates every table, which is what
// makes this the zero-to-head proof root CLAUDE.md asks for on the second
// dialect.
func openRBACPostgres(t *testing.T, ctx context.Context, pgContainer *postgres.PostgresContainer) *gorm.DB {
	t.Helper()

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres testcontainer connection string: %v", err)
	}
	db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectPostgres, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open(DialectPostgres): %v", err)
	}
	migrations := dbkit.NewMigrationRegistry()
	if err = migrations.Register(rbac.NewModule(db)); err != nil {
		t.Fatalf("registering the rbac migrations: %v", err)
	}
	if err = migrations.Apply(ctx, db, dbkit.DialectPostgres); err != nil {
		t.Fatalf("applying the rbac migrations on PostgreSQL: %v", err)
	}
	return db
}

// attachRBACService folds hostPermissions into a fresh registry over the
// given bus and returns the Service Attach produced, with a cache lifetime
// long enough that no test can pass because an entry happened to expire.
// Every convergence assertion in this tier therefore proves the event path,
// never the anti-loss TTL.
func attachRBACService(t *testing.T, db *gorm.DB, bus pkgcore.EventBus, opts ...rbac.Option) *rbac.Service {
	t.Helper()

	reg := pkgcore.NewRegistry(bus, pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := reg.Permissions.Add(hostPermissions...); err != nil {
		t.Fatalf("declaring the host's permissions: %v", err)
	}
	module := rbac.NewModule(db, append([]rbac.Option{rbac.WithCacheTTL(time.Hour)}, opts...)...)
	if err := module.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	svc, err := module.Attach(reg)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := svc.Close(); closeErr != nil {
			t.Errorf("Close: %v", closeErr)
		}
	})
	return svc
}

// tenantContext is the context a host hands in after tenancy.Middleware
// has resolved the tenant.
func tenantContext(tenant pkgcore.TenantID) context.Context {
	return pkgcore.WithTenant(context.Background(), tenant)
}

// TestPostgres_Migrations_CreateEveryTableFromZero proves the second
// dialect's migration set actually runs and produces the three tables the
// models are mapped onto. openRBACPostgres has already applied it from an
// empty database; this asserts the result rather than trusting the absence
// of an error.
func TestPostgres_Migrations_CreateEveryTableFromZero(t *testing.T) {
	ctx := context.Background()
	db := openRBACPostgres(t, ctx, startPostgresContainer(t, ctx))

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB(): %v", err)
	}
	for _, table := range []string{"rbac_roles", "rbac_role_permissions", "rbac_role_bindings"} {
		var exists bool
		if err = sqlDB.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)`,
			table).Scan(&exists); err != nil {
			t.Fatalf("querying information_schema for %q: %v", table, err)
		}
		if !exists {
			t.Errorf("table %q was not created by the PostgreSQL migration set", table)
		}
	}
}

// TestPostgres_Repositories_AreTenantIsolated runs the mandatory suite
// against a real server rather than SQLite. The unit tier runs the same
// three assertions; repeating them here is what proves the isolation does
// not depend on a SQLite-only behaviour of the GORM plugin.
func TestPostgres_Repositories_AreTenantIsolated(t *testing.T) {
	ctx := context.Background()
	db := openRBACPostgres(t, ctx, startPostgresContainer(t, ctx))

	t.Run("roles", func(t *testing.T) {
		repo := rbac.NewRoleRepository(db)
		tenancytest.AssertIsolated(t, repo.Repository, func(pkgcore.TenantID) *rbac.Role {
			id := uuid.NewString()
			return &rbac.Role{ID: id, Key: "role-" + id, DescriptionKey: "rbac.role.member"}
		})
	})
	t.Run("role permissions", func(t *testing.T) {
		repo := rbac.NewRolePermissionRepository(db)
		tenancytest.AssertIsolated(t, repo.Repository, func(pkgcore.TenantID) *rbac.RolePermission {
			return &rbac.RolePermission{ID: uuid.NewString(), RoleID: uuid.NewString(), Permission: "notes:read"}
		})
	})
	t.Run("role bindings", func(t *testing.T) {
		repo := rbac.NewRoleBindingRepository(db)
		tenancytest.AssertIsolated(t, repo.Repository, func(pkgcore.TenantID) *rbac.RoleBinding {
			return &rbac.RoleBinding{ID: uuid.NewString(), UserID: uuid.NewString(), RoleID: uuid.NewString()}
		})
	})
}

// TestPostgres_AssignRole_TenantWideTwice_WritesOneRow is the reason this
// leg exists at all.
//
// A tenant-wide binding stores node_id as the empty string, never NULL,
// and the column is NOT NULL with an empty-string default. PostgreSQL
// treats NULLs as
// DISTINCT inside a unique index, so with a nullable column the unique
// constraint on (tenant_id, user_id, role_id, node_id) would not fire for
// two tenant-wide grants of the same role to the same user -- the duplicate
// row would be accepted, and revoking once would leave the grant standing.
// SQLite's default (also NULLS DISTINCT) hides nothing here, but the
// dialect divergence around it is exactly the class of bug the second leg
// is for, so the invariant is pinned on the server that would suffer it.
func TestPostgres_AssignRole_TenantWideTwice_WritesOneRow(t *testing.T) {
	ctx := context.Background()
	db := openRBACPostgres(t, ctx, startPostgresContainer(t, ctx))
	svc := attachRBACService(t, db, pkgcore.NewMemoryEventBus())

	tenantCtx := tenantContext("tenant-a")
	sub := rbac.Subject{TenantID: "tenant-a", UserID: "user-1"}
	if _, err := svc.DefineRole(tenantCtx, rbac.RoleDefinition{
		Key:            "reader",
		DescriptionKey: "rbac.role.member",
		Permissions:    []string{"notes:read"},
	}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if err := svc.AssignRole(tenantCtx, sub, "reader", rbac.Scope{}); err != nil {
			t.Fatalf("AssignRole attempt %d: %v", attempt, err)
		}
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB(): %v", err)
	}
	var rows int64
	if err = sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM rbac_role_bindings WHERE tenant_id = $1 AND user_id = $2`,
		"tenant-a", "user-1").Scan(&rows); err != nil {
		t.Fatalf("counting bindings: %v", err)
	}
	if rows != 1 {
		t.Fatalf("binding rows = %d, want 1 -- a repeated tenant-wide assignment must upsert, not duplicate", rows)
	}

	// The sentinel, asserted directly: a NULL here would silently defeat
	// the unique index above on PostgreSQL.
	var nulls int64
	if err = sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM rbac_role_bindings WHERE node_id IS NULL`).Scan(&nulls); err != nil {
		t.Fatalf("counting null node_ids: %v", err)
	}
	if nulls != 0 {
		t.Fatalf("%d binding row(s) hold a NULL node_id; the tenant-root sentinel must be the empty string", nulls)
	}

	// And the grant is a single revocable unit rather than two stacked
	// rows, which is what the duplicate would have made it.
	if err = svc.RevokeRole(tenantCtx, sub, "reader", rbac.Scope{}); err != nil {
		t.Fatalf("RevokeRole: %v", err)
	}
	allowed, err := svc.Can(ctx, sub, "read", "notes")
	if err != nil {
		t.Fatalf("Can after revoke: %v", err)
	}
	if allowed {
		t.Fatal("one revoke left the grant standing; the second assignment had written a duplicate row")
	}
}

// TestPostgres_Evaluation_IsTenantIsolatedEndToEnd exercises the whole
// decision path -- define, assign, evaluate, revoke -- on a real server,
// with the sharpest isolation case: the SAME user id bound to the SAME
// role key in two tenants.
func TestPostgres_Evaluation_IsTenantIsolatedEndToEnd(t *testing.T) {
	ctx := context.Background()
	db := openRBACPostgres(t, ctx, startPostgresContainer(t, ctx))
	svc := attachRBACService(t, db, pkgcore.NewMemoryEventBus())

	inA := rbac.Subject{TenantID: "tenant-a", UserID: "user-1"}
	inB := rbac.Subject{TenantID: "tenant-b", UserID: "user-1"}

	// Both tenants define a role under the same key. The unique index is
	// (tenant_id, key), so this must succeed twice.
	for _, sub := range []rbac.Subject{inA, inB} {
		if _, err := svc.DefineRole(tenantContext(sub.TenantID), rbac.RoleDefinition{
			Key:            "reader",
			DescriptionKey: "rbac.role.member",
			Permissions:    []string{"notes:read"},
		}); err != nil {
			t.Fatalf("DefineRole in %s: %v", sub.TenantID, err)
		}
	}
	if err := svc.AssignRole(tenantContext(inA.TenantID), inA, "reader", rbac.Scope{}); err != nil {
		t.Fatalf("AssignRole in tenant-a: %v", err)
	}

	assertCan := func(sub rbac.Subject, want bool, what string) {
		t.Helper()
		got, err := svc.Can(ctx, sub, "read", "notes")
		if err != nil {
			t.Fatalf("Can(%s): %v", what, err)
		}
		if got != want {
			t.Fatalf("Can(%s) = %v, want %v", what, got, want)
		}
	}
	assertCan(inA, true, "tenant-a, granted")
	assertCan(inB, false, "tenant-b, same user id, never granted")

	// A permission the role does not carry stays denied, and a permission
	// nobody declared denies rather than erroring.
	assertNotGranted := func(action, resource string) {
		t.Helper()
		got, err := svc.Can(ctx, inA, action, resource)
		if err != nil {
			t.Fatalf("Can(%s:%s): %v", resource, action, err)
		}
		if got {
			t.Fatalf("Can(%s:%s) = true, want false", resource, action)
		}
	}
	assertNotGranted("write", "notes")
	assertNotGranted("read", "nosuch")

	if err := svc.RevokeRole(tenantContext(inA.TenantID), inA, "reader", rbac.Scope{}); err != nil {
		t.Fatalf("RevokeRole: %v", err)
	}
	assertCan(inA, false, "tenant-a, after revoke")
}

// TestPostgres_SystemDomain_IsAnOrdinaryTenant pins the pseudo-tenant's
// whole contract on a real server: a platform-operations grant is a row
// with tenant_id = 'system', written and read through the identical code
// path, and holding it grants nothing in any customer tenant.
func TestPostgres_SystemDomain_IsAnOrdinaryTenant(t *testing.T) {
	ctx := context.Background()
	db := openRBACPostgres(t, ctx, startPostgresContainer(t, ctx))
	svc := attachRBACService(t, db, pkgcore.NewMemoryEventBus())

	operator := rbac.Subject{TenantID: rbac.SystemDomain, UserID: "staff-1"}
	systemCtx := tenantContext(rbac.SystemDomain)
	if err := svc.EnsureBuiltinRoles(systemCtx); err != nil {
		t.Fatalf("EnsureBuiltinRoles in the system domain: %v", err)
	}
	if err := svc.AssignRole(systemCtx, operator, "owner", rbac.Scope{}); err != nil {
		t.Fatalf("AssignRole in the system domain: %v", err)
	}

	granted, err := svc.Can(ctx, operator, "manage", "billing")
	if err != nil {
		t.Fatalf("Can(system operator): %v", err)
	}
	if !granted {
		t.Fatal("the system-domain owner does not hold billing:manage; the pseudo-tenant must evaluate like any other")
	}

	// The same person, named in a customer tenant, holds nothing: the
	// pseudo-tenant is not a wildcard.
	inCustomer := rbac.Subject{TenantID: "tenant-a", UserID: "staff-1"}
	crossed, err := svc.Can(ctx, inCustomer, "manage", "billing")
	if err != nil {
		t.Fatalf("Can(same user in a customer tenant): %v", err)
	}
	if crossed {
		t.Fatal("a system-domain grant leaked into a customer tenant")
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB(): %v", err)
	}
	var rows int64
	if err = sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM rbac_role_bindings WHERE tenant_id = $1`, string(rbac.SystemDomain)).Scan(&rows); err != nil {
		t.Fatalf("counting system-domain bindings: %v", err)
	}
	if rows != 1 {
		t.Fatalf("system-domain binding rows = %d, want 1 -- the grant must be an ordinary row, not a special case", rows)
	}
}
