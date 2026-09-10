package redis

import (
	"context"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/vislake/speed/go/pkgcore"
)

// TestFromAddr_BuildsAUsableBusAndDeclaresTheCapabilities drives the
// one-step contract of the bare-injection path over a real (in-process
// miniredis) server: FromAddr alone yields a working bus -- the locally
// subscribed handler runs synchronously on a publish that commits to the
// type's stream -- together with this package's declared capabilities, so
// the injection pair pkgcore.WithEventBus takes comes out of one call.
// Close then releases what the constructor built: the bus refuses further
// publishes, exactly as after the built-in registration's own Close.
func TestFromAddr_BuildsAUsableBusAndDeclaresTheCapabilities(t *testing.T) {
	mini := miniredis.RunT(t)

	bus, caps, err := FromAddr(mini.Addr())
	if err != nil {
		t.Fatalf("FromAddr(%q) error = %v, want nil", mini.Addr(), err)
	}
	if bus == nil {
		t.Fatal("FromAddr returned a nil bus")
	}
	if caps != Capabilities {
		t.Errorf("FromAddr capabilities = %v, want the exported Capabilities constant %v", caps, Capabilities)
	}

	delivered := 0
	bus.Subscribe("note.created", func(context.Context, pkgcore.Event) error {
		delivered++
		return nil
	})
	err = bus.Publish(context.Background(), pkgcore.Event{Type: "note.created", Payload: "p"})
	if err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}
	if delivered != 1 {
		t.Errorf("local subscriber invocations = %d, want 1", delivered)
	}

	err = bus.Close()
	if err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	err = bus.Publish(context.Background(), pkgcore.Event{Type: "note.created"})
	if !errors.Is(err, ErrEventBusClosed) {
		t.Errorf("Publish after Close error = %v, want ErrEventBusClosed", err)
	}
}

// TestFromAddr_EmptyAddrReturnsError pins the call-site contract for an
// address the caller never set: unlike the configuration channel's
// clientFromConfig, which defaults an unset "addr" so a zero-configuration
// Preset can build something, the explicit constructor refuses an empty
// address as the wiring mistake it is -- an error, never a panic and never
// the silent "localhost:6379" fallback go-redis itself would apply.
func TestFromAddr_EmptyAddrReturnsError(t *testing.T) {
	bus, caps, err := FromAddr("")
	if err == nil {
		t.Fatal(`FromAddr("") error = nil, want an error`)
	}
	if bus != nil {
		t.Errorf(`FromAddr("") bus = %v, want nil`, bus)
	}
	if caps != 0 {
		t.Errorf(`FromAddr("") capabilities = %v, want 0 on the failure path`, caps)
	}
}
