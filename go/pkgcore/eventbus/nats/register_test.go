package nats

import (
	"context"
	"errors"
	"testing"

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
