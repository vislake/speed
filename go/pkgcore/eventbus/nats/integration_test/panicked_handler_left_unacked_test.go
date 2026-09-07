//go:build integration

package nats_test

// Regression tests for the ack semantics of a panicking remote handler on
// the NATS-backed bus: a message whose remote handler panicked must not be
// acknowledged as delivered (a handler whose side effects never -- or only
// partly -- ran was not delivered), and the panic must leave a visible
// trace rather than being swallowed. Before this fix the reader recovered
// the panic and acked the message anyway: the event was gone, the panic
// produced no signal anywhere, and JetStream's own consumer state showed
// nothing (see runRemoteHandler's own doc comment).

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/vislake/speed/go/pkgcore"
	eventbusnats "github.com/vislake/speed/go/pkgcore/eventbus/nats"
)

// TestEventBus_PanickingRemoteHandler_MessageLeftUnacknowledged pins that a
// message whose remote handler panicked stays unacknowledged on the
// reader's durable consumer: the consumer's NumAckPending must grow by
// exactly the number of panicked messages, while the reader keeps
// delivering later messages to the healthy handler that follows the
// panicking one. An unacknowledged message is JetStream's own redelivery
// trigger, which is the honest at-least-once answer to a delivery whose
// side effects never ran.
func TestEventBus_PanickingRemoteHandler_MessageLeftUnacknowledged(t *testing.T) {
	ctx := context.Background()
	connA, connB := startNATSConnPair(t, ctx)
	busA := eventbusnats.NewEventBus(connA) // publisher only
	busB := eventbusnats.NewEventBus(connB)
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

	// Reader readiness: publish until the healthy handler has seen a
	// delivery (the panicking handler runs first and is recovered).
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
	recB.clear()

	js, err := jetstream.New(connA)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	streamName := streamNameForEventType(panickedType)
	consumerName := soleConsumerName(t, ctx, js, streamName)

	if pending := ackPending(t, ctx, js, streamName, consumerName); pending == 0 {
		t.Fatal("the warm-up panicked message was acknowledged, want it unacknowledged: the panic-ack regression is already visible")
	}

	// Three further panicked messages: each must stay unacknowledged, and
	// the healthy handler must still receive each one.
	for seq := 1; seq <= 3; seq++ {
		if err := busA.Publish(ctx, pkgcore.Event{Type: panickedType, TenantID: pkgcore.TenantID("tenant-acme"), Payload: map[string]any{"seq": seq}}); err != nil {
			t.Fatalf("Publish(%d) error = %v, want nil", seq, err)
		}
	}

	eventually(t, "the healthy handler to run for all three messages", func() bool {
		return recB.count() == 3
	})

	eventually(t, "all four panicked messages to sit unacknowledged on the consumer", func() bool {
		return ackPending(t, ctx, js, streamName, consumerName) >= 4
	})
}

// ackPending reads the consumer's NumAckPending -- the number of messages
// delivered but not acknowledged -- the observable shape of "a message was
// not acked as delivered".
func ackPending(t *testing.T, ctx context.Context, js jetstream.JetStream, streamName, consumerName string) int {
	t.Helper()

	consumer, err := js.Consumer(ctx, streamName, consumerName)
	if err != nil {
		t.Fatalf("js.Consumer(%q, %q): %v", streamName, consumerName, err)
	}
	info, err := consumer.Info(ctx)
	if err != nil {
		t.Fatalf("consumer.Info: %v", err)
	}
	return info.NumAckPending
}
