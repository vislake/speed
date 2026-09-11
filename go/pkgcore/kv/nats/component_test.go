package nats

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// TestComponent_WellFormed runs the descriptor through the component
// contract: the naming convention, the decodable schema, the typed tokens.
func TestComponent_WellFormed(t *testing.T) {
	t.Parallel()
	componenttest.AssertWellFormed(t, kvNATSComponent)
}

// TestComponent_DeclaresTheExportedCapabilities pins the descriptor's
// declaration to the package's exported constant, so the bits a host reads
// off this package and the bits the assembly validates cannot drift.
func TestComponent_DeclaresTheExportedCapabilities(t *testing.T) {
	t.Parallel()
	if kvNATSComponent.Capabilities != Capabilities {
		t.Errorf("component capabilities = %v, want the exported Capabilities constant %v", kvNATSComponent.Capabilities, Capabilities)
	}
}

// TestComponent_UnreachableServerFailsConstruction pins the offline-failure
// path: unlike eventbus/nats's retry posture, this component's connection
// dial is synchronous without RetryOnFailedConnect, so an unreachable server
// fails Construct with the dial error rather than leaving a reconnecting
// connection behind -- and the failure surfaces as the assembly's own
// ErrComponentFailed, after the rollback, not as a panic.
func TestComponent_UnreachableServerFailsConstruction(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{
			"kv.nats": map[string]any{"url": "nats://127.0.0.1:1"},
		},
	}))

	if err := reg.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	err := reg.Construct(context.Background())
	if err == nil {
		t.Fatal("Construct() with an unreachable server succeeded, want the dial error")
	}
	if !strings.Contains(err.Error(), "connect to nats") {
		t.Errorf("Construct() error = %v, want it to carry the dial failure", err)
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := reg.Close(closeCtx); err != nil {
		t.Errorf("Close() after the failed Construct error = %v, want nil", err)
	}
}
