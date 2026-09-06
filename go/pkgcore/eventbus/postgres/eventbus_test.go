package postgres

// Hermetic unit tests for the PostgreSQL-backed EventBus: everything here
// runs without a reachable PostgreSQL server, mirroring
// eventbus/redis/eventbus_test.go's own scope note exactly -- a nil pool or
// an empty replicaID is a wiring error reported at construction, and
// Publish on a cancelled context returns the context's error before any
// query reaches the wire. Subscribe is deliberately not exercised here: it
// starts a listener goroutine whose connection attempt retries against
// PostgreSQL in the background, which is behaviour for the integration
// tier (integration_test/).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vislake/speed/go/pkgcore"
)

// TestNewEventBus_PanicsOnNilPool pins that a nil pool is a wiring error
// reported at construction, not a failure deferred to the first publish.
func TestNewEventBus_PanicsOnNilPool(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewEventBus(nil, ...) did not panic, want it to")
		}
	}()
	NewEventBus(nil, "replica-1")
}

// TestNewEventBus_PanicsOnEmptyReplicaID pins that an empty replicaID is a
// wiring error reported at construction: silently accepting one would
// quietly defeat the cross-restart durability guarantee this
// implementation exists to provide (see EventBus's own doc comment).
func TestNewEventBus_PanicsOnEmptyReplicaID(t *testing.T) {
	t.Parallel()

	pool := unconnectedPool(t)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewEventBus(pool, \"\") did not panic, want it to")
		}
	}()
	NewEventBus(pool, "")
}

// TestEventBus_PublishOnCancelledContext pins the contract that no publish
// runs on a cancelled context: Publish returns the context's error before
// any query is attempted, mirroring pkgcore's memory bus and
// eventbus/redis's identical cancelled-context rule.
func TestEventBus_PublishOnCancelledContext(t *testing.T) {
	t.Parallel()

	pool := unconnectedPool(t)
	bus := NewEventBus(pool, "replica-1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := bus.Publish(ctx, pkgcore.Event{Type: "some.event", Payload: "payload"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish() on a cancelled context error = %v, want context.Canceled", err)
	}
}

// TestEventBus_PublishAfterClose pins that a closed bus refuses Publish
// with ErrEventBusClosed rather than attempting the write.
func TestEventBus_PublishAfterClose(t *testing.T) {
	t.Parallel()

	pool := unconnectedPool(t)
	bus := NewEventBus(pool, "replica-1")
	bus.Close()

	err := bus.Publish(context.Background(), pkgcore.Event{Type: "some.event", Payload: "payload"})
	if !errors.Is(err, ErrEventBusClosed) {
		t.Fatalf("Publish() after Close error = %v, want ErrEventBusClosed", err)
	}
}

// TestEventBus_CloseWithoutSubscribeReturnsPromptly pins that Close never
// blocks waiting for a listener goroutine that Subscribe never started --
// the guard listenStarted exists for exactly this case.
func TestEventBus_CloseWithoutSubscribeReturnsPromptly(t *testing.T) {
	t.Parallel()

	pool := unconnectedPool(t)
	bus := NewEventBus(pool, "replica-1")

	done := make(chan struct{})
	go func() {
		bus.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close() did not return within 1s with no listener ever started")
	}
}

// TestEventBus_SubscribeIgnoresNilHandler pins that a nil handler is
// dropped rather than registered, mirroring pkgcore's memory bus and
// eventbus/redis's identical nil-handler rule -- registering it would
// panic the first delivery attempt.
func TestEventBus_SubscribeIgnoresNilHandler(t *testing.T) {
	t.Parallel()

	pool := unconnectedPool(t)
	bus := NewEventBus(pool, "replica-1")
	defer bus.Close()

	bus.Subscribe("some.event", nil)

	if got := len(bus.handlersFor("some.event")); got != 0 {
		t.Fatalf("handlersFor() after Subscribe(nil) = %d handlers, want 0", got)
	}
}

// unconnectedPool returns a *pgxpool.Pool built against a DSN pointing at a
// closed port. pgxpool.New never dials at construction (mirroring
// pgxpool's own lazy-connect contract, the same property eventbus/redis's
// example relies on for redis.NewClient), so building one is safe in a
// hermetic unit test; only an actual query would reach the closed port and
// fail, which is exactly the behaviour these tests never exercise.
func unconnectedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://user:pass@127.0.0.1:1/db")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
