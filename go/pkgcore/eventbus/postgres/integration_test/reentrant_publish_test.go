//go:build integration

// Re-entrant-publish regression tests: a handler that, upon receiving an
// event, publishes another event on the same bus it was invoked by. This is
// the re-entrancy handlersFor's doc comment has always promised ("a handler
// is free to call Subscribe or Publish re-entrantly"), but Publish and
// deliverPending both ran handlers while holding deliverMu, so a re-entrant
// Publish self-deadlocked on the second deliverMu.Lock -- a sync.Mutex is
// not re-entrant. Both delivery arms are covered, because they deadlock
// identically but are reached differently: Publish's own synchronous
// local-delivery arm (the nested Publish runs on the publisher's own
// goroutine, which already holds deliverMu), and the listener goroutine's
// catch-up arm -- the one a real cross-replica deployment dies on, a
// handler for an event the LISTEN connection just received from another
// replica's Publish, nested-publishing on the listener goroutine while
// deliverPending holds deliverMu.
//
// Each nested-publish assertion is wrapped in a goroutine plus a bounded
// deadline, so a deadlock fails the test instead of hanging the run, and
// bus cleanup is likewise deadline-protected: Close waits for the listener
// goroutine, which a deadlocked deliverPending would hold forever.
package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	eventbuspostgres "github.com/vislake/speed/go/pkgcore/eventbus/postgres"
)

// The re-entrancy shape these tests drive is the one real business modules
// exhibit: authn publishes authn.user.created, and the consumer handling it
// (org's "ensure the default root node exists" logic, say) publishes a
// follow-up event in the same handler.
const (
	reentrantUserCreatedEvent = "authn.user.created"
	reentrantOrgRootEvent     = "org.root.ensure"
)

// reentrantDeadlockTimeout bounds the outer Publish (and bus Close below),
// mirroring pkgcore's own deadlockTimeout: a re-entrant Publish on the
// pre-fix code blocks forever on deliverMu, so the wait must fail rather
// than hang CI.
const reentrantDeadlockTimeout = 5 * time.Second

// reentrantQuiescePeriod is how long a test waits after the nested event's
// first delivery before asserting it was delivered exactly once, the same
// window cursor_advance_retry_test.go uses for the same purpose: the
// catch-up poller wakes on every NOTIFY and on every listenBlock timeout,
// so a double-delivery by the poller would surface within one listenBlock
// of the nested publish, and the sleep lets at least two poll cycles pass
// before the exactly-once assertion runs.
const reentrantQuiescePeriod = 5 * time.Second

// closeBusWithin registers a cleanup that closes bus on a background
// goroutine, failing the test -- never hanging the run -- if Close does not
// return within reentrantDeadlockTimeout. Close must wait for the listener
// goroutine (listenDone), and a listener stuck on deliverMu -- the pre-fix
// deadlock, where its deliverPending holds the lock while a handler's
// re-entrant Publish waits for it forever -- would make a bare
// t.Cleanup(bus.Close) hang the whole test binary instead of reporting the
// deadlock this file exists to prove. The wedged goroutine is left to die
// with the process in that case.
func closeBusWithin(t *testing.T, bus *eventbuspostgres.EventBus) {
	t.Helper()
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() {
			bus.Close()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(reentrantDeadlockTimeout):
			t.Error("Close() did not return: the listener goroutine is stuck holding deliverMu (the pre-fix re-entrancy deadlock)")
		}
	})
}

// TestEventBus_HandlerMayPublishReentrantly_LocalDeliveryPath covers the
// local-delivery arm of the deadlock: the handler runs synchronously inside
// Publish, on the publisher's own goroutine, and its own nested Publish on
// the same bus must not deadlock. On the pre-fix code the nested Publish
// blocks forever on deliverMu (already held by the outer Publish on this
// very goroutine), so the goroutine-wrapped outer Publish times out.
func TestEventBus_HandlerMayPublishReentrantly_LocalDeliveryPath(t *testing.T) {
	ctx := context.Background()
	pool := startPostgresPool(t, ctx)

	bus := eventbuspostgres.NewEventBus(pool, "reentrant-local-replica")
	closeBusWithin(t, bus)

	rootSpy := &eventSpy{}
	bus.Subscribe(reentrantOrgRootEvent, rootSpy.handler())
	bus.Subscribe(reentrantUserCreatedEvent, func(ctx context.Context, _ pkgcore.Event) error {
		// The nested publish that used to self-deadlock: Publish ran this
		// handler while holding deliverMu, and this call tries to take
		// deliverMu again on the same goroutine.
		return bus.Publish(ctx, pkgcore.Event{Type: reentrantOrgRootEvent})
	})

	done := make(chan error, 1)
	go func() {
		done <- bus.Publish(ctx, pkgcore.Event{Type: reentrantUserCreatedEvent})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Publish() error = %v, want nil", err)
		}
	case <-time.After(reentrantDeadlockTimeout):
		t.Fatalf("Publish() deadlocked: a handler could not Publish %q re-entrantly within %v",
			reentrantOrgRootEvent, reentrantDeadlockTimeout)
	}

	// The nested event's local delivery is synchronous, so its handler ran
	// before the outer Publish returned. Wait out the catch-up poller's
	// window anyway, then assert exactly once: the nested event's outbox row
	// must never be redelivered by the poller, which is the double-delivery
	// hazard the fix's in-flight gate exists to rule out.
	time.Sleep(reentrantQuiescePeriod)
	if got := rootSpy.count(); got != 1 {
		t.Errorf("nested handler invoked %d times, want exactly 1 (no duplicate from the catch-up path)", got)
	}
}

// TestEventBus_HandlerMayPublishReentrantly_ListenerCatchUpPath covers the
// listener arm of the deadlock, the one a real deployment dies on: another
// replica (publisher) publishes the event, the subscriber's listener
// goroutine receives the NOTIFY and starts delivering it, so the handler
// runs on the listener goroutine while deliverPending holds deliverMu -- and
// the handler's own re-entrant Publish on the subscriber waits for that
// same lock forever. The subscriber's listener never delivers the nested
// event (its own nested Publish wrote the outbox row, but the wedged
// listener can never advance past it), so the exactly-once assertion below
// times out on the pre-fix code.
func TestEventBus_HandlerMayPublishReentrantly_ListenerCatchUpPath(t *testing.T) {
	ctx := context.Background()
	pool := startPostgresPool(t, ctx)

	publisher := eventbuspostgres.NewEventBus(pool, "reentrant-publisher")
	closeBusWithin(t, publisher)

	subscriber := eventbuspostgres.NewEventBus(pool, "reentrant-subscriber")
	closeBusWithin(t, subscriber)

	rootSpy := &eventSpy{}
	subscriber.Subscribe(reentrantOrgRootEvent, rootSpy.handler())

	// warmUp's markers (sequence -1) must not trigger the re-entrant
	// publish: they exist only to prove the subscriber's listener is
	// established and its (replicaID, eventType) cursor is initialized, and
	// a marker-triggered nested publish would pollute the exactly-once
	// assertion below. warmSpy records the markers so warmUp can observe
	// them; the handler after it ignores them and reacts only to real
	// events.
	warmSpy := &eventSpy{}
	subscriber.Subscribe(reentrantUserCreatedEvent, warmSpy.handler())
	subscriber.Subscribe(reentrantUserCreatedEvent, func(ctx context.Context, evt pkgcore.Event) error {
		if seq, ok := sequenceOf(evt); ok && seq == -1 {
			return nil // warm-up marker (see warmUp's own doc comment)
		}
		return subscriber.Publish(ctx, pkgcore.Event{Type: reentrantOrgRootEvent})
	})

	warmUp(t, ctx, publisher, reentrantUserCreatedEvent, warmSpy)

	if err := publisher.Publish(ctx, pkgcore.Event{
		Type:     reentrantUserCreatedEvent,
		TenantID: pkgcore.TenantID("tenant-reentrant"),
		Payload:  map[string]any{"sequence": float64(42)},
	}); err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}

	// The subscriber's nested publish is delivered synchronously on its own
	// listener goroutine, so rootSpy reaches 1 as soon as the handler above
	// runs -- which, on the pre-fix code, it never does.
	eventually(t, "the listener-path handler's re-entrant publish to be delivered", func() bool {
		return rootSpy.count() >= 1
	})

	// Wait out the catch-up poller's window, then assert exactly once: the
	// nested event's outbox row must never be redelivered by the subscriber's
	// own poller -- the double-delivery hazard the fix's in-flight gate
	// exists to rule out.
	time.Sleep(reentrantQuiescePeriod)
	if got := rootSpy.count(); got != 1 {
		t.Errorf("nested handler invoked %d times, want exactly 1 (no duplicate from the catch-up path)", got)
	}
}
