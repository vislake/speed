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
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
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

// redisBuses builds one miniredis server and two bus instances sharing it,
// each over its own go-redis client -- the faithful two-instance model of a
// two-replica deployment.
func redisBuses(t *testing.T) (*miniredis.Miniredis, *EventBus, *EventBus) {
	t.Helper()
	mini := miniredis.RunT(t)
	clientA := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	clientB := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = clientA.Close() })
	t.Cleanup(func() { _ = clientB.Close() })
	return mini, NewEventBus(clientA), NewEventBus(clientB)
}

// waitFor polls cond until it reports true or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// groupCount reports how many consumer groups the server currently holds on
// eventType's stream, or 0 when the stream does not exist yet.
func groupCount(t *testing.T, client *redis.Client, eventType string) int {
	t.Helper()
	groups, err := client.XInfoGroups(context.Background(), eventStreamKey(eventType)).Result()
	if err != nil {
		return 0 // no stream yet, or the stream was deleted
	}
	return len(groups)
}

// TestEventBus_CrossInstanceFanOut_OverMiniredis drives the delivery
// contract between two buses sharing one server: an event published by one
// instance reaches the other's subscriber exactly once, the publishing
// instance's own subscriber runs once synchronously and is never re-run by
// its own reader (the src skip), and the tenant header survives the trip.
func TestEventBus_CrossInstanceFanOut_OverMiniredis(t *testing.T) {
	_, busA, busB := redisBuses(t)
	defer busA.Close()
	defer busB.Close()
	ctx := context.Background()
	const eventType = "fan.out"

	localInvocations := 0
	remote := make(chan pkgcore.Event, 1)
	busA.Subscribe(eventType, func(_ context.Context, evt pkgcore.Event) error {
		localInvocations++
		return nil
	})
	busB.Subscribe(eventType, func(_ context.Context, evt pkgcore.Event) error {
		remote <- evt
		return nil
	})

	// Wait for both instances' consumer groups to exist before publishing,
	// so the event cannot be published before a reader is live.
	waitFor(t, "both consumer groups", func() bool {
		return groupCount(t, redisClientOf(busB), eventType) == 2
	})

	if err := busA.Publish(ctx, pkgcore.Event{Type: eventType, TenantID: "tenant-3", Payload: "hello from A"}); err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}

	select {
	case evt := <-remote:
		if evt.Type != eventType || evt.TenantID != "tenant-3" || evt.Payload != "hello from A" {
			t.Errorf("remote event = %+v, want the published type, tenant and payload", evt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bus B never delivered the event published on bus A")
	}

	// Wait until A's own reader has processed (and skipped) its own entry --
	// visible as its group's pending count returning to zero -- then pin
	// that the local handler ran exactly once: the publishing instance's own
	// reader must never re-run its handlers.
	waitFor(t, "A's reader to acknowledge its own entry", func() bool {
		pending, err := redisClientOf(busA).XPending(ctx, eventStreamKey(eventType), eventGroupPrefix+instanceIDOf(busA)).Result()
		return err == nil && pending.Count == 0
	})
	if localInvocations != 1 {
		t.Errorf("A's local handler ran %d times, want exactly 1: the publishing instance must not re-run its own handlers", localInvocations)
	}
}

func redisClientOf(b *EventBus) *redis.Client { return b.client }
func instanceIDOf(b *EventBus) string         { return b.instanceID }

// TestEventBus_Close_RemovesItsGroupAndThenTheStream_OverMiniredis drives
// Close's server-side cleanup: closing one instance removes only its own
// consumer group, and closing the last instance removes the stream itself,
// so a gracefully shut-down deployment leaves nothing behind.
func TestEventBus_Close_RemovesItsGroupAndThenTheStream_OverMiniredis(t *testing.T) {
	_, busA, busB := redisBuses(t)
	ctx := context.Background()
	const eventType = "cleanup.me"

	busA.Subscribe(eventType, func(context.Context, pkgcore.Event) error { return nil })
	busB.Subscribe(eventType, func(context.Context, pkgcore.Event) error { return nil })
	waitFor(t, "both consumer groups", func() bool {
		return groupCount(t, redisClientOf(busA), eventType) == 2
	})

	busA.Close()
	waitFor(t, "A's group to be destroyed", func() bool {
		return groupCount(t, redisClientOf(busB), eventType) == 1
	})
	if err := busA.Publish(ctx, pkgcore.Event{Type: eventType, Payload: "x"}); !errors.Is(err, ErrEventBusClosed) {
		t.Errorf("Publish after Close error = %v, want ErrEventBusClosed", err)
	}

	stream := eventStreamKey(eventType)
	before := redisClientOf(busB).Exists(ctx, stream).Val()
	if before != 1 {
		t.Fatalf("stream existence while a reader remains = %d, want 1", before)
	}

	busB.Close()
	waitFor(t, "the stream to be deleted with its last reader", func() bool {
		return redisClientOf(busA).Exists(ctx, stream).Val() == 0
	})
}

// TestEventBus_PublishSubscribeLifecycle_OverMiniredis pins the local half
// of the contract over the wire: handlers run synchronously in registration
// order, a failing handler's error is joined into the publish's error, and
// a Subscribe after Close registers nothing.
func TestEventBus_PublishSubscribeLifecycle_OverMiniredis(t *testing.T) {
	_, busA, _ := redisBuses(t)
	ctx := context.Background()
	const eventType = "lifecycle"

	// Publish before any subscription: appends fine, delivers nowhere.
	if err := busA.Publish(ctx, pkgcore.Event{Type: eventType, Payload: "early"}); err != nil {
		t.Fatalf("Publish before Subscribe error = %v, want nil", err)
	}

	var order []string
	busA.Subscribe(eventType, func(_ context.Context, evt pkgcore.Event) error {
		order = append(order, "first")
		if evt.Payload != "body" {
			t.Errorf("first handler payload = %v, want the published payload", evt.Payload)
		}
		return errors.New("first handler failed")
	})
	busA.Subscribe(eventType, func(context.Context, pkgcore.Event) error {
		order = append(order, "second")
		return nil
	})
	busA.Subscribe("other.type", func(context.Context, pkgcore.Event) error {
		order = append(order, "wrong-type")
		return errors.New("must not run")
	})

	err := busA.Publish(ctx, pkgcore.Event{Type: eventType, Payload: "body"})
	if err == nil || !strings.Contains(err.Error(), "handler 0 for event") {
		t.Fatalf("Publish() error = %v, want the failing handler named by index", err)
	}
	if strings.Join(order, ",") != "first,second" {
		t.Errorf("handler run order = %v, want exactly [first second]", order)
	}

	// Unserializable payload fails before the append.
	err = busA.Publish(ctx, pkgcore.Event{Type: eventType, Payload: make(chan int)})
	if err == nil || !strings.Contains(err.Error(), "not JSON-serializable") {
		t.Fatalf("Publish() with an unserializable payload error = %v, want a JSON error", err)
	}
	if n := redisClientOf(busA).XLen(ctx, eventStreamKey(eventType)).Val(); n != 2 {
		t.Errorf("stream length = %d, want 2 (the two successful publishes only)", n)
	}

	// Subscribe after Close registers nothing.
	busA.Close()
	invocations := 0
	busA.Subscribe(eventType, func(context.Context, pkgcore.Event) error { invocations++; return nil })
	if err := busA.Publish(ctx, pkgcore.Event{Type: eventType, Payload: "late"}); !errors.Is(err, ErrEventBusClosed) {
		t.Errorf("Publish after Close error = %v, want ErrEventBusClosed", err)
	}
}

// TestEventBus_PanickingLocalHandlerIsContained_OverMiniredis pins the
// containment of a local handler panic on the wire: recovered, logged, the
// publish stays a success, and the handler's siblings still run.
func TestEventBus_PanickingLocalHandlerIsContained_OverMiniredis(t *testing.T) {
	_, busA, _ := redisBuses(t)
	defer busA.Close()
	ctx := context.Background()
	const eventType = "panic.contained"

	panicked, siblings := 0, 0
	busA.Subscribe(eventType, func(context.Context, pkgcore.Event) error {
		panicked++
		panic("local handler bug")
	})
	busA.Subscribe(eventType, func(context.Context, pkgcore.Event) error {
		siblings++
		return nil
	})

	if err := busA.Publish(ctx, pkgcore.Event{Type: eventType, Payload: "x"}); err != nil {
		t.Fatalf("Publish() with a panicking local handler error = %v, want nil", err)
	}
	if panicked != 1 {
		t.Errorf("panicking handler ran %d times, want 1", panicked)
	}
	if siblings != 1 {
		t.Errorf("sibling handler ran %d times, want 1", siblings)
	}
}
