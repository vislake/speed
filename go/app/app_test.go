package app

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// testTokenKey is the bootstrap key the fixture modules declare: the host
// target binds it, so a bare assembly passes the binding check.
var testTokenKey = pkgcore.BootstrapKey{Key: "token", Format: "string"}

// TestNew_RunsTheAssemblyStagesInOrder pins the engine's fixed order as the
// host feels it: the pre-database callback before any module is constructed,
// the modules before the kernel bootstraps, the attach hooks after it, the
// HTTP face before the pre-serve step, and the background worker last.
func TestNew_RunsTheAssemblyStagesInOrder(t *testing.T) {
	var host testHostConfig
	var order []string
	record := func(step string) { order = append(order, step) }
	worker := &testWorker{}

	a, err := New(context.Background(), append(testBaseOptions(t, &host),
		WithPreDB(func(_ context.Context, cipher *dbkit.Cipher) error {
			if cipher == nil {
				t.Error("the pre-database callback received a nil cipher")
			}
			record("pre-db")
			return nil
		}),
		WithModules(func(_ context.Context, deps ModuleDeps) ([]pkgcore.Module, error) {
			if deps.DB == nil || deps.Cipher == nil {
				t.Errorf("WithModules received deps %+v, want an open database and the platform cipher", deps)
			}
			record("modules")
			return []pkgcore.Module{&testModule{name: "probe", keys: []pkgcore.BootstrapKey{testTokenKey}}}, nil
		}),
		WithHooks(Hooks{
			PostBootstrap: func(_ context.Context, a *Application) error {
				if a.Registry() == nil {
					t.Error("PostBootstrap saw a nil registry")
				}
				record("post-bootstrap")
				return nil
			},
			PostAttach: func(context.Context, *Application) error { record("post-attach"); return nil },
			PreServe: func(_ context.Context, a *Application) error {
				if a.Handler() == nil {
					t.Error("PreServe saw a nil handler; the HTTP face runs before it")
				}
				record("pre-serve")
				return nil
			},
		}),
		WithWorker(worker),
	)...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	want := []string{"pre-db", "modules", "post-bootstrap", "post-attach", "pre-serve", "worker-start"}
	if got := append(order, "worker-start"); !slices.Equal(got, want) {
		t.Fatalf("stage order = %v, want %v", got, want)
	}
	if starts, _ := worker.counts(); starts != 1 {
		t.Fatalf("worker Start calls = %d, want 1", starts)
	}
	if a.Kernel() == nil || a.Registry() == nil || a.Handler() == nil {
		t.Fatalf("accessors after New: Kernel=%v Registry=%v Handler=%v, want all non-nil",
			a.Kernel() != nil, a.Registry() != nil, a.Handler() != nil)
	}
	routes := a.Registry().Routes.Routes()
	if len(routes) != 0 {
		t.Fatalf("registry routes = %v, want the module to have mounted none", routes)
	}
}

// TestNew_HandlerAppearsOnlyAtTheHTTPStage pins where the handler comes into
// existence: the post-bootstrap attach runs before it, the pre-serve step
// after it.
func TestNew_HandlerAppearsOnlyAtTheHTTPStage(t *testing.T) {
	var host testHostConfig
	var postBootstrapHandler, preServeHandler http.Handler

	a, err := New(context.Background(), append(testBaseOptions(t, &host),
		WithHooks(Hooks{
			PostBootstrap: func(_ context.Context, a *Application) error {
				postBootstrapHandler = a.Handler()
				return nil
			},
			PreServe: func(_ context.Context, a *Application) error {
				preServeHandler = a.Handler()
				return nil
			},
		}),
	)...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	if postBootstrapHandler != nil {
		t.Error("PostBootstrap saw a composed handler, want nil: the HTTP face runs after that stage")
	}
	if preServeHandler == nil {
		t.Error("PreServe saw a nil handler, want the composed one: the HTTP face runs before that stage")
	}
}

// TestNew_RequiresConfigAndDatabase pins the refusal a host gets when it
// leaves out either required option: the engine supplies no default of its
// own.
func TestNew_RequiresConfigAndDatabase(t *testing.T) {
	if _, err := New(context.Background()); err == nil || !strings.Contains(err.Error(), "WithConfig is required") {
		t.Fatalf("New() with no options error = %v, want one naming WithConfig", err)
	}

	var host testHostConfig
	_, err := New(context.Background(), WithConfig(
		ConfigSpec{Host: &host, Platform: &host.PlatformConfig},
		testConfigOptions()...,
	))
	if err == nil || !strings.Contains(err.Error(), "WithDatabase is required") {
		t.Fatalf("New() without WithDatabase error = %v, want one naming WithDatabase", err)
	}
}

// TestNew_RefusesAnIncompleteConfigSpec pins the two target refusals the
// configuration stage names.
func TestNew_RefusesAnIncompleteConfigSpec(t *testing.T) {
	var host testHostConfig

	_, err := New(context.Background(),
		WithConfig(ConfigSpec{Platform: &host.PlatformConfig}, testConfigOptions()...),
		WithDatabase(testDatabaseSpec(t)),
	)
	if err == nil || !strings.Contains(err.Error(), "ConfigSpec.Host") {
		t.Fatalf("New() with a nil Host error = %v, want one naming ConfigSpec.Host", err)
	}

	_, err = New(context.Background(),
		WithConfig(ConfigSpec{Host: &host}, testConfigOptions()...),
		WithDatabase(testDatabaseSpec(t)),
	)
	if err == nil || !strings.Contains(err.Error(), "ConfigSpec.Platform") {
		t.Fatalf("New() with a nil Platform error = %v, want one naming ConfigSpec.Platform", err)
	}
}

// TestNew_RollsBackAModuleFailure pins the stage rollback: a module
// construction failure surfaces the callback's own error and leaves no
// database open behind it.
func TestNew_RollsBackAModuleFailure(t *testing.T) {
	var host testHostConfig
	var deps ModuleDeps
	wantErr := errors.New("module construction refused")

	_, err := New(context.Background(), append(testBaseOptions(t, &host),
		WithModules(func(_ context.Context, d ModuleDeps) ([]pkgcore.Module, error) {
			deps = d
			return nil, wantErr
		}),
	)...)
	if !errors.Is(err, wantErr) {
		t.Fatalf("New() error = %v, want it to wrap the module callback's error", err)
	}
	assertDatabaseClosed(t, deps.DB)
}

// TestNew_RollsBackALateStageFailure pins the teardown a failure in the last
// stages triggers: the composed handler's stage included, the database is
// closed and a registered worker is closed without ever having started.
func TestNew_RollsBackALateStageFailure(t *testing.T) {
	var host testHostConfig
	var deps ModuleDeps
	worker := &testWorker{}
	wantErr := errors.New("pre-serve refused")

	_, err := New(context.Background(), append(testBaseOptions(t, &host),
		WithModules(func(_ context.Context, d ModuleDeps) ([]pkgcore.Module, error) {
			deps = d
			return nil, nil
		}),
		WithWorker(worker),
		WithHooks(Hooks{PreServe: func(context.Context, *Application) error { return wantErr }}),
	)...)
	if !errors.Is(err, wantErr) {
		t.Fatalf("New() error = %v, want it to wrap the pre-serve hook's error", err)
	}
	assertDatabaseClosed(t, deps.DB)
	starts, closes := worker.counts()
	if starts != 0 || closes != 1 {
		t.Fatalf("worker calls: starts=%d closes=%d, want 0 starts and the rollback's close", starts, closes)
	}
}

// TestNew_RefusesAnUnboundDeclaredKey pins the binding verification: a
// module declaring a bootstrap key neither configuration target binds fails
// the assembly, naming the key.
func TestNew_RefusesAnUnboundDeclaredKey(t *testing.T) {
	var host testHostConfig

	_, err := New(context.Background(), append(testBaseOptions(t, &host),
		WithModules(func(context.Context, ModuleDeps) ([]pkgcore.Module, error) {
			return []pkgcore.Module{&testModule{
				name: "probe",
				keys: []pkgcore.BootstrapKey{{Key: "probe.token", Format: "string"}},
			}}, nil
		}),
	)...)
	if err == nil {
		t.Fatal("New() with an unbound declared key error = nil, want a binding refusal")
	}
	if !strings.Contains(err.Error(), "probe.token") {
		t.Fatalf("binding refusal does not name the declared key: %v", err)
	}
	if !errors.Is(err, pkgcore.ErrInvalidBootstrapKey) && !strings.Contains(err.Error(), "maps onto no field") {
		t.Fatalf("binding refusal does not explain the missing field: %v", err)
	}
}

// TestWithoutBackgroundWorkers_SkipsStartButStillCloses pins the explicit
// gate: the worker never starts this process's background work, and the
// ordered shutdown still closes it.
func TestWithoutBackgroundWorkers_SkipsStartButStillCloses(t *testing.T) {
	var host testHostConfig
	worker := &testWorker{}

	a, err := New(context.Background(), append(testBaseOptions(t, &host),
		WithWorker(worker),
		WithoutBackgroundWorkers(),
	)...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if starts, closes := worker.counts(); starts != 0 || closes != 0 {
		t.Fatalf("before Close: starts=%d closes=%d, want both 0", starts, closes)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if starts, closes := worker.counts(); starts != 0 || closes != 1 {
		t.Fatalf("after Close: starts=%d closes=%d, want 0 starts and 1 close", starts, closes)
	}
}

// TestClose_IsIdempotent pins the shutdown contract: the first call drains,
// every later call reports the same result without repeating a step.
func TestClose_IsIdempotent(t *testing.T) {
	var host testHostConfig
	worker := &testWorker{onClose: func(context.Context) error { return errors.New("worker drain failed") }}

	a, err := New(context.Background(), append(testBaseOptions(t, &host), WithWorker(worker))...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	first := a.Close(context.Background())
	if first == nil || !strings.Contains(first.Error(), "worker drain failed") {
		t.Fatalf("Close() error = %v, want the worker's drain failure", first)
	}
	second := a.Close(context.Background())
	if second == nil || second.Error() != first.Error() {
		t.Fatalf("second Close() error = %v, want the cached %v", second, first)
	}
	if _, closes := worker.counts(); closes != 1 {
		t.Fatalf("worker Close calls = %d, want exactly 1", closes)
	}
}

// TestNew_RefusesAMalformedPlatformCipher pins where the platform cipher is
// built and how a bad material is reported: the infrastructure stage refuses
// it, naming the declared key path the material belongs to.
func TestNew_RefusesAMalformedPlatformCipher(t *testing.T) {
	var host testHostConfig
	opts := testBaseOptions(t, &host)
	host.Config.Cipher_Key = []byte("too short")

	_, err := New(context.Background(), opts...)
	if err == nil {
		t.Fatal("New() with a malformed platform cipher error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "config.cipher_key") {
		t.Fatalf("cipher refusal does not name the declared key path: %v", err)
	}
	if !errors.Is(err, dbkit.ErrInvalidKeySize) {
		t.Fatalf("cipher refusal = %v, want it to wrap dbkit.ErrInvalidKeySize", err)
	}
}

// assertDatabaseClosed proves the handle is unusable, which is what a
// rollback promises: an open handle answers a ping, a closed one does not.
func assertDatabaseClosed(t *testing.T, db *gorm.DB) {
	t.Helper()
	if db == nil {
		t.Fatal("no database handle was captured; the test never reached the module stage")
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("reach the database handle: %v", err)
	}
	if err := sqlDB.Ping(); err == nil {
		t.Fatal("the database still answers a ping after the failed assembly, want it closed by the rollback")
	}
}
