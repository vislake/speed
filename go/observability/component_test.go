package observability

// component_test.go carries the component's own contract: the registration
// the package's init performs, the lifecycle the assembly drives (Prepare
// initializes the providers and publishes the runtime, New hands the runtime
// back, Close shuts the providers down), and the refusal surfaces on the
// way. It lives in the internal observability package (like
// factory_vars_test.go, and unlike the external observability_test package
// every other test file uses) because the lifecycle and the refusals reach
// the component's unexported callbacks and types; what a successful Init
// installs stays go/observability's own contract, pinned by the external
// suite.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// observabilityComposition returns a composition configuration whose
// components block is exactly the given entries.
func observabilityComposition(entries map[string]any) pkgcore.ComponentConfig {
	return pkgcore.NewComponentConfig(map[string]any{"components": entries})
}

// nilOTLPFactory removes the registered OTLP exporter factory for the
// duration of one test, the state a host that never wants OTLP is in. This
// test binary blank-imports exporter/otlp for the external suite's own
// success-path tests (init_test.go), so the refusal can only be reproduced
// by unregistering it, and only package-internal code can restore the
// package variable.
func nilOTLPFactory(t *testing.T) {
	t.Helper()
	previous := otlpFactory
	otlpFactory = nil
	t.Cleanup(func() { otlpFactory = previous })
}

// TestObservabilityComponent_SelfRegisters pins the package's init: every
// binary importing go/observability carries exactly one component named
// "observability" in the global registration -- the name the loader's
// builtin composition selects -- and its descriptor satisfies the component
// contract.
func TestObservabilityComponent_SelfRegisters(t *testing.T) {
	var found int
	for _, c := range pkgcore.GlobalComponents() {
		if c.Name != "observability" {
			continue
		}
		found++
		componenttest.AssertWellFormed(t, c)
	}
	if found != 1 {
		t.Fatalf("the global registration carries %d components named %q, want exactly 1", found, "observability")
	}
}

// TestObservabilityComponent_RunsItsWholeLifecycle drives the component
// through the stages its descriptor fills: selected with its configuration
// block, its Prepare initializes the providers and publishes the runtime,
// Construct builds the instance carrying that runtime's shutdown function,
// and Close shuts the providers down.
func TestObservabilityComponent_RunsItsWholeLifecycle(t *testing.T) {
	ctx := context.Background()
	reg := pkgcore.NewComponentRegistry()
	reg.Put(observabilityComposition(map[string]any{
		"observability": map[string]any{"service_name": "observability-component-test"},
	}))

	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare() with the observability component selected error = %v", err)
	}
	if _, err := pkgcore.Get[*observabilityRuntime](reg); err != nil {
		t.Fatalf("Get[*observabilityRuntime] error = %v, want the runtime its Prepare published", err)
	}
	if err := reg.Construct(ctx); err != nil {
		t.Fatalf("Construct() error = %v", err)
	}
	instance, err := pkgcore.Get[*observabilityInstance](reg)
	if err != nil {
		t.Fatalf("Get[*observabilityInstance] error = %v, want the constructed instance", err)
	}
	if instance.shutdown == nil {
		t.Error("the constructed instance carries no shutdown function, want the one Prepare's Init returned")
	}
	if err := reg.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v, want the providers flushed cleanly", err)
	}
}

// TestObservabilityComponent_RefusesAnUnknownConfigurationKey pins the
// strict decode: the component's Prepare fails on a key outside its schema,
// naming the key, before any provider is built.
func TestObservabilityComponent_RefusesAnUnknownConfigurationKey(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	reg.Put(observabilityComposition(map[string]any{
		"observability": map[string]any{"unknown_key": "x"},
	}))

	err := reg.Prepare(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unknown_key") {
		t.Fatalf("Prepare() with an unknown observability key error = %v, want one naming the key", err)
	}
}

// TestObservabilityComponent_RefusesAnOTLPEndpointWithNoExporterRegistered
// pins the wiring refusal Init reports: an endpoint without a registered
// OTLP exporter fails the component's Prepare, carrying
// ErrOTLPExporterNotRegistered -- the blank import that fixes it is named on
// the error.
func TestObservabilityComponent_RefusesAnOTLPEndpointWithNoExporterRegistered(t *testing.T) {
	nilOTLPFactory(t)

	reg := pkgcore.NewComponentRegistry()
	reg.Put(observabilityComposition(map[string]any{
		"observability": map[string]any{"otlp_endpoint": "127.0.0.1:4317"},
	}))

	err := reg.Prepare(context.Background())
	if !errors.Is(err, ErrOTLPExporterNotRegistered) {
		t.Fatalf("Prepare() with an OTLP endpoint and no exporter error = %v, want ErrOTLPExporterNotRegistered", err)
	}
}

// TestObservabilityComponent_RenamedCopyReadsItsOwnConfigurationBlock is the
// overlay pin: a host that registers a renamed copy of the module's
// descriptor selects the COPY in its composition, and the copy's own
// configuration block must be the one its Prepare consumes. The endpoint in
// the copy's block reaching Init is observable through the same refusal the
// test above pins -- a mis-read block would leave the endpoint unset, Init
// would succeed, and this assertion would fail. A literal
// components.observability lookup (the shape this test exists against)
// reads the original's block -- deselected here -- instead of the copy's,
// silently dropping the configuration the host actually wrote.
func TestObservabilityComponent_RenamedCopyReadsItsOwnConfigurationBlock(t *testing.T) {
	nilOTLPFactory(t)

	reg := pkgcore.NewComponentRegistry()
	var base pkgcore.Component
	for _, c := range pkgcore.RegisteredComponents(reg) {
		if c.Name == "observability" {
			base = c
			break
		}
	}
	if base.Name == "" {
		t.Fatal("the module's observability component is not registered")
	}
	renamed := base
	renamed.Name = "test-host.observability"
	if err := reg.Register(renamed); err != nil {
		t.Fatalf("registering the renamed copy: %v", err)
	}
	reg.Put(observabilityComposition(map[string]any{
		"observability":           false,
		"test-host.observability": map[string]any{"otlp_endpoint": "127.0.0.1:4317"},
	}))

	err := reg.Prepare(context.Background())
	if !errors.Is(err, ErrOTLPExporterNotRegistered) {
		t.Fatalf("Prepare() with a renamed observability copy carrying an OTLP endpoint error = %v, want ErrOTLPExporterNotRegistered: the copy's own block must be the one its Prepare consumes", err)
	}
}

// TestNewObservability_RequiresItsPrepareRuntime pins the construction
// refusal: the instance is built from the runtime Prepare publishes, so a
// registry without one fails the read naming the missing step.
func TestNewObservability_RequiresItsPrepareRuntime(t *testing.T) {
	_, err := newObservability(context.Background(), pkgcore.NewComponentRegistry(), pkgcore.ComponentConfig{})
	if err == nil || !strings.Contains(err.Error(), "runtime is missing") {
		t.Fatalf("newObservability() without a published runtime error = %v, want one naming the runtime", err)
	}
}

// TestCloseObservability_RefusesAForeignInstance pins the type guard: an
// instance that is not this component's own is refused, never closed blind.
func TestCloseObservability_RefusesAForeignInstance(t *testing.T) {
	err := closeObservability(context.Background(), nil, struct{}{})
	if err == nil || !strings.Contains(err.Error(), "component holds an instance") {
		t.Fatalf("closeObservability() with a foreign instance error = %v, want the type refusal", err)
	}
}

// TestCloseObservability_WithoutAShutdownFunctionIsANoOp pins the empty
// half: an instance whose shutdown function is nil closes cleanly, so a
// Close that reaches it never fails the assembly over nothing to shut down.
func TestCloseObservability_WithoutAShutdownFunctionIsANoOp(t *testing.T) {
	if err := closeObservability(context.Background(), nil, &observabilityInstance{}); err != nil {
		t.Fatalf("closeObservability() with a nil shutdown function error = %v, want nil", err)
	}
}
