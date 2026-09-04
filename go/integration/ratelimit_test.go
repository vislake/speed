package integration

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/ratelimit"
)

const minute = time.Minute

// newTestLayeredLimiter returns a LayeredLimiter over a fresh in-memory
// KVStore -- pkgcore.NewMemoryKVStore doubles as go/ratelimit's own test
// double (root CLAUDE.md's "in-process implementations double as test
// doubles" note), so this needs no Redis or any other external dependency.
func newTestLayeredLimiter(limits LayeredLimits) *LayeredLimiter {
	return NewLayeredLimiter(ratelimit.New(pkgcore.NewMemoryKVStore()), limits)
}

func TestLayeredLimiter_AllThreeLayersAllow(t *testing.T) {
	l := newTestLayeredLimiter(LayeredLimits{
		Global: ratelimit.Limit{Rate: 100, Per: minute},
		Tenant: ratelimit.Limit{Rate: 100, Per: minute},
		Key:    ratelimit.Limit{Rate: 100, Per: minute},
	})

	got, err := l.Allow(context.Background(), "g", "t1", "k1")
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if !got.Allowed {
		t.Errorf("Allow = %+v, want Allowed=true", got)
	}
	if got.Layer != LayerKey {
		t.Errorf("Layer = %q, want %q (the last layer evaluated on an allow)", got.Layer, LayerKey)
	}
}

func TestLayeredLimiter_GlobalLayerDenies_TenantAndKeyNeverConsulted(t *testing.T) {
	l := newTestLayeredLimiter(LayeredLimits{
		Global: ratelimit.Limit{Rate: 1, Per: minute},
		Tenant: ratelimit.Limit{Rate: 100, Per: minute},
		Key:    ratelimit.Limit{Rate: 100, Per: minute},
	})
	ctx := context.Background()

	if got, err := l.Allow(ctx, "g", "t1", "k1"); err != nil || !got.Allowed {
		t.Fatalf("first Allow = %+v, err=%v, want the global layer's first hit to be allowed", got, err)
	}

	// The global layer's single-hit budget is now exhausted. A DIFFERENT
	// tenant and key, each well within their own limits, must still be
	// denied: the global layer denies the whole request before the tenant
	// or key layer is ever consulted.
	got, err := l.Allow(ctx, "g", "t2", "k2")
	if err != nil {
		t.Fatalf("second Allow: %v", err)
	}
	if got.Allowed {
		t.Error("Allowed = true, want the exhausted global layer to deny every tenant/key")
	}
	if got.Layer != LayerGlobal {
		t.Errorf("Layer = %q, want %q", got.Layer, LayerGlobal)
	}
}

func TestLayeredLimiter_TenantLayerDenies_KeyLayerNeverConsulted(t *testing.T) {
	l := newTestLayeredLimiter(LayeredLimits{
		Global: ratelimit.Limit{Rate: 100, Per: minute},
		Tenant: ratelimit.Limit{Rate: 1, Per: minute},
		Key:    ratelimit.Limit{Rate: 100, Per: minute},
	})
	ctx := context.Background()

	if got, err := l.Allow(ctx, "g", "t1", "k1"); err != nil || !got.Allowed {
		t.Fatalf("first Allow = %+v, err=%v", got, err)
	}

	// A different key under the SAME tenant must be denied by the
	// exhausted tenant layer, even though that key's own layer has never
	// been hit.
	got, err := l.Allow(ctx, "g", "t1", "k2")
	if err != nil {
		t.Fatalf("second Allow: %v", err)
	}
	if got.Allowed {
		t.Error("Allowed = true, want the exhausted tenant layer to deny a second key under the same tenant")
	}
	if got.Layer != LayerTenant {
		t.Errorf("Layer = %q, want %q", got.Layer, LayerTenant)
	}
}

func TestLayeredLimiter_KeyLayerDenies_OtherKeysUnaffected(t *testing.T) {
	l := newTestLayeredLimiter(LayeredLimits{
		Global: ratelimit.Limit{Rate: 100, Per: minute},
		Tenant: ratelimit.Limit{Rate: 100, Per: minute},
		Key:    ratelimit.Limit{Rate: 1, Per: minute},
	})
	ctx := context.Background()

	if got, err := l.Allow(ctx, "g", "t1", "k1"); err != nil || !got.Allowed {
		t.Fatalf("first Allow = %+v, err=%v", got, err)
	}

	deny, err := l.Allow(ctx, "g", "t1", "k1")
	if err != nil {
		t.Fatalf("second Allow (same key): %v", err)
	}
	if deny.Allowed || deny.Layer != LayerKey {
		t.Errorf("second Allow (same key) = %+v, want denied at %q", deny, LayerKey)
	}

	// A second key under the same tenant has its own, unexhausted budget.
	other, err := l.Allow(ctx, "g", "t1", "k2")
	if err != nil {
		t.Fatalf("Allow (other key): %v", err)
	}
	if !other.Allowed {
		t.Errorf("Allow (other key) = %+v, want the untouched key-1 quota to leave key-2 unaffected", other)
	}
}

// TestLayeredLimiter_DisabledLayer_IsNeverConsulted proves a zero-value
// Limit disables its layer entirely, per LayeredLimits' own doc comment: a
// Limit{Rate: 0} would otherwise fail go/ratelimit.Limit.validate with
// ErrInvalidLimit, so a call that never errors here is itself proof the
// disabled layer's Allow was never invoked.
func TestLayeredLimiter_DisabledLayer_IsNeverConsulted(t *testing.T) {
	l := newTestLayeredLimiter(LayeredLimits{
		Key: ratelimit.Limit{Rate: 100, Per: minute},
		// Global and Tenant left at their zero value -- disabled.
	})

	got, err := l.Allow(context.Background(), "g", "t1", "k1")
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if !got.Allowed || got.Layer != LayerKey {
		t.Errorf("Allow = %+v, want Allowed=true at layer %q", got, LayerKey)
	}
}

func TestLayeredLimiter_AllLayersDisabled_AlwaysAllows(t *testing.T) {
	l := newTestLayeredLimiter(LayeredLimits{})
	got, err := l.Allow(context.Background(), "g", "t1", "k1")
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if !got.Allowed {
		t.Errorf("Allow = %+v, want Allowed=true with every layer disabled", got)
	}
	if got.Layer != "" {
		t.Errorf("Layer = %q, want empty (no layer was ever evaluated)", got.Layer)
	}
}

// TestLayeredLimiter_InvalidLimit_ReturnsError uses a negative Per, not a
// non-positive Rate: a non-positive Rate is what LayeredLimits' own doc
// comment defines as "disabled" and Allow intercepts before the underlying
// Limiter is ever called, so it is Per that must be invalid here to actually
// reach go/ratelimit.Limit.validate's own rejection.
func TestLayeredLimiter_InvalidLimit_ReturnsError(t *testing.T) {
	l := newTestLayeredLimiter(LayeredLimits{
		Global: ratelimit.Limit{Rate: 10, Per: -1},
	})
	if _, err := l.Allow(context.Background(), "g", "t1", "k1"); err == nil {
		t.Error("Allow with an invalid (negative Per) Limit returned no error")
	}
}
