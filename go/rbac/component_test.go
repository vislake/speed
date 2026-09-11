package rbac

import (
	"context"
	"slices"
	"testing"

	"gorm.io/gorm"

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
// implements, the configuration schema's decodability and the token shapes,
// exactly as componenttest asserts them for a component package.
func TestComponentWellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, component())
}

// TestComponent_InitDeclaresAndPublishesTheService drives the descriptor's
// Init through a real assembly: Register runs inside the one stage whose
// seats accept writes, so its declarations land in the assembly's own
// seats; Attach then runs in the same stage -- the only stage that can
// carry its seat writes -- and the runtime *Service it builds is put into
// the by-type context, where a consumer reads it after the assembly.
func TestComponent_InitDeclaresAndPublishesTheService(t *testing.T) {
	db := newRBACTestDB(t)
	reg := pkgcore.NewComponentRegistry()
	bus := pkgcore.NewMemoryEventBus()
	if err := componenttest.RunInit(t, reg, component(), db, bus); err != nil {
		t.Fatalf("RunInit: %v", err)
	}
	for _, want := range []string{PermissionRead, PermissionManage} {
		if !slices.Contains(reg.Permissions.Permissions(), want) {
			t.Errorf("Permissions seat = %v, want the %q declaration", reg.Permissions.Permissions(), want)
		}
	}
	var types []string
	for _, decl := range reg.Events.Published() {
		types = append(types, decl.Type)
	}
	for _, want := range []string{EventRoleBindingAssigned, EventRoleBindingRevoked, EventRoleChanged} {
		if !slices.Contains(types, want) {
			t.Errorf("Events seat = %v, want the %q declaration", types, want)
		}
	}

	svc, err := pkgcore.Get[*Service](reg)
	if err != nil {
		t.Fatalf("the published service is not reachable: %v", err)
	}
	// The catalog is the Init stage's snapshot: it carries the permissions
	// declared before this component's turn, rbac's own included.
	if !svc.catalog.Load().Has(PermissionRead) || !svc.catalog.Load().Has(PermissionManage) {
		t.Errorf("the published service's catalog lacks the module's own permissions: %v", svc.catalog.Load().permissions())
	}
	if svc.bus != pkgcore.EventBus(bus) {
		t.Error("the service did not take the assembly's bus")
	}
}

// TestComponent_StartCompletesTheSnapshotOverEveryDeclaration pins the stage
// boundary the catalog snapshot needs. A module that declares its permission
// in its own Init callback, planned AFTER rbac's turn, must still land in
// the service's catalog: the snapshot is completed at Start -- after every
// Init callback has run -- so a snapshot taken at rbac's own Init turn
// cannot silently shrink the catalog the grants validate against.
func TestComponent_StartCompletesTheSnapshotOverEveryDeclaration(t *testing.T) {
	ctx := context.Background()
	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(testDBComponent(newRBACTestDB(t))); err != nil {
		t.Fatalf("registering the database stand-in: %v", err)
	}
	// The late declarer runs after rbac (it is listed after it in the
	// composition) and declares its permission only when its own Init
	// callback runs.
	const latePermission = "test.late:read"
	late := pkgcore.Component{
		Name:   "test.late",
		Module: "test",
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &struct{}{}, nil
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
			return reg.PermissionsSeat().Add(latePermission)
		},
	}
	if err := reg.Register(late); err != nil {
		t.Fatalf("registering the late declarer: %v", err)
	}
	reg.Put(pkgcore.NewMemoryEventBus())
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"deployment": "standalone",
		"strict":     true,
		"components": map[string]any{
			"rbac":      map[string]any{},
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
	declared := svc.DeclaredPermissions()
	if !slices.Contains(declared, latePermission) {
		t.Errorf("the service's catalog %v misses the declaration made after rbac's own Init turn; the snapshot must be completed at Start, when every Init callback has run", declared)
	}
}

// TestComponentAssemblesThroughRegistry drives the registered descriptor
// through the assembly's stages the way a host would: selection from a
// composition configuration, construction from the database product in the
// by-type context, the closing validation, and the shutdown sequence. It
// proves the declared dependencies, assets and configuration schema line up
// with what New actually consumes.
func TestComponentAssemblesThroughRegistry(t *testing.T) {
	ctx := context.Background()
	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(testDBComponent(newRBACTestDB(t))); err != nil {
		t.Fatalf("registering the database stand-in: %v", err)
	}
	// The Init callback runs Attach during the assembly's Init stage:
	// Attach installs the service's subscriptions and job handlers, so the
	// assembly carries the bus they land on.
	reg.Put(pkgcore.NewMemoryEventBus())
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"deployment": "standalone",
		"components": map[string]any{
			"rbac":    map[string]any{},
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
