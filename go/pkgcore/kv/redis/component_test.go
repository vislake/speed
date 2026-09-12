package redis

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// TestComponent_WellFormed runs the descriptor through the component
// contract: the naming convention, the decodable schema, the typed tokens.
func TestComponent_WellFormed(t *testing.T) {
	t.Parallel()
	componenttest.AssertWellFormed(t, kvRedisComponent)
}

// TestComponent_DeclaresTheExportedCapabilities pins the descriptor's
// declaration to the package's exported constant, so the bits a host reads
// off this package and the bits the assembly validates cannot drift.
func TestComponent_DeclaresTheExportedCapabilities(t *testing.T) {
	t.Parallel()
	if kvRedisComponent.Capabilities != Capabilities {
		t.Errorf("component capabilities = %v, want the exported Capabilities constant %v", kvRedisComponent.Capabilities, Capabilities)
	}
}

// TestComponent_ConstructsThroughTheRegistry selects the component in a real
// assembly and drives Prepare and Construct: nothing is dialed (go-redis
// connects lazily), the product resolves through Get at the module's
// contract type, and Close releases the client the component built -- which
// the store observes afterwards: an operation on the released client fails
// with redis.ErrClosed, never with the dial refusal a live client aimed at
// the unreachable addr produces.
func TestComponent_ConstructsThroughTheRegistry(t *testing.T) {
	ctx := context.Background()
	reg := pkgcore.NewComponentRegistry()
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{
			"kv.redis": map[string]any{"addr": "127.0.0.1:1"},
		},
	}))

	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := reg.Construct(ctx); err != nil {
		t.Fatalf("Construct() error = %v", err)
	}
	store, err := pkgcore.Get[pkgcore.KVStore](reg)
	if err != nil {
		t.Fatalf("Get[pkgcore.KVStore] error = %v, want the constructed store", err)
	}
	// The control: while the client lives, the unreachable addr yields the
	// dial refusal, so the closed-client sentinel below can only come from
	// the release.
	if _, _, err := store.Get(ctx, "close-probe"); errors.Is(err, redis.ErrClosed) {
		t.Fatalf("Get before Close = %v, want the dial refusal from the unreachable addr", err)
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := reg.Close(closeCtx); err != nil {
		t.Fatalf("Close() error = %v, want the built client released", err)
	}
	if _, _, err := store.Get(ctx, "close-probe"); !errors.Is(err, redis.ErrClosed) {
		t.Errorf("Get after Close = %v, want redis.ErrClosed: the close stage must have released the client", err)
	}
}
