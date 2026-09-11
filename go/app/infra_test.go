package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// TestNew_RunsPreDBBeforeTheDatabaseIsOpen pins the ordering constraint the
// callback exists for: it runs after the platform cipher is built and before
// dbkit.Open touches the DSN, so GORM never parses a model whose serializer
// has not been registered yet.
func TestNew_RunsPreDBBeforeTheDatabaseIsOpen(t *testing.T) {
	host := testHostConfig{PlatformConfig: testPlatformConfig()}
	spec := testDatabaseSpec(t)
	var preDBCipher *dbkit.Cipher
	var moduleCipher *dbkit.Cipher

	_, err := New(context.Background(),
		WithConfig(ConfigSpec{Host: &host, Platform: &host.PlatformConfig}, testConfigOptions()...),
		WithDatabase(spec),
		WithPreDB(func(_ context.Context, cipher *dbkit.Cipher) error {
			if cipher == nil {
				t.Error("the pre-database callback received a nil cipher")
			}
			preDBCipher = cipher
			if _, statErr := os.Stat(spec.DSN); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("the database file exists during pre-db (stat error %v), want the callback to run before Open", statErr)
			}
			return nil
		}),
		WithModules(func(_ context.Context, deps ModuleDeps) ([]pkgcore.Module, error) {
			moduleCipher = deps.Cipher
			if _, statErr := os.Stat(spec.DSN); statErr != nil {
				t.Errorf("the database file is missing during module construction: %v", statErr)
			}
			return nil, nil
		}),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := os.Stat(spec.DSN); err != nil {
		t.Errorf("the database file is missing after New: %v", err)
	}
	if preDBCipher == nil || preDBCipher != moduleCipher {
		t.Error("WithPreDB and WithModules received different ciphers, want one platform cipher for both")
	}
}

// TestNew_AppliesEveryModulesMigrations pins the migration stage: a module
// shipping sqlite migrations has them applied to the opened database, and the
// empty-FS module beside it is no obstacle.
func TestNew_AppliesEveryModulesMigrations(t *testing.T) {
	var host testHostConfig
	var deps ModuleDeps

	a, err := New(context.Background(), append(testBaseOptions(t, &host),
		WithModules(func(_ context.Context, d ModuleDeps) ([]pkgcore.Module, error) {
			deps = d
			return []pkgcore.Module{
				&testModule{name: "migrating", migrations: testMigrations()},
				&testModule{name: "empty"},
			}, nil
		}),
	)...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	var probes int64
	if err := deps.DB.Table("engine_probe").Count(&probes).Error; err != nil {
		t.Fatalf("the fixture migration's table is not queryable: %v", err)
	}
	var applied []string
	if err := deps.DB.Table("schema_migrations").Where("module = ?", "migrating").Pluck("filename", &applied).Error; err != nil {
		t.Fatalf("read the applied-migration bookkeeping: %v", err)
	}
	if len(applied) != 1 || applied[0] != "0001_engine_probe.sql" {
		t.Fatalf("applied migrations for the fixture module = %v, want the one fixture file", applied)
	}
	var emptyRows int64
	if err := deps.DB.Table("schema_migrations").Where("module = ?", "empty").Count(&emptyRows).Error; err != nil {
		t.Fatalf("count the empty module's bookkeeping rows: %v", err)
	}
	if emptyRows != 0 {
		t.Fatalf("the module with no migrations recorded %d rows, want none", emptyRows)
	}
}

// TestNew_RefusesDuplicateModuleNames pins the registration refusal: two
// modules with one name fail the assembly, naming the registration that
// refused.
func TestNew_RefusesDuplicateModuleNames(t *testing.T) {
	var host testHostConfig

	_, err := New(context.Background(), append(testBaseOptions(t, &host),
		WithModules(func(context.Context, ModuleDeps) ([]pkgcore.Module, error) {
			return []pkgcore.Module{&testModule{name: "twice"}, &testModule{name: "twice"}}, nil
		}),
	)...)
	if err == nil {
		t.Fatal("New() with duplicate module names error = nil, want a refusal")
	}
	if !errors.Is(err, dbkit.ErrDuplicateModule) || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate refusal = %v, want it to wrap dbkit.ErrDuplicateModule and name the module", err)
	}
}

// TestNew_RefusesAModuleRegistrationError pins the kernel's own refusal path:
// a module whose Register fails fails the assembly.
func TestNew_RefusesAModuleRegistrationError(t *testing.T) {
	var host testHostConfig
	wantErr := errors.New("register refused")

	_, err := New(context.Background(), append(testBaseOptions(t, &host),
		WithModules(func(context.Context, ModuleDeps) ([]pkgcore.Module, error) {
			return []pkgcore.Module{&testModule{name: "faulty", registerErr: wantErr}}, nil
		}),
	)...)
	if !errors.Is(err, wantErr) {
		t.Fatalf("New() error = %v, want it to wrap the module's Register failure", err)
	}
}

// TestNew_RefusesAnUnknownDialect pins the dialect refusal: an unrecognized
// dialect never reaches the driver, it fails the infrastructure stage.
func TestNew_RefusesAnUnknownDialect(t *testing.T) {
	host := testHostConfig{PlatformConfig: testPlatformConfig()}

	_, err := New(context.Background(),
		WithConfig(ConfigSpec{Host: &host, Platform: &host.PlatformConfig}, testConfigOptions()...),
		WithDatabase(DatabaseSpec{Dialect: "oracle", DSN: "anywhere"}),
	)
	if err == nil {
		t.Fatal("New() with an unknown dialect error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "open the database") {
		t.Fatalf("dialect refusal = %v, want it from the infrastructure stage", err)
	}
}

// TestNew_HandsTheAuditCaptureScopeToOpen pins the DatabaseSpec's audit
// fields reaching dbkit.Open: a capture scope naming a model that cannot be
// captured is refused by Open, which never happens if the spec never arrives.
func TestNew_HandsTheAuditCaptureScopeToOpen(t *testing.T) {
	host := testHostConfig{PlatformConfig: testPlatformConfig()}
	spec := testDatabaseSpec(t)
	spec.AuditBus = pkgcore.NewMemoryEventBus()
	spec.AuditModels = []any{&struct{ NotAuditable string }{}} // deliberately not Auditable

	_, err := New(context.Background(),
		WithConfig(ConfigSpec{Host: &host, Platform: &host.PlatformConfig}, testConfigOptions()...),
		WithDatabase(spec),
	)
	if err == nil {
		t.Fatal("New() with a non-capturable audit model error = nil, want dbkit.Open's refusal")
	}
	if !strings.Contains(err.Error(), "open the database") {
		t.Fatalf("audit-scope refusal = %v, want it from the infrastructure stage", err)
	}
}
