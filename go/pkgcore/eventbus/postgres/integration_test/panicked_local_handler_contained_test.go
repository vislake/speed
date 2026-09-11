//go:build integration

package postgres_test

// Regression tests for the containment of a panicking LOCAL handler -- one
// subscribed on the same instance whose Publish call delivers it: the local
// fan-out runs synchronously on the publishing goroutine (see Publish's own
// doc comment), and before this fix the handlers were invoked bare, so a
// panicking handler unwound through Publish into whatever the publisher's
// own stack was (a jobs handler, a request handler, a goroutine with no
// recover of its own) -- contradicting this package's own claim that the
// local path behaves exactly like pkgcore's in-memory bus, which contains
// per-handler panics. The escape also skipped the row's locallyDelivered
// mark, leaving the row's fate to a catch-up redelivery that the scan may
// already have advanced past (the no-loss exception Publish's doc comment
// used to concede). Post-fix the fan-out runs each handler through
// runLocalHandler, which recovers the panic, logs it, lets the handler's
// siblings still run, keeps the publish a success, and marks the row
// delivered exactly like a panic-free one -- so the catch-up scan never
// re-runs the row's handlers (see runLocalHandler's own doc comment and
// Publish's defer comment).

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	eventbuspostgres "github.com/vislake/speed/go/pkgcore/eventbus/postgres"
	"github.com/vislake/speed/go/pkgcore/testkit"
)

// TestEventBus_PanickingLocalHandler_ContainedSiblingsRunRowMarked pins the
// containment contract on a real database: a Publish whose first local
// handler panics must return normally (never unwind the caller), the
// handlers registered after the panicking one must still run, ordinary
// handler errors must still surface through the joined error, and the row
// must be marked delivered so the catch-up scan never redelivers it -- the
// panicked and healthy handlers' invocation counts settle at exactly one
// per publish. Failing before the fix: the first Publish panicked through
// the test goroutine, so the test died at the panic; even a hypothetical
// recover around the call would have shown the siblings after the
// panicking handler never running and the row left unmarked for a
// redelivery that may never come.
func TestEventBus_PanickingLocalHandler_ContainedSiblingsRunRowMarked(t *testing.T) {
	ctx := context.Background()
	pool := startPostgresPool(t, ctx)
	bus := eventbuspostgres.NewEventBus(pool, "local-panic-contained")
	t.Cleanup(bus.Close)

	const eventType = "eventbus_postgres_test.local_panic_contained"

	var panickedCalls atomic.Int64
	bus.Subscribe(eventType, func(_ context.Context, evt pkgcore.Event) error {
		panickedCalls.Add(1)
		panic("local handler bug (containment regression)")
	})
	errSibling := errors.New("sibling failure (error path regression)")
	var errCalls atomic.Int64
	bus.Subscribe(eventType, func(_ context.Context, evt pkgcore.Event) error {
		errCalls.Add(1)
		return errSibling
	})
	spy := testkit.NewEventRecorder()
	bus.Subscribe(eventType, spy.Handler())

	for seq := 1; seq <= 2; seq++ {
		if err := bus.Publish(ctx, pkgcore.Event{Type: eventType, TenantID: pkgcore.TenantID("tenant-acme"), Payload: map[string]any{"sequence": float64(seq)}}); err == nil {
			t.Fatalf("Publish(%d) error = nil, want the erroring sibling's error (handler errors must still surface past a contained panic)", seq)
		}
	}
	if got := spy.Total(); got != 2 {
		t.Fatalf("healthy sibling received %d events, want 2 (it must still run after the panicking handler)", got)
	}
	if got := panickedCalls.Load(); got != 2 {
		t.Fatalf("panicking handler ran %d times, want 2 (one per publish, its panic contained each time)", got)
	}
	if got := errCalls.Load(); got != 2 {
		t.Fatalf("erroring sibling ran %d times, want 2", got)
	}

	// The row-marking half: the local delivery of each row was completed
	// and marked, so the catch-up scan (which wakes on this publish's own
	// NOTIFY and on every listenBlock timeout) must skip both rows rather
	// than re-run their handlers -- the panicked handler in particular
	// must not be picked up for the poller-path bounded retry, because
	// nothing was left unmarked for it to retry. Any redelivery would
	// drive all three counters past 2 within the flat window.
	settled := assertEventuallyFlat(t, "the local delivery counters to settle at 2 (no catch-up redelivery of the marked rows)", 2, func() int64 {
		return int64(spy.Total()) + panickedCalls.Load() + errCalls.Load()
	})
	if settled != 6 {
		t.Fatalf("delivery counters settled at %d, want 6 (2 publishes x 3 handlers): a redelivery re-ran one of the marked rows", settled)
	}
}
