//go:build integration

package nats_test

// Regression tests for the containment of a panicking LOCAL handler -- one
// subscribed on the same instance whose Publish call delivers it: the local
// fan-out runs synchronously on the publishing goroutine (see Publish's own
// doc comment), and before this fix the handlers were invoked bare, so a
// panicking handler unwound through Publish into whatever the publisher's
// own stack was (a jobs handler, a request handler, a goroutine with no
// recover of its own) -- contradicting this package's own claim that the
// local path behaves exactly like pkgcore's in-memory bus, whose
// per-handler recover (runMemoryBusHandler) contains panics, logs them, and
// lets the handler's siblings still run (the reader path's own
// runRemoteHandler already contained panics on OTHER replicas; the local
// fan-out was the hole). Post-fix the fan-out runs each handler through
// runLocalHandler, which recovers the panic, logs it, drops it (never an
// error a caller could act on), lets the handler's siblings still run, and
// keeps the publish a success.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	eventbusnats "github.com/vislake/speed/go/pkgcore/eventbus/nats"
)

// TestEventBus_PanickingLocalHandler_ContainedSiblingsRun pins the
// containment contract against a real NATS server: a Publish whose first
// local handler panics must return normally (never unwind the caller), the
// handlers registered after the panicking one must still run, and ordinary
// handler errors must still surface through the joined error. Failing
// before the fix: the first Publish panicked through the test goroutine,
// so the test died at the panic and the sibling never ran.
func TestEventBus_PanickingLocalHandler_ContainedSiblingsRun(t *testing.T) {
	ctx := context.Background()
	connA, _ := startNATSConnPair(t, ctx)
	bus := eventbusnats.NewEventBus(connA)
	t.Cleanup(bus.Close)

	const eventType = "eventbus_nats_test.local_panic_contained"
	var panickedCalls atomic.Int64
	bus.Subscribe(eventType, func(context.Context, pkgcore.Event) error {
		panickedCalls.Add(1)
		panic("local handler bug (containment regression)")
	})
	errSibling := errors.New("sibling failure (error path regression)")
	var errCalls atomic.Int64
	bus.Subscribe(eventType, func(context.Context, pkgcore.Event) error {
		errCalls.Add(1)
		return errSibling
	})
	sibling := &eventRecorder{}
	bus.Subscribe(eventType, sibling.handler())

	for seq := 1; seq <= 2; seq++ {
		if err := bus.Publish(ctx, pkgcore.Event{Type: eventType, TenantID: pkgcore.TenantID("tenant-acme"), Payload: map[string]any{"seq": seq}}); err == nil {
			t.Fatalf("Publish(%d) error = nil, want the erroring sibling's error (handler errors must still surface past a contained panic)", seq)
		}
	}
	if got := sibling.count(); got != 2 {
		t.Fatalf("healthy sibling received %d events, want 2 (it must still run after the panicking handler)", got)
	}
	if got := panickedCalls.Load(); got != 2 {
		t.Fatalf("panicking handler ran %d times, want 2 (one per publish, its panic contained each time)", got)
	}
	if got := errCalls.Load(); got != 2 {
		t.Fatalf("erroring sibling ran %d times, want 2", got)
	}
}
