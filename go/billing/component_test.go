package billing

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"

	"github.com/vislake/speed/go/billing/internal/testutil"
	"github.com/vislake/speed/go/billing/migrations"
)

// testUsageReader is a billing.UsageReader answering every live-usage
// question with zero, the shape a metering module's aggregator provides.
type testUsageReader struct{}

// RealtimeCount implements billing.UsageReader.
func (testUsageReader) RealtimeCount(_, _ string, _ time.Time) (float64, error) { return 0, nil }

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
// whose seats accept writes, so every declaration lands in the assembly's
// own seats and the module's handler is built over its own services.
func TestComponent_InitDeclaresThroughTheGate(t *testing.T) {
	db := testutil.NewSQLite(t, moduleName, migrations.FS)
	reg := pkgcore.NewComponentRegistry()
	if err := componenttest.RunInit(t, reg, component(), db, pkgcore.NewMemoryEventBus()); err != nil {
		t.Fatalf("RunInit: %v", err)
	}
	assertContainsAll(t, reg.Permissions.Permissions(), []string{
		PermissionPlanManage, PermissionSubscriptionRead, PermissionSubscriptionManage,
		PermissionCreditRead, PermissionCreditManage,
	})
	assertContainsAll(t, reg.AuditActions.Actions(), []string{
		AuditActionCreditGrant, AuditActionCreditDeductReserve, AuditActionCreditDeductConfirm,
		AuditActionCreditRefund, AuditActionCreditExpire,
	})
	var types []string
	for _, decl := range reg.Events.Published() {
		types = append(types, decl.Type)
	}
	assertContainsAll(t, types, []string{EventPlanChanged, EventSubscriptionStatusChanged})
	if routes := componenttest.FaceOf(reg).Routes(); len(routes) != 1 || routes[0].Path != apiPath {
		t.Fatalf("Init mounted %v, want exactly the %s mount", routes, apiPath)
	}
	// No queue was wired: Init must claim neither the poll handler nor its
	// schedule.
	if _, ok := reg.Jobs.Handlers()[taskTypePoll]; ok {
		t.Errorf("Init claimed the poll job handler without a queue")
	}
	if decls := reg.Schedules.Declarations(); len(decls) != 0 {
		t.Errorf("Init declared %v, want no schedule without a queue", decls)
	}
	m, err := pkgcore.Get[*Module](reg)
	if err != nil {
		t.Fatalf("the assembly's product: %v", err)
	}
	if m.handler == nil {
		t.Error("the module's HTTP handler was not built by Init")
	}
}

// TestComponentAssemblesThroughRegistry drives the registered descriptor
// through the assembly's stages the way a host would: selection from a
// composition configuration, construction from the database, usage-reader
// and queue products in the by-type context, the closing validation, and the
// shutdown sequence. It proves the declared dependencies and assets line up
// with what New actually consumes.
func TestComponentAssemblesThroughRegistry(t *testing.T) {
	ctx := context.Background()
	db := testutil.NewSQLite(t, moduleName, migrations.FS)
	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(testDBComponent(db)); err != nil {
		t.Fatalf("registering the database stand-in: %v", err)
	}
	reg.Put(testUsageReader{})
	reg.Put(jobs.NewStandaloneQueue(db))
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"deployment": "standalone",
		"components": map[string]any{
			"billing": map[string]any{},
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

// TestComponentConstructionRefusesBadInputs pins New's failure paths: a
// missing database product and an ambiguous optional dependency each fail
// the construction with an error rather than a half-built module.
func TestComponentConstructionRefusesBadInputs(t *testing.T) {
	ctx := context.Background()
	empty := pkgcore.NewComponentConfig(nil)

	t.Run("missing database", func(t *testing.T) {
		_, err := component().New(ctx, pkgcore.NewComponentRegistry(), empty)
		if err == nil {
			t.Fatal("construction proceeded without a database product")
		}
	})

	t.Run("ambiguous usage reader", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		if err := reg.Register(testDBComponent(testutil.NewSQLite(t, moduleName, migrations.FS))); err != nil {
			t.Fatalf("registering the database stand-in: %v", err)
		}
		reg.Put(testUsageReader{})
		reg.Put(testUsageReader{})
		_, err := component().New(ctx, reg, empty)
		if err == nil {
			t.Fatal("construction picked one of two usage readers instead of refusing the ambiguity")
		}
	})

	t.Run("ambiguous queue", func(t *testing.T) {
		db := testutil.NewSQLite(t, moduleName, migrations.FS)
		reg := pkgcore.NewComponentRegistry()
		if err := reg.Register(testDBComponent(db)); err != nil {
			t.Fatalf("registering the database stand-in: %v", err)
		}
		reg.Put(jobs.NewStandaloneQueue(db))
		reg.Put(jobs.NewStandaloneQueue(db))
		_, err := component().New(ctx, reg, empty)
		if err == nil {
			t.Fatal("construction picked one of two queue values instead of refusing the ambiguity")
		}
	})
}
