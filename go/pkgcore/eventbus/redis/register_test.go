package redis

import (
	"context"
	"errors"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/pkgcore"
)

// TestInit_RegistersEventBusRedisOnTheSharedRegistry proves this package's
// init() really lands "eventbus.redis" on pkgcore's shared EventBusRegistry
// with the capability the distributed deployment mode requires -- the
// registration this package itself performs, verified from the consuming
// side. pkgcore's PresetDistributed already names this
// implementation for the "eventbus" seam (preset_test.go pins the name
// itself); this test is what proves the name actually resolves once this
// package is imported.
func TestInit_RegistersEventBusRedisOnTheSharedRegistry(t *testing.T) {
	impl, caps, err := pkgcore.EventBusRegistry.Build("eventbus.redis", pkgcore.Config{})
	if err != nil {
		t.Fatalf("Build(%q) error = %v, want nil", "eventbus.redis", err)
	}
	if impl == nil {
		t.Error("Build(\"eventbus.redis\") returned a nil EventBus")
	}
	if want := pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart; caps != want {
		t.Errorf("Build(%q) capabilities = %v, want %v", "eventbus.redis", caps, want)
	}
	if caps != Capabilities {
		t.Errorf("Build(%q) capabilities = %v, want the exported Capabilities constant %v the host reads off this package", "eventbus.redis", caps, Capabilities)
	}
}

// TestClientFromConfig_DefaultsAddrWhenUnset pins the fallback address a
// zero-configuration Preset relies on.
func TestClientFromConfig_DefaultsAddrWhenUnset(t *testing.T) {
	client, err := clientFromConfig(pkgcore.Config{})
	if err != nil {
		t.Fatalf("clientFromConfig(Config{}) error = %v, want nil", err)
	}
	if got := client.Options().Addr; got != "localhost:6379" {
		t.Errorf("client.Options().Addr = %q, want %q", got, "localhost:6379")
	}
}

// TestClientFromConfig_InvalidDBReturnsError pins that a malformed "db"
// value is rejected rather than silently defaulting.
func TestClientFromConfig_InvalidDBReturnsError(t *testing.T) {
	_, err := clientFromConfig(pkgcore.Config{"db": "not-a-number"})
	if err == nil {
		t.Fatal("clientFromConfig() with an invalid db succeeded, want an error")
	}
}

// TestBuiltinClose_StopsTheBusAndReleasesTheDialedClient drives the
// registration's resource-ownership contract: the value Build hands back is
// the closable wrapper, and Close both stops the bus (a publish afterwards
// is refused with ErrEventBusClosed) and releases the client the
// registration built.
func TestBuiltinClose_StopsTheBusAndReleasesTheDialedClient(t *testing.T) {
	impl, _, err := pkgcore.EventBusRegistry.Build("eventbus.redis", pkgcore.Config{})
	if err != nil {
		t.Fatalf("Build(%q) error = %v, want nil", "eventbus.redis", err)
	}
	closable, ok := impl.(*closableEventBus)
	if !ok {
		t.Fatalf("Build(%q) returned %T, want the *closableEventBus whose Close releases the dialed client", "eventbus.redis", impl)
	}
	if err := closable.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	if err := closable.Publish(context.Background(), pkgcore.Event{Type: "some.event", Payload: "x"}); !errors.Is(err, ErrEventBusClosed) {
		t.Errorf("Publish after the builtin's Close error = %v, want ErrEventBusClosed", err)
	}
}

// TestRegistration_WrapsTheHostBuiltClient pins the factory contract a host
// follows on the name-registration path: the Registration carries the name
// the host chose and the package's own exported Capabilities, and its New
// hands back the bare bus over the host's client -- no Close() error
// method, so Kernel.Shutdown leaves the client and the bus to the host, the
// same ownership pkgcore.WithEventBus records. New ignores a preset entry's
// Config: the deliberately broken "db" below would fail clientFromConfig if
// the factory consulted it.
func TestRegistration_WrapsTheHostBuiltClient(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = client.Close() })

	r := Registration("eventbus.redis.host", client)
	if r.Name != "eventbus.redis.host" {
		t.Errorf("Registration().Name = %q, want the host-chosen name", r.Name)
	}
	if r.Capabilities != Capabilities {
		t.Errorf("Registration().Capabilities = %v, want the exported constant %v", r.Capabilities, Capabilities)
	}

	impl, err := r.New(pkgcore.Config{"db": "not-a-number"})
	if err != nil {
		t.Fatalf("Registration().New() error = %v, want nil: the factory ignores the preset entry's Config", err)
	}
	if impl == nil {
		t.Fatal("Registration().New() returned a nil EventBus")
	}
	if _, ok := impl.(interface{ Close() error }); ok {
		t.Error("Registration().New() returned a value carrying Close() error, want the bare bus the host owns")
	}
}
