package http

import (
	"context"
	"errors"
	"net"
	nethttp "net/http"
	"reflect"
	"testing"

	"github.com/vislake/speed/pkg/config"
	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/log"
)

// testEngine builds the engine the descriptor is driven with. The standard
// library's multiplexer is what the shipped subpackage binds, so the tests here
// exercise the same seam a run does.
func testEngine() Engine { return nethttp.NewServeMux() }

// testModule is the descriptor under test.
func testModule() core.Module { return Module("http", testEngine) }

// testProduct builds the product the stage callbacks are driven with, from a
// stubbed reader.
func testProduct(t *testing.T, cfg moduleConfig) *router {
	t.Helper()
	logger, _ := newRecordingLogger()
	r, err := newModule(&stubReader{cfg: cfg}, testEngine, nil)
	if err != nil {
		t.Fatalf("building the product: %v", err)
	}
	r.logger = logger
	return r
}

// oneEndpoint is a configuration with a single endpoint on a loopback port the
// operating system picks.
func oneEndpoint(name string) moduleConfig {
	return moduleConfig{Endpoints: map[string]endpointConfig{name: {Address: "127.0.0.1:0"}}}
}

// TestPrepareDisablesWithReasonWhenNoEndpoints pins the stance a host with no
// endpoint configured gets. An entry point that quietly does nothing is the
// failure this stance exists to prevent, so the reason names the key to write.
func TestPrepareDisablesWithReasonWhenNoEndpoints(t *testing.T) {
	stance, err := prepare(&stubReader{}, nil)
	if err != nil {
		t.Fatalf("Prepare with no endpoints failed instead of standing down: %v", err)
	}
	if stance.State != core.StateDisabled {
		t.Fatalf("the stance is %v, and with no endpoint there is nothing to serve", stance.State)
	}
	mustContain(t, stance.Reason, "http.endpoints", "the configuration key to write")
}

// TestPrepareStatesEnabledNotAuto pins the pair that keeps this module from
// standing down. Resolution only disables a provider that claims exclusivity
// and states StateAuto; written as StateAuto, a second entry point would
// silently take the registrations meant for this one, and every other test here
// would still pass.
func TestPrepareStatesEnabledNotAuto(t *testing.T) {
	stance, err := prepare(&stubReader{cfg: oneEndpoint("public")}, nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if stance.State != core.StateEnabled {
		t.Errorf("the stance is %v and has to be %v", stance.State, core.StateEnabled)
	}
}

// TestPrepareValidatesDeclaredSpecs pins where a malformed fragment is caught.
// Left to the reader that merges the fragments, it would surface long after
// startup with nothing tying it back to the module that declared it.
func TestPrepareValidatesDeclaredSpecs(t *testing.T) {
	specs := []core.Resource[Spec]{{Module: "billing", Value: Spec{Endpoint: "public", Document: []byte(`{}`)}}}
	_, err := prepare(&stubReader{cfg: oneEndpoint("public")}, specs)
	if !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("a fragment without an openapi field did not fail Prepare: %v", err)
	}
	mustContain(t, err.Error(), "billing", "the module that declared the fragment")
}

// TestPrepareAndNewReadOneSection pins that the stance and the product are
// built from the same configuration path. Two paths would let a host's endpoint
// set decide one and not the other.
func TestPrepareAndNewReadOneSection(t *testing.T) {
	stance := &stubReader{cfg: oneEndpoint("public")}
	if _, err := prepare(stance, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	product := &stubReader{cfg: oneEndpoint("public")}
	if _, err := newModule(product, testEngine, nil); err != nil {
		t.Fatalf("New: %v", err)
	}
	if stance.path != configPath || product.path != configPath {
		t.Errorf("Prepare read %q and New read %q; both are this module's section %q",
			stance.path, product.path, configPath)
	}
}

// TestModuleProvidesRouterExclusively pins the delivery declaration. Two entry
// points at once leave a registrant unable to tell which one it registered
// with, and the exclusive claim is what turns that into a named conflict rather
// than a route nobody can reach.
func TestModuleProvidesRouterExclusively(t *testing.T) {
	provides := testModule().Provides
	if len(provides) != 1 {
		t.Fatalf("the module declares %d capabilities: %+v", len(provides), provides)
	}
	if got := reflect.TypeOf(provides[0].Token); got != reflect.TypeFor[*Router]() {
		t.Errorf("the capability delivered is %v, and it has to be (*Router)(nil)", got)
	}
	if !provides[0].Exclusive {
		t.Error("Router is delivered without the exclusive claim")
	}
}

// TestLoggerRequirementIsOptional pins the one declared dependency. Without the
// Optional flag, a host assembling no logging module would fail to start over a
// dependency this module states a default behaviour for.
func TestLoggerRequirementIsOptional(t *testing.T) {
	requires := testModule().Requires
	if len(requires) != 1 {
		t.Fatalf("the module declares %d requirements: %+v", len(requires), requires)
	}
	if got := reflect.TypeOf(requires[0].Token); got != reflect.TypeFor[*log.Logger]() {
		t.Errorf("the requirement is on %v, and it has to be (*log.Logger)(nil)", got)
	}
	if !requires[0].Optional {
		t.Error("the dependency on Logger is declared as mandatory")
	}

	injected, err := requestLogger(core.New(), "http")
	if err != nil {
		t.Fatalf("taking up the logger from a registry that has none: %v", err)
	}
	if injected != nil {
		t.Error("a logger was produced although no module delivers the capability")
	}
}

// TestRootPackageRegistersNothing pins that importing this package opens no
// port. A host chooses an entry point by importing the subpackage that binds
// the engine it wants; the registration surface and the request helpers come
// without one.
//
// Both names are looked up: "http" names this release unit and "http.stdmux"
// is what the shipped subpackage registers as, and neither may appear from an
// import of this package alone.
func TestRootPackageRegistersNothing(t *testing.T) {
	for _, name := range []string{"http", "http.stdmux"} {
		if _, found := core.ProcessRegistry.Lookup(name); found {
			t.Errorf("importing the root package registered a module named %q", name)
		}
	}
	for _, m := range core.ProcessRegistry.Modules() {
		for _, p := range m.Provides {
			if reflect.TypeOf(p.Token) == reflect.TypeFor[*Router]() {
				t.Errorf("module %q delivers Router although only a subpackage should", m.Name)
			}
		}
	}
}

// TestConfigIsUndeclaredAndStillRequired pins the one dependency no descriptor
// states. config is implicit for every module and is therefore absent from
// Requires, which does not make it optional: the endpoints are read from it and
// there is nothing to serve without them. A Prepare that swallowed the missing
// provider, or fell back to an empty endpoint set, would let a host that forgot
// the config module start up and stand down for "no endpoint configured",
// naming a configuration key instead of the module that has to deliver the
// reader.
func TestConfigIsUndeclaredAndStillRequired(t *testing.T) {
	m := testModule()
	for i, req := range m.Requires {
		if reflect.TypeOf(req.Token) == reflect.TypeFor[*config.Reader]() {
			t.Errorf("Requires[%d] declares config, which every module depends on implicitly", i)
		}
	}

	_, err := m.Prepare(context.Background(), core.New())
	if !errors.Is(err, core.ErrMissingProvider) {
		t.Fatalf("Prepare against a registry that has no config module: %v", err)
	}
	mustContain(t, err.Error(), "configuration", "what the startup is missing")
}

// TestImportingThisPackageCarriesALoggingModule pins the half of the optional
// dependency that makes it a statement of contract rather than a branch hosts
// reach: this package imports pkg/log, which registers itself at package
// initialisation, so every host that imports this module transitively carries a
// logging module. The absent case documented on Module stands only in a
// registry assembled by hand.
//
// Should the import go — or should pkg/log stop registering itself — the
// documented default behaviour would start applying to real hosts, and this is
// where that shows up.
func TestImportingThisPackageCarriesALoggingModule(t *testing.T) {
	for _, m := range core.ProcessRegistry.Modules() {
		for _, p := range m.Provides {
			if reflect.TypeOf(p.Token) == reflect.TypeFor[*log.Logger]() {
				return
			}
		}
	}
	t.Error("no module in the process registry delivers Logger, although importing this " +
		"package pulls pkg/log in and that registers itself")
}

// TestModuleWithoutAnEngineIsACallSiteError pins the seam's one requirement.
// Left to fail later, the nil would surface as a panic inside Serve, naming
// nothing about the subpackage that forgot to pass an engine.
func TestModuleWithoutAnEngineIsACallSiteError(t *testing.T) {
	text := wantPanic(t, "Module with no engine", func() { Module("http", nil) })
	mustContain(t, text, "engine", "what is missing")
}

// TestGateOpensInMigrateAndSealsInStart pins the two borrowed stages, which is
// what makes the window registrations are accepted in exactly the Init stage.
//
// Opening in this module's own Init would refuse a module that legally
// registers from an Init running before this one — Lookup does not filter by
// Requires, so such a module exists. Sealing in this module's own Serve would
// accept registrations through the whole Start stage and through every Serve
// callback ordered ahead of this module, none of which reach a chain.
func TestGateOpensInMigrateAndSealsInStart(t *testing.T) {
	m := testModule()
	product := testProduct(t, oneEndpoint("public"))
	e := endpointOf(t, product, "public")
	ctx := context.Background()

	wantPanic(t, "a registration before Migrate", func() {
		e.Route("GET /before", nethttp.NotFoundHandler())
	})

	if err := m.Migrate(ctx, nil, product); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// This stands for a module whose own Init runs before this one's: the
	// gate is open for the whole of the Init stage, not from this module's
	// own Init callback onwards.
	e.Route("GET /during", nethttp.NotFoundHandler())

	if err := m.Start(ctx, nil, product); err != nil {
		t.Fatalf("Start: %v", err)
	}
	wantPanic(t, "a registration during the Start stage", func() {
		e.Route("GET /after", nethttp.NotFoundHandler())
	})
}

// TestServeBindsAndTheEndpointServes drives the stages in order and pins that
// what comes out the other end really answers requests, with the chain and the
// routes that were registered.
func TestServeBindsAndTheEndpointServes(t *testing.T) {
	m := testModule()
	product := testProduct(t, oneEndpoint("public"))
	ctx := context.Background()
	t.Cleanup(func() { _ = m.Close(ctx, nil, product) })

	if err := m.Migrate(ctx, nil, product); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	e := endpointOf(t, product, "public")
	e.Route("GET /things", nethttp.HandlerFunc(func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		w.WriteHeader(nethttp.StatusTeapot)
	}))
	if err := m.Start(ctx, nil, product); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Serve(ctx, nil, product); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	if !e.Accepting() {
		t.Fatal("the endpoint reports that it is not accepting requests after Serve returned")
	}
	addr := product.endpoints["public"].socket.addr().String()
	resp, err := nethttp.Get("http://" + addr + "/things") //nolint:noctx // the test's own deadline applies
	if err != nil {
		t.Fatalf("the bound endpoint refused a request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != nethttp.StatusTeapot {
		t.Errorf("the registered handler answered %d, not the %d it writes",
			resp.StatusCode, nethttp.StatusTeapot)
	}

	if err := m.Stop(ctx, nil, product); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if e.Accepting() {
		t.Error("the endpoint still reports that it is accepting requests after Stop")
	}
	if err := m.Close(ctx, nil, product); err != nil {
		t.Errorf("Close after a clean stop: %v", err)
	}
}

// TestBindFailureReleasesTheEndpointsAlreadyBound pins the rollback across
// endpoints. All the endpoints share this module's lifecycle: one of them
// failing to bind ends the startup, and the ports the others took have to come
// back, or a restart fails on an address the dead process still holds.
func TestBindFailureReleasesTheEndpointsAlreadyBound(t *testing.T) {
	taken, takeErr := net.Listen("tcp", "127.0.0.1:0")
	if takeErr != nil {
		t.Fatalf("taking a port to collide with: %v", takeErr)
	}
	defer taken.Close()

	m := testModule()
	// The endpoints are bound in name order, so "a" comes up and "b" fails.
	product := testProduct(t, moduleConfig{Endpoints: map[string]endpointConfig{
		"a": {Address: "127.0.0.1:0"},
		"b": {Address: taken.Addr().String()},
	}})
	ctx := context.Background()

	if err := m.Migrate(ctx, nil, product); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := m.Start(ctx, nil, product); err != nil {
		t.Fatalf("Start: %v", err)
	}

	serveErr := m.Serve(ctx, nil, product)
	if !errors.Is(serveErr, ErrListen) {
		t.Fatalf("the second endpoint's bind failure did not end the startup: %v", serveErr)
	}
	first := product.endpoints["a"].socket.addr().String()
	if !product.endpoints["a"].Accepting() {
		t.Error("the endpoint that did bind is not accepting, so the failure was reported on the wrong one")
	}

	// This is what core does after a startup failure, over every constructed
	// instance whatever stage it reached.
	if err := m.Stop(ctx, nil, product); err != nil {
		t.Errorf("Stop during rollback: %v", err)
	}
	if err := m.Close(ctx, nil, product); err != nil {
		t.Errorf("Close during rollback: %v", err)
	}

	reopened, err := net.Listen("tcp", first)
	if err != nil {
		t.Fatalf("the port of the endpoint that did bind was not released: %v", err)
	}
	reopened.Close()
}

// TestStopAndCloseToleranceOnAnInstanceThatNeverServed pins the rollback
// obligation from the other side: a startup that fails before Serve still runs
// both callbacks over this module's product.
func TestStopAndCloseToleranceOnAnInstanceThatNeverServed(t *testing.T) {
	m := testModule()
	product := testProduct(t, oneEndpoint("public"))
	ctx := context.Background()

	if err := m.Stop(ctx, nil, product); err != nil {
		t.Errorf("Stop on a product that never served: %v", err)
	}
	if err := m.Close(ctx, nil, product); err != nil {
		t.Errorf("Close on a product that never served: %v", err)
	}
}

// TestStageCallbacksTolerateAForeignInstance pins the same obligation for the
// instance itself: rollback runs the callbacks on modules whose New never
// returned a product.
func TestStageCallbacksTolerateAForeignInstance(t *testing.T) {
	m := testModule()
	ctx := context.Background()
	for name, call := range map[string]func(any) error{
		"Migrate": func(i any) error { return m.Migrate(ctx, nil, i) },
		"Start":   func(i any) error { return m.Start(ctx, nil, i) },
		"Serve":   func(i any) error { return m.Serve(ctx, nil, i) },
		"Stop":    func(i any) error { return m.Stop(ctx, nil, i) },
		"Close":   func(i any) error { return m.Close(ctx, nil, i) },
	} {
		for _, instance := range []any{nil, (*router)(nil), "not a router"} {
			if err := call(instance); err != nil {
				t.Errorf("%s on the instance %#v: %v", name, instance, err)
			}
		}
	}
}
