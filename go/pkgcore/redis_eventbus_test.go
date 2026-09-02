package pkgcore

// Hermetic unit tests for the Redis-backed EventBus: everything here runs
// without a Redis server. Behaviour that needs a real server lives in the
// integration tier (integration_test/redis_eventbus_test.go); what belongs
// here is what is local to the bus itself -- a nil client is a wiring error
// reported at construction, and Publish on a cancelled context returns the
// context's error before any command reaches the wire. Subscribe is
// deliberately not exercised here: it starts a reader goroutine whose group
// creation retries against Redis in the background, which is behaviour for
// the integration tier.

import (
	"context"
	"errors"
	"testing"

	"github.com/redis/go-redis/v9"
)

// TestNewRedisEventBus_PanicsOnNilClient pins that a nil client is a wiring
// error reported at construction, not a failure deferred to the first
// publish.
func TestNewRedisEventBus_PanicsOnNilClient(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewRedisEventBus(nil) did not panic, want it to")
		}
	}()
	NewRedisEventBus(nil)
}

// TestRedisEventBus_PublishOnCancelledContext pins the contract that no
// publish runs on a cancelled context: Publish returns the context's error
// before the append, mirroring the memory bus and the KVStore's cancelled
// context rule (see TestRedisKVStore_CancelledContext for why the closed-port
// address is the right stand-in for "no server" -- the assertion is that the
// publish reports the context's error, never a transport failure).
func TestRedisEventBus_PublishOnCancelledContext(t *testing.T) {
	t.Parallel()

	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { client.Close() })
	bus := NewRedisEventBus(client)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := bus.Publish(ctx, Event{Type: "some.event", Payload: "payload"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish on a cancelled context error = %v, want context.Canceled", err)
	}
}
