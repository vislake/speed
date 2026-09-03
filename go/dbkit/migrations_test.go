package dbkit

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/vislake/speed/go/dbkit/internal/migrationfixture/basemodule"
	"github.com/vislake/speed/go/dbkit/internal/migrationfixture/derivedmodule"
	"github.com/vislake/speed/go/pkgcore"
)

// fakeModule is a minimal pkgcore.Module used only by this test file. Only
// Name, DependsOn and Migrations are ever read by MigrationRegistry;
// Locales, OpenAPISpec and Register exist solely to satisfy the interface
// and are never exercised here.
type fakeModule struct {
	name       string
	dependsOn  []string
	migrations embed.FS
}

func (m fakeModule) Name() string                     { return m.name }
func (m fakeModule) DependsOn() []string              { return m.dependsOn }
func (m fakeModule) Migrations() embed.FS             { return m.migrations }
func (m fakeModule) Locales() embed.FS                { return embed.FS{} }
func (m fakeModule) OpenAPISpec() []byte              { return nil }
func (m fakeModule) Register(*pkgcore.Registry) error { return nil }

var _ pkgcore.Module = fakeModule{}

// migrationsTestDBSeq gives every per-test SQLite database, and every
// per-test Postgres schema, a distinct name -- mirroring
// testutil.NewTestSQLite and newEncryptionTestDB, which use the same
// pattern for the same reason: no two calls, whether from the same test,
// from parallel tests, or from subtests, may ever share state.
var migrationsTestDBSeq atomic.Uint64

// newMigrationsTestSQLite opens a private in-memory SQLite database for one
// test. It is pinned to a single connection so SQLite's shared in-memory
// cache is never torn down between queries; see the fuller explanation on
// testutil.NewTestSQLite, which this mirrors. Unlike that helper, it applies
// no migration itself -- MigrationRegistry.Apply is exactly the thing under
// test here.
func newMigrationsTestSQLite(t *testing.T) *gorm.DB {
	t.Helper()

	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	dsn := fmt.Sprintf("file:%s_%d?mode=memory&cache=shared", name, migrationsTestDBSeq.Add(1))

	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get underlying sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	return db
}

// localPostgresDSN is the conventional local devcontainer/testcontainer
// Postgres endpoint this suite opportunistically probes: default port,
// default "postgres" superuser and database, no password, sslmode disabled
// for a local connection. It matches the defaults most local
// "docker run postgres" one-liners and testcontainers-go's postgres module
// use.
const localPostgresDSN = "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"

// openLocalPostgresForTest attempts to open and ping localPostgresDSN within
// a short timeout, returning ok=false on any failure: nothing listening,
// wrong credentials, wrong database, and so on are all treated the same
// way, since this suite's job is only to opportunistically use a local
// Postgres when one happens to be available, not to diagnose why one is
// not.
func openLocalPostgresForTest(t *testing.T) (db *gorm.DB, ok bool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	db, err := gorm.Open(postgres.Open(localPostgresDSN), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		return nil, false
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, false
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, false
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db, true
}

// prepareMigrationsTestPostgresSchema creates a fresh, uniquely named
// Postgres schema and points db's search_path at it, so this test's tables
// -- including schema_migrations itself -- can never collide with another
// test's, or with anything already present on a long-lived local Postgres
// instance. The schema, and everything in it, is dropped when the test
// finishes.
//
// db is pinned to a single pooled connection first: search_path is a
// session-scoped setting, and a second pooled connection would silently not
// see it, putting later statements back on the default "public" schema.
func prepareMigrationsTestPostgresSchema(t *testing.T, db *gorm.DB) {
	t.Helper()

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get underlying sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	schema := fmt.Sprintf("dbkit_migrations_test_%d", migrationsTestDBSeq.Add(1))
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
}

// runOnEachAvailableDialect runs test once against a fresh SQLite database,
// and again against a fresh, uniquely schemed local Postgres database if
// one is reachable at localPostgresDSN within a short timeout.
//
// Postgres is opportunistic, not required: this file carries no build tag,
// so it must stay fast and hermetic on a machine with no local Postgres,
// which is the common case for a plain "go test ./...". When none is
// reachable, the Postgres subtest reports itself skipped (not failed) and
// says why; SQLite coverage above it still ran.
//
// Be careful what you infer from that skip: this package's own
// integration_test/ tier carries no MigrationRegistry test at all -- its
// Postgres files execute testutil.WidgetPostgresMigrationSQL by hand and
// never call Apply. The always-on PostgreSQL exercise of Apply is
// go/config/integration_test/postgres_leg_test.go's openConfigPostgres,
// which registers a real module and applies its postgres/*.sql through
// this very registry. So the engine does get real Postgres coverage, but
// borrowed from a downstream module rather than owned here; adding a
// dbkit-local Apply test to integration_test/ (via dbtest.NewPostgres)
// would make it local. Either way it belongs to the integration tier, not
// to this unit-tier file (AGENTS.md, and the testing standard's
// unit/integration split).
func runOnEachAvailableDialect(t *testing.T, test func(t *testing.T, dialect Dialect, db *gorm.DB)) {
	t.Helper()

	t.Run("sqlite", func(t *testing.T) {
		test(t, DialectSQLite, newMigrationsTestSQLite(t))
	})

	t.Run("postgres", func(t *testing.T) {
		db, ok := openLocalPostgresForTest(t)
		if !ok {
			t.Skip("no local Postgres reachable at the conventional local dev endpoint; " +
				"skipping (SQLite coverage above still ran; the integration_test/ tier is " +
				"the authoritative Postgres check)")
		}
		prepareMigrationsTestPostgresSchema(t, db)
		test(t, DialectPostgres, db)
	})
}

func TestMigrationRegistry_Apply_CalledTwice_SecondCallIsNoOp(t *testing.T) {
	runOnEachAvailableDialect(t, func(t *testing.T, dialect Dialect, db *gorm.DB) {
		reg := NewMigrationRegistry()
		if err := reg.Register(fakeModule{name: "base", migrations: basemodule.Migrations}); err != nil {
			t.Fatalf("Register(base) error = %v", err)
		}

		ctx := context.Background()
		if err := reg.Apply(ctx, db, dialect); err != nil {
			t.Fatalf("first Apply() error = %v", err)
		}

		var firstCount int64
		if err := db.Table(schemaMigrationsTable).Count(&firstCount).Error; err != nil {
			t.Fatalf("count %s after first Apply(): %v", schemaMigrationsTable, err)
		}
		if firstCount != 2 {
			t.Fatalf("%s rows after first Apply() = %d, want 2 (base declares two migration files)", schemaMigrationsTable, firstCount)
		}

		if err := reg.Apply(ctx, db, dialect); err != nil {
			t.Fatalf("second Apply() error = %v, want nil: re-running must skip already-applied files, not re-execute them", err)
		}

		var secondCount int64
		if err := db.Table(schemaMigrationsTable).Count(&secondCount).Error; err != nil {
			t.Fatalf("count %s after second Apply(): %v", schemaMigrationsTable, err)
		}
		if secondCount != firstCount {
			t.Errorf("%s rows after second Apply() = %d, want %d (unchanged)", schemaMigrationsTable, secondCount, firstCount)
		}

		var seedRows int64
		if err := db.Table("base_items").Where("id = ?", "seed-0002").Count(&seedRows).Error; err != nil {
			t.Fatalf("count base_items seed row: %v", err)
		}
		if seedRows != 1 {
			t.Errorf("base_items rows with id = seed-0002 after two Apply() calls = %d, want exactly 1 (0002_seed_base_items.sql must not re-run)", seedRows)
		}
	})
}

func TestMigrationRegistry_Apply_ModulesRegisteredOutOfOrder_AppliesInDependencyOrder(t *testing.T) {
	runOnEachAvailableDialect(t, func(t *testing.T, dialect Dialect, db *gorm.DB) {
		reg := NewMigrationRegistry()

		// Registered in reverse dependency order on purpose: Apply is
		// responsible for reordering by DependsOn, not for trusting
		// registration order. If it did trust registration order, derived's
		// 0002 migration (which inserts into a table only base's migration
		// creates) would fail.
		derived := fakeModule{name: "derived", dependsOn: []string{"base"}, migrations: derivedmodule.Migrations}
		base := fakeModule{name: "base", migrations: basemodule.Migrations}
		if err := reg.Register(derived); err != nil {
			t.Fatalf("Register(derived) error = %v", err)
		}
		if err := reg.Register(base); err != nil {
			t.Fatalf("Register(base) error = %v", err)
		}

		if err := reg.Apply(context.Background(), db, dialect); err != nil {
			t.Fatalf("Apply() error = %v, want nil -- a \"no such table\"/\"relation does not exist\" error for "+
				"base_items here means MigrationRegistry applied \"derived\" before \"base\"", err)
		}

		var seededFromDerived int64
		if err := db.Table("base_items").Where("id = ?", "seed-from-derived").Count(&seededFromDerived).Error; err != nil {
			t.Fatalf("count base_items seeded-from-derived row: %v", err)
		}
		if seededFromDerived != 1 {
			t.Errorf("base_items rows with id = seed-from-derived = %d, want exactly 1", seededFromDerived)
		}
	})
}

func TestMigrationRegistry_Apply_DependsOnUnregisteredModule_ReturnsMissingDependencyError(t *testing.T) {
	// Dialect-independent: sortModulesByDependency runs, and fails, before
	// any SQL is read or executed, so SQLite-only coverage is sufficient
	// here.
	reg := NewMigrationRegistry()
	orphan := fakeModule{name: "orphan", dependsOn: []string{"ghost"}, migrations: basemodule.Migrations}
	if err := reg.Register(orphan); err != nil {
		t.Fatalf("Register(orphan) error = %v", err)
	}

	db := newMigrationsTestSQLite(t)
	err := reg.Apply(context.Background(), db, DialectSQLite)
	if err == nil {
		t.Fatal("Apply() error = nil, want an error wrapping ErrMissingDependency")
	}
	if !errors.Is(err, ErrMissingDependency) {
		t.Errorf("Apply() error = %v, want it to wrap ErrMissingDependency", err)
	}
}

func TestMigrationRegistry_Apply_DependencyCycle_ReturnsDependencyCycleError(t *testing.T) {
	// Dialect-independent for the same reason as the missing-dependency
	// case above.
	reg := NewMigrationRegistry()
	a := fakeModule{name: "a", dependsOn: []string{"b"}, migrations: basemodule.Migrations}
	b := fakeModule{name: "b", dependsOn: []string{"a"}, migrations: basemodule.Migrations}
	if err := reg.Register(a); err != nil {
		t.Fatalf("Register(a) error = %v", err)
	}
	if err := reg.Register(b); err != nil {
		t.Fatalf("Register(b) error = %v", err)
	}

	db := newMigrationsTestSQLite(t)
	err := reg.Apply(context.Background(), db, DialectSQLite)
	if err == nil {
		t.Fatal("Apply() error = nil, want an error wrapping ErrDependencyCycle")
	}
	if !errors.Is(err, ErrDependencyCycle) {
		t.Errorf("Apply() error = %v, want it to wrap ErrDependencyCycle", err)
	}
}

func TestMigrationRegistry_Apply_UnknownDialect_ReturnsUnknownDialectError(t *testing.T) {
	reg := NewMigrationRegistry()
	db := newMigrationsTestSQLite(t)

	tests := []struct {
		name    string
		dialect Dialect
	}{
		{name: "unrecognized dialect value", dialect: Dialect("mysql")},
		{name: "empty dialect (zero value)", dialect: Dialect("")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := reg.Apply(context.Background(), db, tt.dialect)
			if err == nil {
				t.Fatal("Apply() error = nil, want an error wrapping ErrUnknownDialect")
			}
			if !errors.Is(err, ErrUnknownDialect) {
				t.Errorf("Apply() error = %v, want it to wrap ErrUnknownDialect", err)
			}
		})
	}
}

func TestMigrationRegistry_Apply_NilDB_ReturnsError(t *testing.T) {
	reg := NewMigrationRegistry()
	if err := reg.Apply(context.Background(), nil, DialectSQLite); err == nil {
		t.Fatal("Apply() error = nil, want an error for a nil *gorm.DB")
	}
}

func TestMigrationRegistry_Apply_NoModulesRegistered_CreatesTrackingTableAndSucceeds(t *testing.T) {
	runOnEachAvailableDialect(t, func(t *testing.T, dialect Dialect, db *gorm.DB) {
		reg := NewMigrationRegistry()
		if err := reg.Apply(context.Background(), db, dialect); err != nil {
			t.Fatalf("Apply() error = %v, want nil", err)
		}

		var count int64
		if err := db.Table(schemaMigrationsTable).Count(&count).Error; err != nil {
			t.Fatalf("count %s: %v", schemaMigrationsTable, err)
		}
		if count != 0 {
			t.Errorf("%s rows = %d, want 0", schemaMigrationsTable, count)
		}
	})
}

func TestMigrationRegistry_Apply_ModuleWithNoMigrationsForDialect_TreatsAsZeroFiles(t *testing.T) {
	reg := NewMigrationRegistry()
	// The zero value of embed.FS behaves as an empty filesystem: no
	// "postgres" or "sqlite" directory exists in it for any dialect, which
	// is exactly the case this test targets.
	if err := reg.Register(fakeModule{name: "empty"}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	db := newMigrationsTestSQLite(t)
	if err := reg.Apply(context.Background(), db, DialectSQLite); err != nil {
		t.Fatalf("Apply() error = %v, want nil: a module with no migrations directory for a dialect declares zero files, not an error", err)
	}
}

func TestMigrationRegistry_Register_RejectsInvalidModules(t *testing.T) {
	tests := []struct {
		name    string
		run     func(r *MigrationRegistry) error
		wantErr error
	}{
		{
			name:    "nil module",
			run:     func(r *MigrationRegistry) error { return r.Register(nil) },
			wantErr: ErrNilModule,
		},
		{
			name:    "empty module name",
			run:     func(r *MigrationRegistry) error { return r.Register(fakeModule{name: ""}) },
			wantErr: ErrEmptyModuleName,
		},
		{
			name: "duplicate module name",
			run: func(r *MigrationRegistry) error {
				if err := r.Register(fakeModule{name: "dup"}); err != nil {
					return err
				}
				return r.Register(fakeModule{name: "dup"})
			},
			wantErr: ErrDuplicateModule,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run(NewMigrationRegistry())
			if err == nil {
				t.Fatalf("Register() error = nil, want an error wrapping %v", tt.wantErr)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Register() error = %v, want it to wrap %v", err, tt.wantErr)
			}
		})
	}
}
