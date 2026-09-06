package nats

// Hermetic unit tests for the NATS-backed EventBus: everything here runs
// without a real NATS server, mirroring eventbus/redis's own eventbus_test.go
// split -- what belongs here is local to the bus itself, what needs a real
// broker lives in the integration tier (integration_test/eventbus_test.go).

import (
	"context"
	"errors"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/vislake/speed/go/pkgcore"
)

// TestNewEventBus_PanicsOnNilConn pins that a nil connection is a wiring
// error reported at construction, not a failure deferred to the first
// publish -- the direct analogue of eventbus/redis's identical nil-client
// pin.
func TestNewEventBus_PanicsOnNilConn(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewEventBus(nil) did not panic, want it to")
		}
	}()
	NewEventBus(nil)
}

// TestEventBus_PublishOnCancelledContext pins the contract that no publish
// runs on a cancelled context: Publish returns the context's error before
// any JetStream call, mirroring pkgcore's memory bus and eventbus/redis's
// identical pin. The connection dials a closed port with
// RetryOnFailedConnect so construction itself never blocks or fails.
func TestEventBus_PublishOnCancelledContext(t *testing.T) {
	t.Parallel()

	nc, err := nats.Connect("127.0.0.1:1", nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1))
	if err != nil {
		t.Fatalf("nats.Connect() error = %v, want nil (RetryOnFailedConnect must not fail synchronously)", err)
	}
	t.Cleanup(nc.Close)
	bus := NewEventBus(nc)
	t.Cleanup(bus.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = bus.Publish(ctx, pkgcore.Event{Type: "some.event", Payload: "payload"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish on a cancelled context error = %v, want context.Canceled", err)
	}
}

// TestStreamNameForEventType pins the "." (and other invalid-rune) folding
// rule the package doc comment documents, including the many-to-one
// collision it owns up to.
func TestStreamNameForEventType(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"authn.user.created": "PKGCORE_EVENTS_authn_user_created",
		"a.b":                "PKGCORE_EVENTS_a_b",
		"a_b":                "PKGCORE_EVENTS_a_b", // deliberate collision with "a.b" above
		"plain":              "PKGCORE_EVENTS_plain",
	}
	for eventType, want := range tests {
		if got := streamNameForEventType(eventType); got != want {
			t.Errorf("streamNameForEventType(%q) = %q, want %q", eventType, got, want)
		}
	}
}

// TestBusConsumerName pins the durable consumer name shape every bus
// instance creates on every stream it subscribes to.
func TestBusConsumerName(t *testing.T) {
	t.Parallel()

	if got, want := busConsumerName("abc123"), "pkgcore-bus-abc123"; got != want {
		t.Errorf("busConsumerName(%q) = %q, want %q", "abc123", got, want)
	}
}

// TestNewBusInstanceID_ReturnsDistinctIDs pins that two instances never
// collide on the identifier that keeps their consumers -- and therefore
// their fan-out delivery -- independent of one another.
func TestNewBusInstanceID_ReturnsDistinctIDs(t *testing.T) {
	t.Parallel()

	a, b := newBusInstanceID(), newBusInstanceID()
	if a == "" {
		t.Fatal("newBusInstanceID() returned an empty string")
	}
	if a == b {
		t.Errorf("newBusInstanceID() returned %q twice, want two distinct instance ids", a)
	}
}
