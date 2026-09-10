package nats

import (
	"context"
	"errors"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/vislake/speed/go/pkgcore"
)

// TestInit_RegistersEventBusNatsOnTheSharedRegistry proves this package's
// init() really lands "eventbus.nats" on pkgcore's shared EventBusRegistry
// with the capability this implementation actually has, mirroring
// eventbus/redis/register_test.go's identical proof for "eventbus.redis".
// Unlike that name, PresetDistributed does not point the "eventbus" seam at
// this one; a host that wants it builds it by name explicitly (this test
// does exactly that) or wires it directly with pkgcore.WithEventBus.
func TestInit_RegistersEventBusNatsOnTheSharedRegistry(t *testing.T) {
	impl, caps, err := pkgcore.EventBusRegistry.Build("eventbus.nats", pkgcore.Config{})
	if err != nil {
		t.Fatalf("Build(%q) error = %v, want nil", "eventbus.nats", err)
	}
	if impl == nil {
		t.Error("Build(\"eventbus.nats\") returned a nil EventBus")
	}
	if bus, ok := impl.(*EventBus); ok {
		t.Cleanup(bus.Close)
	}
	if want := pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart; caps != want {
		t.Errorf("Build(%q) capabilities = %v, want %v", "eventbus.nats", caps, want)
	}
	if caps != Capabilities {
		t.Errorf("Build(%q) capabilities = %v, want the exported Capabilities constant %v the host reads off this package", "eventbus.nats", caps, Capabilities)
	}
}

// TestConnFromConfig_DefaultsURLWhenUnset pins the fallback address a
// zero-configuration Build call relies on.
func TestConnFromConfig_DefaultsURLWhenUnset(t *testing.T) {
	conn, err := connFromConfig(pkgcore.Config{})
	if err != nil {
		t.Fatalf("connFromConfig(Config{}) error = %v, want nil", err)
	}
	t.Cleanup(conn.Close)
	if got := conn.ConnectedUrl(); got != "" {
		// Not yet connected against a real server (RetryOnFailedConnect keeps
		// the initial dial from failing), so ConnectedUrl is empty; what this
		// pins is that construction itself never errors on the default.
		t.Logf("connFromConfig(Config{}) connected eagerly to %q", got)
	}
}

// TestConnFromConfig_UsesConfiguredURL pins that an explicit "url" in cfg is
// honored rather than silently overridden by the default.
func TestConnFromConfig_UsesConfiguredURL(t *testing.T) {
	conn, err := connFromConfig(pkgcore.Config{"url": "nats://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("connFromConfig() error = %v, want nil (RetryOnFailedConnect must not fail synchronously)", err)
	}
	t.Cleanup(conn.Close)
}

// TestBuiltinClose_StopsTheBusAndReleasesTheDialedConnection drives the
// registration's resource-ownership contract: the value Build hands back is
// the closable wrapper, and Close both stops the bus (a publish afterwards is
// refused with ErrEventBusClosed) and releases the connection the
// registration itself dialed.
func TestBuiltinClose_StopsTheBusAndReleasesTheDialedConnection(t *testing.T) {
	impl, _, err := pkgcore.EventBusRegistry.Build("eventbus.nats", pkgcore.Config{})
	if err != nil {
		t.Fatalf("Build(%q) error = %v, want nil", "eventbus.nats", err)
	}
	closable, ok := impl.(*closableEventBus)
	if !ok {
		t.Fatalf("Build(%q) returned %T, want the *closableEventBus whose Close releases the dialed connection", "eventbus.nats", impl)
	}
	if err := closable.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	if err := closable.Publish(context.Background(), pkgcore.Event{Type: "some.event", Payload: "x"}); !errors.Is(err, ErrEventBusClosed) {
		t.Errorf("Publish after the builtin's Close error = %v, want ErrEventBusClosed", err)
	}
}

// TestRegistration_WrapsTheHostBuiltConnection pins the factory contract a
// host follows on the name-registration path: the Registration carries the
// name the host chose and the package's own exported Capabilities, and its
// New hands back the bare bus over the host's connection -- no Close()
// error method, so Kernel.Shutdown leaves the connection and the bus to the
// host, the same ownership pkgcore.WithEventBus records. New ignores a
// preset entry's Config: the deliberately broken "url" below would fail
// connFromConfig if the factory consulted it.
func TestRegistration_WrapsTheHostBuiltConnection(t *testing.T) {
	conn, err := nats.Connect("127.0.0.1:1", nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1))
	if err != nil {
		t.Fatalf("nats.Connect() error = %v, want nil (RetryOnFailedConnect keeps the connection live)", err)
	}
	t.Cleanup(conn.Close)

	r := Registration("eventbus.nats.host", conn)
	if r.Name != "eventbus.nats.host" {
		t.Errorf("Registration().Name = %q, want the host-chosen name", r.Name)
	}
	if r.Capabilities != Capabilities {
		t.Errorf("Registration().Capabilities = %v, want the exported constant %v", r.Capabilities, Capabilities)
	}

	impl, err := r.New(pkgcore.Config{"url": "not a url"})
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
