//go:build integration

package postgres_test

// Regression test for the delivery semantics of a panicking remote handler
// on the PostgreSQL-backed bus: a row whose remote handler panicked must
// not be treated as delivered -- its delivery mark must not land and its
// replica's cursor must not advance past it, so the listener's next
// catch-up cycle redelivers the row rather than the panic being silently
// swallowed -- and each panic must leave a visible trace. Before this fix
// the catch-up path recovered the panic and advanced the cursor anyway:
// the event was gone for that replica, and the panic produced no signal
// anywhere (see runHandlerRecovered's own doc comment).

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	eventbuspostgres "github.com/vislake/speed/go/pkgcore/eventbus/postgres"
)

// TestEventBus_PanickingRemoteHandler_RowRedeliveredNotDropped pins that a
// row whose remote handler panicked keeps being redelivered by the
// listener's catch-up cycles instead of being advanced past: after the
// warm-up that proves the listener is consuming, the panicking handler's
// attempt count must keep growing past its post-warm-up baseline. Before
// the fix the first catch-up scan recovered the panic and advanced the
// cursor past the row, so the count stalled at the baseline forever -- the
// event silently dropped for this replica.
func TestEventBus_PanickingRemoteHandler_RowRedeliveredNotDropped(t *testing.T) {
	ctx := context.Background()
	pool := startPostgresPool(t, ctx)

	publisher := eventbuspostgres.NewEventBus(pool, "panic-publisher")
	subscriber := eventbuspostgres.NewEventBus(pool, "panic-subscriber")
	t.Cleanup(func() {
		subscriber.Close()
		publisher.Close()
	})

	const panickedType = "invoice.panicked"
	var attempts atomic.Int64
	spy := &eventSpy{}
	subscriber.Subscribe(panickedType, func(context.Context, pkgcore.Event) error {
		attempts.Add(1)
		panic("remote handler bug")
	})
	subscriber.Subscribe(panickedType, spy.handler())

	// warmUp publishes until the healthy spy has received a delivery, which
	// proves the subscriber's listener is up and its cursor initialized.
	// Every warm-up row also runs (and panics) the first handler, so the
	// attempt counter starts at the warm-up count.
	warmUp(t, ctx, publisher, panickedType, spy)
	// Let any in-flight cycle settle, then take the baseline: post-fix, the
	// panicked warm-up rows sit unmarked behind the cursor and redeliver on
	// every subsequent cycle.
	time.Sleep(700 * time.Millisecond)
	baseline := attempts.Load()
	if baseline == 0 {
		t.Fatal("the panicking handler never ran during warm-up: the subscriber never delivered")
	}

	// The fix's property: the rows whose delivery panicked are redelivered
	// by the catch-up cycles (the listener scans on every NOTIFY and
	// listenBlock timeout, whose cadence is about two seconds), because a
	// panicked delivery neither marked its row nor advanced the cursor. A
	// delivery that was acked-as-done stalls at the baseline forever.
	deadline := time.Now().Add(convergenceDeadline)
	for attempts.Load() <= baseline {
		if time.Now().After(deadline) {
			t.Fatalf("panicked handler attempts stalled at %d (baseline %d) for %v: the panicked rows were treated as delivered and their cursor advanced past them (pre-fix behaviour), or redelivery is wedged", attempts.Load(), baseline, convergenceDeadline)
		}
		time.Sleep(150 * time.Millisecond)
	}
}
