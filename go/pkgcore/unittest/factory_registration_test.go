// This file lives in package unittest — this module's dedicated unit-test
// directory for unit-tier suites with no single source file as their target
// (the backend coding standard's testing-layout rule). The name-registration
// path spans pkgcore's preset/registry/kernel machinery and an
// implementation subpackage at once, so it has no single target either; and
// it must be black-box against package pkgcore: an internal test file
// (package pkgcore) importing an implementation package would be the import
// cycle eventbus_conformance_test.go's own note records, since those
// packages import pkgcore back.
package unittest

import (
	"context"
	"errors"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/pkgcore"
	eventbusredis "github.com/vislake/speed/go/pkgcore/eventbus/redis"
	kvredis "github.com/vislake/speed/go/pkgcore/kv/redis"
)

// TestBootstrap_HostRegisteredImplementationsResolveThroughPreset is the
// end-to-end proof of the name-registration path: a host builds the typed
// object itself (here one *redis.Client shared by two seams, the shape the
// flat Config cannot express), wraps it in each implementation package's
// Registration factory, registers the two Registrations under names of its
// own, and names those names in a Preset — then Bootstrap resolves both
// seams to host-built values, an operation through the resolved store
// reaches the host's client, and Shutdown leaves everything the host built
// alone, the same ownership a pkgcore.WithEventBus injection records.
//
// The preset entries carry a deliberately broken Config ("db" is not a
// number, which clientFromConfig rejects): resolving anyway is what pins
// the factory's documented "the entry's Config is ignored" behavior.
func TestBootstrap_HostRegisteredImplementationsResolveThroughPreset(t *testing.T) {
	ctx := context.Background()

	// The host's object: one client shared by the eventbus and kv seams.
	// Nothing here dials (go-redis connects lazily); the address is
	// unreachable on purpose, so every reachability check below can only
	// pass through this client.
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = client.Close() })

	const busName, kvName = "eventbus.redis.host-built", "kv.redis.host-built"
	if err := pkgcore.EventBusRegistry.Register(eventbusredis.Registration(busName, client)); err != nil {
		t.Fatalf("Register(%q) on EventBusRegistry error = %v, want nil", busName, err)
	}
	if err := pkgcore.KVStoreRegistry.Register(kvredis.Registration(kvName, client)); err != nil {
		t.Fatalf("Register(%q) on KVStoreRegistry error = %v, want nil", kvName, err)
	}

	ignored := pkgcore.Config{"db": "not-a-number"}
	preset := pkgcore.PresetStandalone.
		With("eventbus", pkgcore.SeamPreset{Implementation: busName, Config: ignored}).
		With("kv", pkgcore.SeamPreset{Implementation: kvName, Config: ignored})

	kernel := pkgcore.NewKernel(pkgcore.WithPreset(preset))
	reg, err := kernel.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("Bootstrap() error = %v, want both host-registered implementations resolved", err)
	}

	// The resolved bus is the Redis-backed one, not PresetStandalone's
	// in-memory default: the Preset entry chose the host's registration.
	if _, ok := reg.EventBus().(*eventbusredis.EventBus); !ok {
		t.Fatalf("Bootstrap() resolved eventbus %T, want the *eventbusredis.EventBus over the host's client", reg.EventBus())
	}

	// The resolved store runs on the host's client: the operation reaches
	// it and fails dialing the unreachable address. A store over a client
	// go-redis had closed would report ErrClosed instead, and the in-memory
	// default store would succeed.
	if _, _, err := reg.KVStore().Get(ctx, "factory:registration:probe"); err == nil || errors.Is(err, redis.ErrClosed) {
		t.Errorf("Get() through the resolved store error = %v, want a connection failure from the host's live client", err)
	}

	if err := kernel.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v, want nil", err)
	}

	// The host keeps ownership: Shutdown records no closer for a
	// factory-built value, so the host's client stays usable and still
	// fails on reachability (a dial error) rather than on closure.
	if err := client.Ping(ctx).Err(); errors.Is(err, redis.ErrClosed) {
		t.Errorf("Ping() after Shutdown error = %v: the host-built client was closed by the Kernel", err)
	}
}
