package app

// The observability component's failure and teardown surfaces: the refusals
// its Prepare reports before a provider pair is built, and the two halves
// its Close distinguishes. What a successful Init installs is
// go/observability's own contract, pinned by that package's suite.

import (
	"context"
	"errors"
	"strings"
	"testing"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// observabilityBlockSpec returns a load spec over the test fixtures whose
// composition selects the observability component with block.
func observabilityBlockSpec(t *testing.T, block pkgcore.ComponentConfig) LoadSpec {
	t.Helper()
	var host testHostConfig
	spec := driverLoadSpec(t, &host)
	spec.Overrides = &CompositionOverrides{Config: pkgcore.ComponentConfig{}.With("components",
		pkgcore.ComponentConfig{}.With("observability", block))}
	return spec
}

// TestAssemble_RefusesAnObservabilityBlockWithAnUnknownKey pins the strict
// decode: the component's Prepare fails on a key outside its schema, naming
// the key, before any provider is built.
func TestAssemble_RefusesAnObservabilityBlockWithAnUnknownKey(t *testing.T) {
	spec := observabilityBlockSpec(t, pkgcore.ComponentConfig{}.With("unknown_key", "x"))
	err := Assemble(context.Background(), pkgcore.NewComponentRegistry(), spec)
	if err == nil || !strings.Contains(err.Error(), "unknown_key") {
		t.Fatalf("Assemble() with an unknown observability key error = %v, want one naming the key", err)
	}
}

// TestAssemble_RefusesAnOTLPEndpointWithNoExporterRegistered pins the wiring
// refusal Init reports: an endpoint without a registered OTLP exporter fails
// the assembly, naming the blank import that fixes it (this test binary,
// like a host that never wants OTLP, imports no exporter).
func TestAssemble_RefusesAnOTLPEndpointWithNoExporterRegistered(t *testing.T) {
	spec := observabilityBlockSpec(t, pkgcore.ComponentConfig{}.With("otlp_endpoint", "127.0.0.1:4317"))
	err := Assemble(context.Background(), pkgcore.NewComponentRegistry(), spec)
	if !errors.Is(err, obs.ErrOTLPExporterNotRegistered) {
		t.Fatalf("Assemble() with an OTLP endpoint and no exporter error = %v, want obs.ErrOTLPExporterNotRegistered", err)
	}
}

// TestAssemble_RenamedObservabilityCopyReadsItsOwnBlock is the overlay pin:
// a host that registers a renamed copy of the engine's observability
// descriptor selects the COPY in its composition, and the copy's own
// configuration block must be the one its Prepare consumes. The endpoint in
// the copy's block reaching obs.Init is observable through the same refusal
// the test above pins -- a mis-read block would leave the endpoint unset,
// obs.Init would succeed, and this assertion would fail. A literal
// components.observability lookup (the shape this test exists against)
// reads the original's block instead of the copy's, silently dropping the
// configuration the host actually wrote.
func TestAssemble_RenamedObservabilityCopyReadsItsOwnBlock(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	var base pkgcore.Component
	for _, c := range pkgcore.RegisteredComponents(reg) {
		if c.Name == "observability" {
			base = c
			break
		}
	}
	if base.Name == "" {
		t.Fatal("the engine's observability component is not registered")
	}
	renamed := base
	renamed.Name = "test-host.observability"
	if err := reg.Register(renamed); err != nil {
		t.Fatalf("registering the renamed copy: %v", err)
	}

	var host testHostConfig
	spec := driverLoadSpec(t, &host)
	spec.Overrides = &CompositionOverrides{Config: pkgcore.ComponentConfig{}.With("components",
		pkgcore.ComponentConfig{}.
			With("observability", false).
			With(renamed.Name, pkgcore.ComponentConfig{}.With("otlp_endpoint", "127.0.0.1:4317")))}

	err := Assemble(context.Background(), reg, spec)
	if !errors.Is(err, obs.ErrOTLPExporterNotRegistered) {
		t.Fatalf("Assemble() with a renamed observability copy carrying an OTLP endpoint error = %v, want obs.ErrOTLPExporterNotRegistered: the copy's own block must be the one its Prepare consumes", err)
	}
}

// TestNewObservability_RequiresItsPrepareRuntime pins the construction
// refusal: the instance is built from the runtime Prepare publishes, so a
// registry without one fails the read naming the missing step.
func TestNewObservability_RequiresItsPrepareRuntime(t *testing.T) {
	_, err := newObservability(context.Background(), pkgcore.NewComponentRegistry(), pkgcore.ComponentConfig{})
	if err == nil || !strings.Contains(err.Error(), "observability runtime") {
		t.Fatalf("newObservability() without a published runtime error = %v, want one naming the runtime", err)
	}
}

// TestCloseObservability_RefusesAForeignInstance pins the type guard: an
// instance that is not this component's own is refused, never closed blind.
func TestCloseObservability_RefusesAForeignInstance(t *testing.T) {
	err := closeObservability(context.Background(), nil, struct{}{})
	if err == nil || !strings.Contains(err.Error(), "observability component holds an instance") {
		t.Fatalf("closeObservability() with a foreign instance error = %v, want the type refusal", err)
	}
}

// TestCloseObservability_WithoutAShutdownFunctionIsANoOp pins the empty half:
// an instance whose shutdown function is nil closes cleanly, so a Close that
// reaches it never fails the assembly over nothing to shut down.
func TestCloseObservability_WithoutAShutdownFunctionIsANoOp(t *testing.T) {
	if err := closeObservability(context.Background(), nil, &observabilityInstance{}); err != nil {
		t.Fatalf("closeObservability() with a nil shutdown function error = %v, want nil", err)
	}
}
