package config

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
	"github.com/vislake/speed/go/tenancy"
)

// testResolver is a tenancy.Resolver standing in for the host's
// host-to-tenant map: every request resolves to the empty tenant.
type testResolver struct{}

// Resolve implements tenancy.Resolver.
func (testResolver) Resolve(*http.Request) (pkgcore.TenantID, error) { return "", nil }

// testCipher builds a cipher over a fixed 32-byte key, the shape a host's
// config.cipher_key material produces.
func testCipher(t *testing.T) *dbkit.Cipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	cipher, err := dbkit.NewCipher(key)
	if err != nil {
		t.Fatalf("building the test cipher: %v", err)
	}
	return cipher
}

// testDBComponent is a stand-in for the database component a real assembly
// selects: it declares the *gorm.DB product and constructs the test's
// migrated handle, so the descriptor's declared database dependency resolves
// exactly as it will against the real db component.
func testDBComponent(db *gorm.DB) pkgcore.Component {
	return pkgcore.Component{
		Name:     "test.db",
		Module:   "test",
		Provides: []any{(*gorm.DB)(nil)},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return db, nil
		},
	}
}

// TestComponentWellFormed pins the descriptor's contract: the naming
// convention, the relation between the component name and the module it
// implements, the configuration schema's decodability and the token shapes,
// exactly as componenttest asserts them for a component package.
func TestComponentWellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, component())
}

// TestComponent_StartDeclaresAndPublishesTheService drives the descriptor
// through a real assembly: Register runs inside the one stage whose seats
// accept writes, so its declarations land in the assembly's own seats; the
// Start callback then takes the full-catalog schema snapshot -- every
// component's Init turn has run by then -- and publishes the runtime
// *Service into the by-type context, where a consumer reads it.
func TestComponent_StartDeclaresAndPublishesTheService(t *testing.T) {
	db := openModuleTestDB(t)
	reg := pkgcore.NewComponentRegistry()
	bus := pkgcore.NewMemoryEventBus()
	if err := componenttest.RunInit(t, reg, component(), db, bus, pkgcore.NewMemoryKVStore()); err != nil {
		t.Fatalf("RunInit: %v", err)
	}
	// No service is published before Start: the snapshot must not be taken
	// over a declaration set that is still growing.
	if _, err := pkgcore.Get[*Service](reg); err == nil {
		t.Error("a service is published before the Start stage; the snapshot must wait for the complete declaration set")
	}
	if err := reg.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if actions := reg.AuditActions.Actions(); !slices.Contains(actions, AuditActionConfigSet) {
		t.Errorf("AuditActions seat = %v, want the config-set action", actions)
	}
	var types []string
	for _, decl := range reg.Events.Published() {
		types = append(types, decl.Type)
	}
	if !slices.Contains(types, EventConfigItemChanged) {
		t.Errorf("Events seat = %v, want the item-changed event declaration", types)
	}
	paths := make([]string, 0, 2)
	for _, route := range reg.Routes.Routes() {
		paths = append(paths, route.Path)
	}
	if !slices.Contains(paths, PathPublic) || !slices.Contains(paths, PathSystemFeatures) {
		t.Errorf("Routes seat = %v, want the two pre-auth mounts", paths)
	}

	// The Service is the Start stage's publication: reachable from the
	// assembly's by-type context, carrying the schema frozen over the
	// declarations above and the assembly's own bus and store.
	svc, err := pkgcore.Get[*Service](reg)
	if err != nil {
		t.Fatalf("the published service is not reachable: %v", err)
	}
	if svc.schema.Load() == nil {
		t.Error("the published service carries no schema snapshot")
	}
	if svc.bus != pkgcore.EventBus(bus) {
		t.Error("the service did not take the assembly's bus")
	}
	if svc.kv == nil {
		t.Error("the service did not take the assembly's key-value store")
	}
}

// TestComponent_StartCompletesTheSnapshotOverEveryDeclaration pins the
// stage boundary the schema snapshot needs. A module that declares its
// configuration item in its own Init callback, planned AFTER config's turn,
// must still land in the runtime schema: the snapshot is taken at Start --
// after every Init callback has run -- not at config's Init turn, so plan
// order cannot silently shrink the schema.
func TestComponent_StartCompletesTheSnapshotOverEveryDeclaration(t *testing.T) {
	ctx := context.Background()
	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(testDBComponent(openModuleTestDB(t))); err != nil {
		t.Fatalf("registering the database stand-in: %v", err)
	}
	// The late declarer runs after config (it is listed after it in the
	// composition) and declares its one item only when its own Init
	// callback runs.
	late := pkgcore.Component{
		Name:   "test.late",
		Module: "test",
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &struct{}{}, nil
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
			return reg.ConfigSeat().Add(pkgcore.ConfigItem{Key: "test.late.key", Type: "string", Description: "declared after config's own Init turn"})
		},
	}
	if err := reg.Register(late); err != nil {
		t.Fatalf("registering the late declarer: %v", err)
	}
	reg.Put(pkgcore.NewMemoryEventBus())
	reg.Put(pkgcore.NewMemoryKVStore())
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"deployment": "standalone",
		"strict":     true,
		"components": map[string]any{
			"config":    nil,
			"test.db":   nil,
			"test.late": nil,
		},
	}))

	for _, stage := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"prepare", reg.Prepare},
		{"construct", reg.Construct},
		{"verify", reg.Verify},
		{"init", reg.Init},
		{"start", reg.Start},
	} {
		if err := stage.run(ctx); err != nil {
			t.Fatalf("%s: %v", stage.name, err)
		}
	}

	svc, err := pkgcore.Get[*Service](reg)
	if err != nil {
		t.Fatalf("the published service is not reachable: %v", err)
	}
	var keys []string
	for _, item := range svc.Describe() {
		keys = append(keys, item.Key)
	}
	if !slices.Contains(keys, "test.late.key") {
		t.Errorf("the runtime schema %v misses the declaration made after config's own Init turn; the snapshot must be taken at Start, when every Init callback has run", keys)
	}
}

// TestComponentAssemblesThroughRegistry drives the registered descriptor
// through the assembly's stages the way a host would: selection from a
// composition configuration, construction from the database, cipher and
// resolver values in the by-type context, the closing validation, and the
// shutdown sequence. It proves the declared dependencies, assets and
// configuration schema line up with what New actually consumes.
func TestComponentAssemblesThroughRegistry(t *testing.T) {
	ctx := context.Background()
	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(testDBComponent(openModuleTestDB(t))); err != nil {
		t.Fatalf("registering the database stand-in: %v", err)
	}
	reg.Put(testCipher(t))
	reg.Put(testResolver{})
	// The Init callback runs Attach during the assembly's Init stage:
	// Attach installs the service's own subscription and reads the
	// assembly's seam values, so the assembly carries a bus and a store.
	reg.Put(pkgcore.NewMemoryEventBus())
	reg.Put(pkgcore.NewMemoryKVStore())
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"deployment": "standalone",
		"components": map[string]any{
			"config":  map[string]any{"poll_interval": "1m"},
			"test.db": nil,
		},
	}))

	for _, stage := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"prepare", reg.Prepare},
		{"construct", reg.Construct},
		{"verify", reg.Verify},
		{"init", reg.Init},
		{"start", reg.Start},
	} {
		if err := stage.run(ctx); err != nil {
			t.Fatalf("%s: %v", stage.name, err)
		}
	}

	module, err := pkgcore.Get[*Module](reg)
	if err != nil {
		t.Fatalf("the assembled product is not reachable: %v", err)
	}
	if module == nil {
		t.Fatal("the assembled product is nil")
	}
	if names := pkgcore.MemberNames(reg, moduleName); len(names) != 1 || names[0] != moduleName {
		t.Fatalf("MemberNames = %v, want [%s]", names, moduleName)
	}
	if assets := pkgcore.Assets(reg); len(assets) != 1 || assets[0].Name != moduleName {
		t.Fatalf("Assets = %v, want exactly the %s entry", assets, moduleName)
	}

	if err := reg.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := reg.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestComponentConstructionRefusesBadInputs pins New's failure paths: an
// undeclared configuration key, a missing database product and an ambiguous
// optional dependency each fail the construction with an error rather than a
// half-built module.
func TestComponentConstructionRefusesBadInputs(t *testing.T) {
	ctx := context.Background()
	empty := pkgcore.NewComponentConfig(nil)

	t.Run("undeclared configuration key", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		if err := reg.Register(testDBComponent(openModuleTestDB(t))); err != nil {
			t.Fatalf("registering the database stand-in: %v", err)
		}
		_, err := component().New(ctx, reg, pkgcore.NewComponentConfig(map[string]any{"bogus": "x"}))
		if err == nil {
			t.Fatal("construction accepted an undeclared configuration key")
		}
	})

	t.Run("missing database", func(t *testing.T) {
		_, err := component().New(ctx, pkgcore.NewComponentRegistry(), empty)
		if err == nil {
			t.Fatal("construction proceeded without a database product")
		}
	})

	t.Run("ambiguous cipher", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		// Construction reads the by-type context, so the database goes in as
		// a value: the two ciphers must be the ambiguity the read reaches,
		// never masked by a missing database one check earlier.
		reg.Put(openModuleTestDB(t))
		reg.Put(testCipher(t))
		reg.Put(testCipher(t))
		_, err := component().New(ctx, reg, empty)
		if err == nil {
			t.Fatal("construction picked one of two cipher values instead of refusing the ambiguity")
		}
	})

	t.Run("ambiguous resolver", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		reg.Put(openModuleTestDB(t))
		reg.Put(testCipher(t))
		reg.Put(testResolver{})
		reg.Put(testResolver{})
		_, err := component().New(ctx, reg, empty)
		if err == nil {
			t.Fatal("construction picked one of two resolver values instead of refusing the ambiguity")
		}
	})
}

// TestComponent_CallbacksRefuseAForeignInstance pins the descriptor's type
// guards: Init and Start each refuse an instance that is not the *Module New
// builds, rather than reading fields off whatever value the assembly handed
// the callback.
func TestComponent_CallbacksRefuseAForeignInstance(t *testing.T) {
	comp := component()
	reg := pkgcore.NewComponentRegistry()
	if err := comp.Init(context.Background(), reg, &struct{}{}); err == nil {
		t.Error("Init accepted a foreign instance; it accepts only the *config.Module its New built")
	}
	if err := comp.Start(context.Background(), reg, &struct{}{}); err == nil {
		t.Error("Start accepted a foreign instance; it accepts only the *config.Module its New built")
	}
}

// TestComponent_Start_ReportsASnapshotFailure pins Start's error path: a
// module whose snapshot cannot be taken -- here one that was never given a
// database, so its attach cannot open the configs table -- fails the Start
// callback with that error instead of reporting success over a service
// nothing published.
func TestComponent_Start_ReportsASnapshotFailure(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	if err := component().Start(context.Background(), reg, NewModule(nil, WithPollInterval(0))); err == nil {
		t.Fatal("Start succeeded for a module whose snapshot cannot be taken")
	}
}

// compile-time check that testResolver satisfies tenancy.Resolver.
var _ tenancy.Resolver = testResolver{}
