package nats

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// TestComponent_WellFormed runs the descriptor through the component
// contract: the naming convention, the decodable schema, the typed tokens.
func TestComponent_WellFormed(t *testing.T) {
	t.Parallel()
	componenttest.AssertWellFormed(t, eventBusNATSComponent)
}

// TestComponent_DeclaresTheExportedCapabilities pins the descriptor's
// declaration to the package's exported constant, so the bits a host reads
// off this package and the bits the assembly validates cannot drift.
func TestComponent_DeclaresTheExportedCapabilities(t *testing.T) {
	t.Parallel()
	if eventBusNATSComponent.Capabilities != Capabilities {
		t.Errorf("component capabilities = %v, want the exported Capabilities constant %v", eventBusNATSComponent.Capabilities, Capabilities)
	}
}

// TestComponent_ConstructsThroughTheRegistry selects the component against
// an unreachable server: nats.Connect with this package's retry posture
// returns a live connection in its reconnecting state rather than an error,
// so construction succeeds and a following Close (no readers, one
// connection) returns promptly.
func TestComponent_ConstructsThroughTheRegistry(t *testing.T) {
	ctx := context.Background()
	reg := pkgcore.NewComponentRegistry()
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{
			"eventbus.nats": map[string]any{"url": "nats://127.0.0.1:1"},
		},
	}))

	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := reg.Construct(ctx); err != nil {
		t.Fatalf("Construct() error = %v, want the reconnecting-connection success path", err)
	}
	if _, err := pkgcore.Get[pkgcore.EventBus](reg); err != nil {
		t.Errorf("Get[pkgcore.EventBus] error = %v, want the constructed bus", err)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := reg.Close(closeCtx); err != nil {
		t.Fatalf("Close() error = %v, want the dialed connection released", err)
	}
}
