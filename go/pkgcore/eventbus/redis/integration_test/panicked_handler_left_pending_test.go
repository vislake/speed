//go:build integration

package redis_test

// Regression tests for the ack semantics of a panicking remote handler: an
// entry whose remote handler panicked must not be acknowledged as delivered
// (a handler whose side effects never -- or only partly -- ran was not
// delivered), and the panic must leave a visible trace rather than being
// swallowed. Before this fix the reader recovered the panic and acked the
// entry anyway: the event was gone, the panic produced no signal anywhere,
// and an operator inspecting the consumer group's pending set found nothing
// (see runRemoteHandler's own doc comment).

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/pkgcore"
	eventbusredis "github.com/vislake/speed/go/pkgcore/eventbus/redis"
)

// TestEventBus_PanickingRemoteHandler_EntryLeftPendingInTheGroup pins that
// every entry whose remote handler panicked stays pending in the
// reader's consumer group: the group's pending count must grow by exactly
// the number of panicked entries, while the reader keeps delivering later
// entries to the healthy handler that follows the panicking one.
func TestEventBus_PanickingRemoteHandler_EntryLeftPendingInTheGroup(t *testing.T) {
	ctx := context.Background()
	client := startRedisClient(t, ctx)
	busA := eventbusredis.NewEventBus(client)
	busB := eventbusredis.NewEventBus(client)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
	})

	const panickedType = "invoice.panicked"
	var attempts atomic.Int64
	recB := &eventRecorder{}
	busB.Subscribe(panickedType, func(context.Context, pkgcore.Event) error {
		attempts.Add(1)
		panic("remote handler bug")
	})
	busB.Subscribe(panickedType, recB.handler())

	// Reader readiness: publish until the recorder has seen a delivery (the
	// panicking handler runs first and recovers, the healthy one still gets
	// the event).
	deadline := time.Now().Add(5 * time.Second)
	for seq := 1; recB.count() == 0; seq++ {
		if err := busA.Publish(ctx, pkgcore.Event{Type: panickedType, TenantID: pkgcore.TenantID("warmup"), Payload: map[string]any{"seq": seq}}); err != nil {
			t.Fatalf("warm-up publish %d: %v", seq, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("warm-up: no event reached the healthy handler within 5s")
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(600 * time.Millisecond) // one full read block: let the warm-up settle
	recB.clear()
	warmupPending := pendingAcrossGroups(t, ctx, client, streamKey(panickedType))
	if warmupPending == 0 {
		t.Fatalf("warm-up panicked entry was acknowledged, want it pending: the panic-ack regression is already visible")
	}

	// Three further panicked events: each must stay pending, and the healthy
	// handler must still receive each one.
	for seq := 1; seq <= 3; seq++ {
		if err := busA.Publish(ctx, pkgcore.Event{Type: panickedType, TenantID: pkgcore.TenantID("tenant-acme"), Payload: map[string]any{"seq": seq}}); err != nil {
			t.Fatalf("Publish(%d) error = %v, want nil", seq, err)
		}
	}

	eventually(t, "the healthy handler to run for all three events", func() bool {
		return recB.count() == 3
	})

	eventually(t, "all four panicked entries to sit pending in the group", func() bool {
		return pendingAcrossGroups(t, ctx, client, streamKey(panickedType)) >= 4
	})

	if got := attempts.Load(); got != 4 {
		t.Errorf("panicking handler attempts = %d, want 4 (each entry delivered exactly once; pending entries are not re-read)", got)
	}
}

// pendingAcrossGroups sums the pending (unacknowledged) entries across every
// consumer group on the stream -- the observable shape of "an entry was not
// acked as delivered".
func pendingAcrossGroups(t *testing.T, ctx context.Context, client *redis.Client, stream string) int64 {
	t.Helper()

	groups, err := client.XInfoGroups(ctx, stream).Result()
	if err != nil {
		t.Fatalf("XInfoGroups(%q): %v", stream, err)
	}
	var total int64
	for _, g := range groups {
		total += g.Pending
	}
	return total
}
