//go:build integration

package nats_test

// Regression tests for the delivery semantics of a panicking remote handler
// on the NATS-backed bus. The scenario every test drives: a message whose
// remote delivery invokes a handler that panics. The contract under test:
// the panicked message is never acked as delivered, but its redelivery is
// bounded, and a panicking handler never drags its healthy siblings along.
// deliverRemote negatively acknowledges the panicked message (redelivered
// at panicRedeliveryDelay intervals) and records the panicked handler
// VALUES; every redelivery re-invokes ONLY those values, never the
// message's whole fan-out, so healthy siblings run exactly once and later
// same-type messages keep reaching them exactly once. Without that bounded
// shape, a leave-unacknowledged delivery -- the consumer carries no
// per-message budget on its own, and the JetStream server default is
// unlimited redelivery -- would re-run the whole fan-out after every
// AckWait: the panicking handler would re-run without bound, drag every
// healthy sibling along, and with enough accumulated unacknowledged
// messages suspend the consumer entirely at MaxAckPending: a permanent
// stall of the type on that replica. The consumer's own MaxDeliver
// (eventMaxDeliver) caps the attempts broker-side, and the same budget is
// enforced in-process: a still-panicking handler settles the message with a
// logged terminal line and a Term on its last allowed delivery -- never an
// unbounded redelivery loop and never an accumulating pile of
// unacknowledged messages.
//
// The AckWait shortening each test performs is scaffolding, not part of the
// mechanism under test: the tests reduce the consumer's AckWait so the
// leave-unacknowledged behaviour (which redelivers only when AckWait
// expires) shows itself in seconds instead of the server default's 30s,
// while the negative-acknowledgement pacing is independent of AckWait and
// unchanged by it.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/vislake/speed/go/pkgcore"
	eventbusnats "github.com/vislake/speed/go/pkgcore/eventbus/nats"
)

// maxDeliverBudget mirrors eventbus.go's unexported eventMaxDeliver: the
// integration tier observes the consumer's Config.MaxDeliver and the
// settled attempt count through this number, the same way the
// streamNameForEventType helper above mirrors the package's unexported
// function of the same name.
const maxDeliverBudget = 4

// flatWindow is the observation window the boundedness assertions use: four
// full redelivery cycles (a leave-unacknowledged redelivery lands on every
// shortened AckWait expiry, about a second apart) plus margin, long enough
// that a message still being redriven would have surfaced another change
// inside the window.
const flatWindow = 4600 * time.Millisecond

// settleDeadline bounds assertEventuallyFlat's wait for a value to stop
// changing. It is deliberately roomier than the mechanism's own schedule
// (the bounded redeliveries finish within a few seconds, paced by the
// broker): under a heavily loaded Docker host a redelivery can arrive late,
// and the deadline must only ever fail a test whose counter is genuinely
// still being driven, never one whose last change simply arrived slowly.
// For an unbounded redelivery no deadline suffices -- the redeliveries
// never stop.
const settleDeadline = 25 * time.Second

// shortAckWait is the consumer AckWait every test's redelivery scaffolding
// sets: the leave-unacknowledged path redelivers on AckWait expiry, so
// shortening it is what makes an unbounded redelivery visible at test
// scale (the negative-acknowledgement path paces its redeliveries on its
// own schedule and is unaffected by AckWait).
const shortAckWait = time.Second

// panicOn registers a handler that panics on every delivery whose payload
// sequence is targetSeq, counting the invocation into attempts first, and
// passes every other delivery (warm-up markers included) straight through.
// The sequence filter keeps warm-up messages clean so the FIRST panicking
// message is exactly the one each test publishes after warm-up, and keeps
// the attempt counter attributable to that one message.
func panicOn(t *testing.T, bus *eventbusnats.EventBus, eventType string, targetSeq float64, attempts *atomic.Int64) {
	t.Helper()
	bus.Subscribe(eventType, func(_ context.Context, evt pkgcore.Event) error {
		if seq, ok := sequenceOf(evt); ok && seq == targetSeq {
			attempts.Add(1)
			panic("remote handler bug (bounded-redelivery regression)")
		}
		return nil
	})
}

// sequenceOf extracts the "sequence" payload marker the tests below publish
// with, reporting whether the event carries one (warm-up markers carry a
// "seq" key instead and are deliberately not markers).
func sequenceOf(evt pkgcore.Event) (float64, bool) {
	payload, ok := evt.Payload.(map[string]any)
	if !ok {
		return 0, false
	}
	seq, ok := payload["sequence"].(float64)
	return seq, ok
}

// spyCountSeq returns how many events of the given payload sequence the spy
// has received.
func spyCountSeq(spy *eventRecorder, seq float64) int {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	n := 0
	for _, evt := range spy.evts {
		if s, ok := sequenceOf(evt); ok && s == seq {
			n++
		}
	}
	return n
}

// assertEventuallyFlat waits until value() has reached at least minValue and
// then stayed unchanged for a full flatWindow -- the shape of "this counter
// is no longer being driven by anything" -- and returns the settled value.
// For a message still being redelivered no such window exists: an
// unacknowledged message comes back on every (shortened) AckWait expiry,
// re-running the whole fan-out, so any counter its delivery touches changes
// again within every ~1s redelivery cycle and the deadline expires
// instead.
func assertEventuallyFlat(t *testing.T, what string, minValue int64, value func() int64) int64 {
	t.Helper()
	deadline := time.Now().Add(settleDeadline)
	var last int64 = -1
	var lastChange time.Time
	for {
		v := value()
		now := time.Now()
		if v != last {
			last = v
			lastChange = now
		}
		if v >= minValue && now.Sub(lastChange) >= flatWindow {
			return v
		}
		if now.After(deadline) {
			t.Fatalf("timed out waiting for %s to settle at or above %d: last value %d, still changing every redelivery cycle (pre-fix unbounded-redelivery behaviour)", what, minValue, last)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// shortenAckWait rewrites the consumer's AckWait to shortAckWait so the
// leave-unacknowledged redelivery cadence fits the test window (see the
// file's doc comment). The update restates the consumer's own current
// configuration -- the bus created it -- changing only AckWait, so a
// consumer carrying no MaxDeliver (the unlimited server default) stays
// unlimited and the unbounded redelivery under test is real, not capped by
// the scaffolding.
func shortenAckWait(t *testing.T, ctx context.Context, js jetstream.JetStream, streamName, consumerName string) {
	t.Helper()
	consumer, err := js.Consumer(ctx, streamName, consumerName)
	if err != nil {
		t.Fatalf("js.Consumer(%q, %q): %v", streamName, consumerName, err)
	}
	info, err := consumer.Info(ctx)
	if err != nil {
		t.Fatalf("consumer.Info(%q): %v", consumerName, err)
	}
	cfg := info.Config
	cfg.AckWait = shortAckWait
	if _, err := js.CreateOrUpdateConsumer(ctx, streamName, cfg); err != nil {
		t.Fatalf("shortenAckWait: %v", err)
	}
}

// TestEventBus_PanickingRemoteHandler_LaterSameTypeMessagesStillReachHealthyHandlers
// pins the property its redis twin asserts for its own backend (see the
// file's doc comment): after one message whose delivery panics, LATER
// messages of the same type must still reach the healthy handler, each
// exactly once. An unbounded redelivery would re-run the whole fan-out on
// every cycle -- the healthy handler would see the panicked message again
// and the exactly-once assertion below would fail the moment the first
// redelivery lands.
func TestEventBus_PanickingRemoteHandler_LaterSameTypeMessagesStillReachHealthyHandlers(t *testing.T) {
	ctx := context.Background()
	connA, connB := startNATSConnPair(t, ctx)
	busA := eventbusnats.NewEventBus(connA) // publisher only
	busB := eventbusnats.NewEventBus(connB)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
	})

	const eventType = "invoice.panicked"
	var attempts atomic.Int64
	spy := &eventRecorder{}
	panicOn(t, busB, eventType, 100, &attempts) // registered first, like the redis twin test
	busB.Subscribe(eventType, spy.handler())

	warmUp(t, busA, spy, eventType)
	spy.clear()

	js, err := jetstream.New(connA)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	streamName := streamNameForEventType(eventType)
	shortenAckWait(t, ctx, js, streamName, soleConsumerName(t, ctx, js, streamName))

	// One panicking message, then two healthy messages of the same type
	// published AFTER it.
	for _, seq := range []float64{100, 200, 201} {
		if err := busA.Publish(ctx, pkgcore.Event{
			Type:     eventType,
			TenantID: pkgcore.TenantID("tenant-panic"),
			Payload:  map[string]any{"sequence": seq},
		}); err != nil {
			t.Fatalf("Publish(%v) error = %v, want nil", seq, err)
		}
	}

	// The later messages must reach the healthy handler despite the earlier
	// message's panicking handler, and so must the panicking message's own
	// first (and only) healthy delivery.
	eventually(t, "the healthy handler to receive every message published after the panicking one", func() bool {
		return spyCountSeq(spy, 100) >= 1 && spyCountSeq(spy, 200) >= 1 && spyCountSeq(spy, 201) >= 1
	})

	// Exactly once each, through the whole bounded-redelivery window: the
	// panicked message's redeliveries re-invoke the panicking handler value
	// only, never the healthy sibling, so no count below may move -- a
	// whole-fan-out redelivery would make the healthy handler's count for
	// the panicked message grow on every cycle.
	time.Sleep(flatWindow)
	for _, seq := range []float64{100, 200, 201} {
		if got := spyCountSeq(spy, seq); got != 1 {
			t.Errorf("healthy handler received sequence %v %d times, want exactly 1: a panicking sibling's redeliveries re-ran it", seq, got)
		}
	}
}

// TestEventBus_PanickingRemoteHandler_HealthySiblingDoesNotRerun pins that a
// healthy sibling handler does not re-run because of a panicking one: the
// panicked message's redeliveries are scoped to the panicked handler value,
// so the healthy sibling's count for the panicked message must settle at
// exactly 1: a redelivery cycle re-running the message's whole fan-out
// would make the sibling's count grow on every ~1s AckWait redelivery,
// never settling.
func TestEventBus_PanickingRemoteHandler_HealthySiblingDoesNotRerun(t *testing.T) {
	ctx := context.Background()
	connA, connB := startNATSConnPair(t, ctx)
	busA := eventbusnats.NewEventBus(connA) // publisher only
	busB := eventbusnats.NewEventBus(connB)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
	})

	const eventType = "invoice.panicked"
	var attempts atomic.Int64
	spy := &eventRecorder{}
	panicOn(t, busB, eventType, 100, &attempts) // panicking sibling, registered first
	busB.Subscribe(eventType, spy.handler())    // the healthy sibling

	// Warm up WITHOUT a panicking message in play: warm-up markers pass
	// straight through panicOn (their payload carries no sequence marker),
	// so the first message this replica ever panics on is exactly the one
	// published below.
	warmUp(t, busA, spy, eventType)
	spy.clear()

	js, err := jetstream.New(connA)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	streamName := streamNameForEventType(eventType)
	shortenAckWait(t, ctx, js, streamName, soleConsumerName(t, ctx, js, streamName))

	if err := busA.Publish(ctx, pkgcore.Event{
		Type:     eventType,
		TenantID: pkgcore.TenantID("tenant-panic"),
		Payload:  map[string]any{"sequence": float64(100)},
	}); err != nil {
		t.Fatalf("Publish(100) error = %v, want nil", err)
	}

	// The healthy sibling must see the panicked message exactly once, and
	// the panicking handler must be retried at least once (the message is
	// not acked-as-delivered) WITHOUT the sibling being re-invoked: the
	// sibling count settles at 1 while the attempts counter climbs to its
	// budget. Without the value-scoped redelivery the sibling count would
	// grow with every cycle and never settle.
	settled := assertEventuallyFlat(t, "the healthy sibling's count for the panicked message", 1, func() int64 {
		return int64(spyCountSeq(spy, 100))
	})
	if settled != 1 {
		t.Errorf("healthy sibling ran %d times for the panicked message, want exactly 1: a panicking sibling must not re-run its siblings", settled)
	}
	if got := attempts.Load(); got < 2 {
		t.Errorf("panicking handler attempts = %d, want >= 2: the panicked message must be retried, not silently acked as delivered", got)
	}
}

// TestEventBus_PanickingRemoteHandler_BoundedRetryThenLoggedTerminal pins
// that the panicked message's own redelivery is bounded: the panicking
// handler is retried (at-least-once, the "never acked-as-delivered" intent)
// but only up to the eventMaxDeliver budget, after which the message
// settles with a terminal log line and leaves nothing pending -- never an
// unbounded redelivery loop and never an accumulating pile of
// unacknowledged messages. The consumer must carry the broker-side
// MaxDeliver cap (the assertion below reads it straight from the consumer
// config), and the in-process attempt count must stop at the budget.
func TestEventBus_PanickingRemoteHandler_BoundedRetryThenLoggedTerminal(t *testing.T) {
	ctx := context.Background()
	connA, connB := startNATSConnPair(t, ctx)
	busA := eventbusnats.NewEventBus(connA) // publisher only
	busB := eventbusnats.NewEventBus(connB)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
	})

	const eventType = "invoice.panicked"
	var attempts atomic.Int64
	spy := &eventRecorder{}
	panicOn(t, busB, eventType, 100, &attempts)
	busB.Subscribe(eventType, spy.handler())
	warmUp(t, busA, spy, eventType)
	spy.clear()

	js, err := jetstream.New(connA)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	streamName := streamNameForEventType(eventType)
	consumerName := soleConsumerName(t, ctx, js, streamName)

	// The consumer's own MaxDeliver must bound redeliveries broker-side:
	// without the cap JetStream's default is unlimited (-1).
	consumer, err := js.Consumer(ctx, streamName, consumerName)
	if err != nil {
		t.Fatalf("js.Consumer: %v", err)
	}
	info, err := consumer.Info(ctx)
	if err != nil {
		t.Fatalf("consumer.Info: %v", err)
	}
	if got := info.Config.MaxDeliver; got != maxDeliverBudget {
		t.Fatalf("consumer MaxDeliver = %d, want %d: without the cap the broker redelivers an unacknowledged panicked message without bound", got, maxDeliverBudget)
	}
	shortenAckWait(t, ctx, js, streamName, consumerName)

	// Capture every panic and terminal log line this test's own message
	// produces (slog.Default is what runRemoteHandler and the budget
	// exhaustion path log through; warm-up messages never panic, so nothing
	// before the publish below is captured).
	captured := &logCapture{}
	previous := slog.Default()
	slog.SetDefault(slog.New(captured))
	defer slog.SetDefault(previous)

	if err := busA.Publish(ctx, pkgcore.Event{
		Type:     eventType,
		TenantID: pkgcore.TenantID("tenant-panic"),
		Payload:  map[string]any{"sequence": float64(100)},
	}); err != nil {
		t.Fatalf("Publish(100) error = %v, want nil", err)
	}

	// Retried (>= 2 attempts), then settled at exactly the budget: the
	// count must stay put through two full idle redelivery windows.
	settled := assertEventuallyFlat(t, "the panicking handler's attempt count", 2, attempts.Load)
	if settled != maxDeliverBudget {
		t.Errorf("panicking handler ran %d times, want exactly the %d-attempt budget", settled, maxDeliverBudget)
	}

	// The settlement is honest, never silent: the message's last delivery
	// exhausts the budget and logs a terminal line naming the failure, and
	// every panicking invocation left its own trace.
	if n := captured.countMatching(func(r slog.Record) bool {
		return strings.Contains(r.Message, "exhausted its redelivery budget")
	}); n < 1 {
		t.Errorf("no terminal log line after the redelivery budget was exhausted, want at least 1: the message must settle honestly, never silently")
	}
	panicLogs := captured.countMatching(func(r slog.Record) bool {
		return strings.Contains(r.Message, "remote handler panicked")
	})
	if panicLogs != int(attempts.Load()) {
		t.Errorf("captured %d panic log lines for %d handler invocations: every panic must be logged", panicLogs, attempts.Load())
	}

	// Nothing stays pending once the budget is exhausted: the message was
	// terminated, so the consumer's unacknowledged count returns to zero
	// instead of accumulating without bound (the MaxAckPending-stall shape
	// of an unbounded redelivery).
	eventually(t, "the consumer's unacknowledged count to return to zero after the budget settlement", func() bool {
		return ackPending(t, ctx, js, streamName, consumerName) == 0
	})
}

// TestEventBus_NoPanickingHandler_ControlDeliveryExactlyOnce is the control
// shape for the suite: without a panicking handler the same publish
// sequence must be delivered to the healthy handler exactly once.
func TestEventBus_NoPanickingHandler_ControlDeliveryExactlyOnce(t *testing.T) {
	ctx := context.Background()
	connA, connB := startNATSConnPair(t, ctx)
	busA := eventbusnats.NewEventBus(connA) // publisher only
	busB := eventbusnats.NewEventBus(connB)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
	})

	const eventType = "invoice.control"
	spy := &eventRecorder{}
	busB.Subscribe(eventType, spy.handler())
	warmUp(t, busA, spy, eventType)
	spy.clear()

	for _, seq := range []float64{100, 200, 201} {
		if err := busA.Publish(ctx, pkgcore.Event{
			Type:     eventType,
			TenantID: pkgcore.TenantID("tenant-control"),
			Payload:  map[string]any{"sequence": seq},
		}); err != nil {
			t.Fatalf("Publish(%v) error = %v, want nil", seq, err)
		}
	}
	eventually(t, "the healthy handler to receive every published message", func() bool {
		return spyCountSeq(spy, 100) >= 1 && spyCountSeq(spy, 200) >= 1 && spyCountSeq(spy, 201) >= 1
	})
	time.Sleep(flatWindow)
	for _, seq := range []float64{100, 200, 201} {
		if got := spyCountSeq(spy, seq); got != 1 {
			t.Errorf("handler received sequence %v %d times, want exactly 1", seq, got)
		}
	}
}

// ackPending reads the consumer's NumAckPending -- the number of messages
// delivered but not acknowledged.
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

// logCapture is a slog.Handler that records every record it handles,
// thread-safely, for the terminal-settlement assertions above.
type logCapture struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, r.Clone())
	return nil
}

func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

func (c *logCapture) countMatching(match func(slog.Record) bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.recs {
		if match(r) {
			n++
		}
	}
	return n
}
