package redis

import (
	"context"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/pkgcore"
)

// TestFromAddr_BuildsAUsableStoreAndDeclaresTheCapabilities drives the
// one-step contract of the bare-injection path over a real (in-process
// miniredis) server: FromAddr alone yields a working store -- a value
// written round-trips -- together with this package's declared
// capabilities, so the injection pair pkgcore.WithKVStore takes comes out
// of one call. Close then releases the client the constructor built, which
// is the store's end of life: go-redis refuses every later command on a
// closed client.
func TestFromAddr_BuildsAUsableStoreAndDeclaresTheCapabilities(t *testing.T) {
	mini := miniredis.RunT(t)

	store, caps, err := FromAddr(mini.Addr())
	if err != nil {
		t.Fatalf("FromAddr(%q) error = %v, want nil", mini.Addr(), err)
	}
	if store == nil {
		t.Fatal("FromAddr returned a nil store")
	}
	if caps != Capabilities {
		t.Errorf("FromAddr capabilities = %v, want the exported Capabilities constant %v", caps, Capabilities)
	}

	// The returned pair must be exactly what the injection path takes; this
	// line is the compile-time half of the contract (WithKVStore returns the
	// option without applying it, so nothing runs here).
	_ = pkgcore.WithKVStore(store, caps)

	ctx := context.Background()
	err = store.Set(ctx, "billing:invoice:1042", []byte("open"), 0)
	if err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	got, ok, err := store.Get(ctx, "billing:invoice:1042")
	if err != nil {
		t.Fatalf("Get() error = %v, want nil", err)
	}
	if !ok || string(got) != "open" {
		t.Errorf("Get() = (%q, %v), want (\"open\", true)", got, ok)
	}

	err = store.Close()
	if err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	err = store.Set(ctx, "billing:invoice:1042", []byte("open"), 0)
	if !errors.Is(err, redis.ErrClosed) {
		t.Errorf("Set after Close error = %v, want redis.ErrClosed: Close must release the client the constructor built", err)
	}
}

// TestFromAddr_EmptyAddrReturnsError pins the call-site contract for an
// address the caller never set: unlike the configuration channel's
// clientFromConfig, which defaults an unset "addr" so a zero-configuration
// Preset can build something, the explicit constructor refuses an empty
// address as the wiring mistake it is -- an error, never a panic and never
// the silent "localhost:6379" fallback go-redis itself would apply.
func TestFromAddr_EmptyAddrReturnsError(t *testing.T) {
	store, caps, err := FromAddr("")
	if err == nil {
		t.Fatal(`FromAddr("") error = nil, want an error`)
	}
	if store != nil {
		t.Errorf(`FromAddr("") store = %v, want nil`, store)
	}
	if caps != 0 {
		t.Errorf(`FromAddr("") capabilities = %v, want 0 on the failure path`, caps)
	}
}
