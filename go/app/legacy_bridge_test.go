package app

import (
	"context"
	"embed"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// bridgeModule is a legacy-style module fixture for the bridge's
// conformance test: it records its registration order and declares a route
// the test reads back out of the assembly's own seat.
type bridgeModule struct {
	name      string
	dependsOn []string
	order     *[]string
	mounted   string
	handler   http.Handler
	keys      []pkgcore.BootstrapKey
}

func (m *bridgeModule) Name() string               { return m.name }
func (m *bridgeModule) DependsOn() []string        { return m.dependsOn }
func (m *bridgeModule) Migrations() embed.FS       { return embed.FS{} }
func (m *bridgeModule) Locales() embed.FS          { return embed.FS{} }
func (m *bridgeModule) OpenAPISpec() []byte        { return nil }
func (m *bridgeModule) Register(reg *pkgcore.Registry) error {
	*m.order = append(*m.order, m.name)
	if len(m.keys) > 0 {
		if err := reg.Bootstrap.Add(m.keys...); err != nil {
			return err
		}
	}
	if m.mounted != "" {
		reg.Routes.Mount(m.mounted, m.handler)
	}
	return nil
}

// bridgeModuleA and bridgeModuleB make the two fixtures distinct Go types,
// as real modules are: the wrapped components' products (the module values
// themselves) must stay individually addressable through the by-type
// context.
type bridgeModuleA struct{ bridgeModule }
type bridgeModuleB struct{ bridgeModule }

// TestBridge_WrapsLegacyModulesThroughTheSevenStages is the bridge's
// conformance test: two old-style modules with a DependsOn edge, handed to
// the engine in the dependent-first order, run through the component
// assembly's seven stages -- registration in Init (once, dependency-ordered
// despite the input order), declarations landing in the assembly's own
// seats, products in the by-type context, and a clean two-phase close.
func TestBridge_WrapsLegacyModulesThroughTheSevenStages(t *testing.T) {
	var host testHostConfig
	var order []string

	dependent := &bridgeModuleA{bridgeModule{
		name:      "dependent",
		dependsOn: []string{"base"},
		order:     &order,
		mounted:   "/api/v1/bridge",
		handler:   staticHandler("bridge"),
	}}
	base := &bridgeModuleB{bridgeModule{
		name:  "base",
		order: &order,
		keys:  []pkgcore.BootstrapKey{{Key: "token", Format: "string"}},
	}}

	a, err := New(context.Background(), append(testBaseOptions(t, &host),
		WithModules(func(context.Context, ModuleDeps) ([]pkgcore.Module, error) {
			// Deliberately dependent-first: DependsOn must reorder the
			// registration.
			return []pkgcore.Module{dependent, base}, nil
		}),
	)...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	// Registration ran exactly once per module, in dependency order.
	if !slices.Equal(order, []string{"base", "dependent"}) {
		t.Fatalf("module registration order = %v, want the DependsOn edge to order base before dependent", order)
	}

	// The declarations landed in the component assembly's own seats, and the
	// module Registry view reads them back.
	routes := a.reg.MountedRoutes()
	if len(routes) != 1 || routes[0].Path != "/api/v1/bridge" {
		t.Fatalf("assembly routes = %v, want the wrapped module's one route", routes)
	}
	if got := a.Registry().MountedRoutes(); len(got) != 1 || got[0].Path != "/api/v1/bridge" {
		t.Fatalf("registry view routes = %v, want the same declarations", got)
	}
	declared := a.Registry().Bootstrap.Keys()
	if len(declared) != 1 || declared[0].Key != "token" {
		t.Fatalf("view bootstrap keys = %v, want the wrapped module's declaration", declared)
	}

	// Every wrapped module's product is reachable through the by-type
	// context, under its own concrete type.
	if got, err := pkgcore.Get[*bridgeModuleA](a.reg); err != nil || got != dependent {
		t.Fatalf("Get[*bridgeModuleA] = %v (err %v), want the wrapped dependent module", got, err)
	}
	if got, err := pkgcore.Get[*bridgeModuleB](a.reg); err != nil || got != base {
		t.Fatalf("Get[*bridgeModuleB] = %v (err %v), want the wrapped base module", got, err)
	}

	// The registry registers both wrappers under the legacy prefix, with the
	// modules' assets forwarded.
	names := make([]string, 0, 2)
	for _, c := range pkgcore.RegisteredComponents(a.reg) {
		if c.Name == "legacy.base" || c.Name == "legacy.dependent" {
			names = append(names, c.Name)
		}
	}
	if len(names) != 2 {
		t.Fatalf("registered components = %v, want both wrapped modules under the legacy prefix", names)
	}

	// Serving works through the composed face, and the two-phase close runs
	// exactly once.
	rec := serveTo(t, a.Handler(), http.MethodGet, "/api/v1/bridge")
	if rec.Code != http.StatusOK || rec.Body.String() != "bridge" {
		t.Fatalf("GET /api/v1/bridge: status %d body %q, want the wrapped module's answer", rec.Code, rec.Body.String())
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("second Close() error = %v, want the cached result", err)
	}
	if !slices.Equal(order, []string{"base", "dependent"}) {
		t.Fatalf("registration order after close = %v, want registration to have run exactly once per module", order)
	}
}

// TestBridge_WrapsADanglingDependencyRefusal pins the DependsOn mapping's
// failure: a dependency naming a module outside the composed set fails the
// assembly, mirroring the module bootstrap's own rule.
func TestBridge_WrapsADanglingDependencyRefusal(t *testing.T) {
	var host testHostConfig
	var order []string
	module := &bridgeModuleA{bridgeModule{
		name:      "lonely",
		dependsOn: []string{"absent"},
		order:     &order,
	}}

	_, err := New(context.Background(), append(testBaseOptions(t, &host),
		WithModules(func(context.Context, ModuleDeps) ([]pkgcore.Module, error) {
			return []pkgcore.Module{module}, nil
		}),
	)...)
	if err == nil {
		t.Fatal("New() with a dangling DependsOn error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), `"lonely"`) || !strings.Contains(err.Error(), `"absent"`) {
		t.Fatalf("dangling-dependency refusal = %v, want it to name both modules", err)
	}
}
