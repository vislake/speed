package audit

import (
	"context"
	"slices"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

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
// implements and the token shapes, exactly as componenttest asserts them for
// a component package.
func TestComponentWellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, component())
}

// TestComponent_InitDeclaresThroughTheGate drives the descriptor's Init
// through a real assembly: the module's Register runs inside the one stage
// whose seats accept writes, so its two declarations land in the assembly's
// own seats and its three subscriptions are installed on the assembly's own
// bus.
func TestComponent_InitDeclaresThroughTheGate(t *testing.T) {
	db := openAuditTestDB(t)
	bus := pkgcore.NewMemoryEventBus()
	reg := pkgcore.NewComponentRegistry()
	if err := componenttest.RunInit(t, reg, component(), db, bus); err != nil {
		t.Fatalf("RunInit: %v", err)
	}
	if actions := reg.AuditActions.Actions(); !slices.Contains(actions, AuditActionSystemContextEntered) {
		t.Errorf("AuditActions seat = %v, want the system-context-enter action", actions)
	}
	var types []string
	for _, decl := range reg.Events.Published() {
		types = append(types, decl.Type)
	}
	for _, want := range []string{dbkit.EventWriteCaptured, EventRecorded} {
		if !slices.Contains(types, want) {
			t.Errorf("Events seat = %v, want the %q declaration", types, want)
		}
	}
	m, err := pkgcore.Get[*Module](reg)
	if err != nil {
		t.Fatalf("the assembly's product: %v", err)
	}
	if m.actions == nil {
		t.Error("the module did not take the assembly's AuditActions registrar")
	}
}

// TestComponentAssemblesThroughRegistry drives the registered descriptor
// through the assembly's stages the way a host would: selection from a
// composition configuration, construction from the database product in the
// by-type context, the closing validation, and the shutdown sequence. It
// proves the declared dependencies and assets line up with what New actually
// consumes.
func TestComponentAssemblesThroughRegistry(t *testing.T) {
	ctx := context.Background()
	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(testDBComponent(openAuditTestDB(t))); err != nil {
		t.Fatalf("registering the database stand-in: %v", err)
	}
	// The Init callback installs the module's three subscriptions during the
	// assembly's Init stage, so the assembly carries the bus they land on.
	reg.Put(pkgcore.NewMemoryEventBus())
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"deployment": "standalone",
		"components": map[string]any{
			"audit":   map[string]any{},
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

// TestComponentConstructionRefusesMissingDatabase pins New's failure path:
// a registry with no database product fails the construction with an error
// rather than a half-built module.
func TestComponentConstructionRefusesMissingDatabase(t *testing.T) {
	_, err := component().New(context.Background(), pkgcore.NewComponentRegistry(), pkgcore.NewComponentConfig(nil))
	if err == nil {
		t.Fatal("construction proceeded without a database product")
	}
}
