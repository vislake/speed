package redis

// Hermetic unit tests for the Redis-backed EventBus: everything here runs
// without a Redis server. Behaviour that needs a real server lives in the
// integration tier (integration_test/eventbus_test.go); what belongs here is
// what is local to the bus itself -- a nil client is a wiring error reported
// at construction, and Publish on a cancelled context returns the context's
// error before any command reaches the wire. Subscribe is deliberately not
// exercised here: it starts a reader goroutine whose group creation retries
// against Redis in the background, which is behaviour for the integration
// tier.

import (
	"context"
	"errors"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/pkgcore"
)

// TestNewEventBus_PanicsOnNilClient pins that a nil client is a wiring error
// reported at construction, not a failure deferred to the first publish.
func TestNewEventBus_PanicsOnNilClient(t *testing.T) {
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
// the append, mirroring pkgcore's memory bus and the KVStore's cancelled
// context rule -- the closed-port address is the right stand-in for "no
// server", since the assertion is that the publish reports the context's
// error, never a transport failure.
func TestEventBus_PublishOnCancelledContext(t *testing.T) {
	t.Parallel()

	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { client.Close() })
	bus := NewEventBus(client)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := bus.Publish(ctx, pkgcore.Event{Type: "some.event", Payload: "payload"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish on a cancelled context error = %v, want context.Canceled", err)
	}
}
