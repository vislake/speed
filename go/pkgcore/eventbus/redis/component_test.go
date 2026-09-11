package redis

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
	componenttest.AssertWellFormed(t, eventBusRedisComponent)
}

// TestComponent_DeclaresTheExportedCapabilities pins the descriptor's
// declaration to the package's exported constant, so the bits a host reads
// off this package and the bits the assembly validates cannot drift.
func TestComponent_DeclaresTheExportedCapabilities(t *testing.T) {
	t.Parallel()
	if eventBusRedisComponent.Capabilities != Capabilities {
		t.Errorf("component capabilities = %v, want the exported Capabilities constant %v", eventBusRedisComponent.Capabilities, Capabilities)
	}
}

// TestComponent_ConstructsThroughTheRegistry selects the component in a real
// assembly and drives Prepare and Construct: nothing is dialed (go-redis
// connects lazily), the product resolves through Get at the module's
// contract type, and Close releases the client the component built.
func TestComponent_ConstructsThroughTheRegistry(t *testing.T) {
	ctx := context.Background()
	reg := pkgcore.NewComponentRegistry()
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{
			"eventbus.redis": map[string]any{"addr": "127.0.0.1:1"},
		},
	}))

	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := reg.Construct(ctx); err != nil {
		t.Fatalf("Construct() error = %v", err)
	}
	if _, err := pkgcore.Get[pkgcore.EventBus](reg); err != nil {
		t.Errorf("Get[pkgcore.EventBus] error = %v, want the constructed bus", err)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := reg.Close(closeCtx); err != nil {
		t.Fatalf("Close() error = %v, want the built client released", err)
	}
}
