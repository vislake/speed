//go:build integration

package postgres_test

// Regression tests for the delivery semantics of a panicking remote handler
// on the PostgreSQL-backed bus. The scenario every test drives: a row whose
// catch-up delivery invokes a handler that panics. The contract under test:
// a panicked delivery never blocks the cursor -- the row is advanced past
// exactly like a clean one, so later same-type rows keep flowing and the
// healthy sibling handlers never re-run -- while the row is not silently
// acked-as-delivered either: the panicked handler values are retried
// in-process, with a per-row spacing and a bounded attempt budget, and a
// handler that exhausts the budget settles with a terminal log line rather
// than an unbounded loop. Without that shape, a delivery path that returned
// before advancing the cursor would make the next catch-up cycle refetch
// the same row and hand it to ALL subscribers again: the panicking handler
// would re-run every cycle (re-joined by its healthy siblings), the type's
// later rows would never be delivered on this replica, and the cycle would
// repeat on every NOTIFY and every listenBlock timeout -- an unbounded hot
// loop permanently stalling the type. The redis reader never refetches a
// pending entry, so it needs no retry cap; the postgres scan would refetch
// forever, so its retry is capped.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	eventbuspostgres "github.com/vislake/speed/go/pkgcore/eventbus/postgres"
	"github.com/vislake/speed/go/pkgcore/testkit"
)

// flatWindow is the observation window the boundedness assertions use: two
// full idle wake cycles (a catch-up scan runs on every listenBlock timeout,
// about two seconds apart), long enough that a delivery still being redriven
// by the periodic scans would have surfaced another change inside the
// window.
const flatWindow = 4600 * time.Millisecond

// settleDeadline bounds assertEventuallyFlat's wait for a value to stop
// changing.
const settleDeadline = 15 * time.Second

// panicOn registers a handler that panics on every delivery whose payload
// sequence is targetSeq, counting the invocation into attempts first, and
// passes every other delivery (warm-up markers included) straight through.
// The sequence filter keeps warm-up rows clean so the FIRST panicking row is
// exactly the row each test publishes after warm-up, and keeps the attempt
// counter attributable to that one row.
func panicOn(t *testing.T, bus *eventbuspostgres.EventBus, eventType string, targetSeq float64, attempts *atomic.Int64) {
	t.Helper()
	bus.Subscribe(eventType, func(_ context.Context, evt pkgcore.Event) error {
		if seq, ok := sequenceOf(evt); ok && seq == targetSeq {
			attempts.Add(1)
			panic("remote handler bug (bounded-retry regression)")
		}
		return nil
	})
}

// spyCountSeq returns how many events of the given payload sequence the spy
// has received.
func spyCountSeq(spy *eventSpy, seq float64) int {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	n := 0
	for _, evt := range spy.events {
		if s, ok := sequenceOf(evt); ok && s == seq {
			n++
		}
	}
	return n
}

// assertEventuallyFlat waits until value() has reached at least minValue and
// then stayed unchanged for a full flatWindow -- the shape of "this counter
// is no longer being driven by anything" -- and returns the settled value.
// For a row still being redriven no such window exists: a panicked row
// redelivered by every catch-up cycle would keep changing any counter its
// delivery touches within every ~2s wake, and the deadline would expire
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
			t.Fatalf("timed out waiting for %s to settle at or above %d: last value %d, still changing every catch-up cycle (pre-fix redelivery behaviour)", what, minValue, last)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// TestEventBus_PanickingRemoteHandler_LaterSameTypeRowsStillReachHealthyHandlers
// pins the property its redis twin asserts for its own backend (see the
// file's doc comment): after one event whose delivery panics, LATER events
// of the same type must still reach the healthy handler, each exactly once.
// A panicked row must not wedge the type's cursor: a delivery that returned
// before advancing past the row would leave every later row undelivered on
// this replica.
func TestEventBus_PanickingRemoteHandler_LaterSameTypeRowsStillReachHealthyHandlers(t *testing.T) {
	ctx := context.Background()
	pool := startPostgresPool(t, ctx)

	const eventType = "invoice.panicked"
	var attempts atomic.Int64
	publisher := eventbuspostgres.NewEventBus(pool, "panic-later-publisher")
	subscriber := eventbuspostgres.NewEventBus(pool, "panic-later-subscriber")
	t.Cleanup(subscriber.Close)
	t.Cleanup(publisher.Close)

	spy := &eventSpy{}
	panicOn(t, subscriber, eventType, 100, &attempts) // registered first, like the redis twin test
	subscriber.Subscribe(eventType, spy.handler())

	warmUp(t, ctx, publisher, eventType, spy)

	// One panicking row, then two healthy rows of the same type published
	// AFTER it.
	for _, seq := range []float64{100, 200, 201} {
		if err := publisher.Publish(ctx, pkgcore.Event{
			Type:     eventType,
			TenantID: pkgcore.TenantID("tenant-panic"),
			Payload:  map[string]any{"sequence": seq},
		}); err != nil {
			t.Fatalf("Publish(%v) error = %v, want nil", seq, err)
		}
	}

	// The later rows must reach the healthy handler despite the earlier
	// row's panicking handler -- a wedged cursor would leave 200 and 201
	// undelivered.
	testkit.Eventually(t, "the healthy handler to receive every row published after the panicking one", func() bool {
		return spyCountSeq(spy, 200) >= 1 && spyCountSeq(spy, 201) >= 1 && spyCountSeq(spy, 100) >= 1
	})

	// Exactly once each, through the poller's window: the panicked row's
	// bounded retries re-invoke the panicking handler only, never the
	// healthy sibling, so no count below may move.
	time.Sleep(flatWindow)
	for _, seq := range []float64{100, 200, 201} {
		if got := spyCountSeq(spy, seq); got != 1 {
			t.Errorf("healthy handler received sequence %v %d times, want exactly 1: a panicking sibling redelivered the row to every subscriber", seq, got)
		}
	}
}

// TestEventBus_PanickingRemoteHandler_HealthySiblingDoesNotRerun pins that a
// healthy sibling handler does not re-run because of a panicking one: the
// panicked row's redelivery is scoped to the panicked handler values, so the
// healthy sibling's count for the panicked row must settle at exactly 1: a
// catch-up cycle redelivering the row to ALL subscribers would make the
// sibling's count grow without bound.
func TestEventBus_PanickingRemoteHandler_HealthySiblingDoesNotRerun(t *testing.T) {
	ctx := context.Background()
	pool := startPostgresPool(t, ctx)

	const eventType = "invoice.panicked"
	var attempts atomic.Int64
	publisher := eventbuspostgres.NewEventBus(pool, "panic-sibling-publisher")
	subscriber := eventbuspostgres.NewEventBus(pool, "panic-sibling-subscriber")
	t.Cleanup(subscriber.Close)
	t.Cleanup(publisher.Close)

	spy := &eventSpy{}
	panicOn(t, subscriber, eventType, 100, &attempts) // panicking sibling, registered first
	subscriber.Subscribe(eventType, spy.handler())    // the healthy sibling

	// Warm up WITHOUT the panicking handler in the delivery path: warm-up
	// rows pass straight through panicOn (their sequence is -1), so the
	// first row this replica ever panics on is exactly the one published
	// below, right after the cursor.
	warmUp(t, ctx, publisher, eventType, spy)

	if err := publisher.Publish(ctx, pkgcore.Event{
		Type:     eventType,
		TenantID: pkgcore.TenantID("tenant-panic"),
		Payload:  map[string]any{"sequence": float64(100)},
	}); err != nil {
		t.Fatalf("Publish(100) error = %v, want nil", err)
	}

	// The healthy sibling must see the panicked row exactly once, and the
	// panicking handler must be retried at least once (the row is not
	// acked-as-delivered) WITHOUT the sibling being re-invoked: the sibling
	// count settles at 1 while the attempts counter keeps climbing to its
	// budget. Without the value-scoped retry the sibling count would grow
	// with every catch-up cycle's redelivery and never settle.
	settled := assertEventuallyFlat(t, "the healthy sibling's count for the panicked row", 1, func() int64 {
		return int64(spyCountSeq(spy, 100))
	})
	if settled != 1 {
		t.Errorf("healthy sibling ran %d times for the panicked row, want exactly 1: a panicking sibling must not re-run its siblings", settled)
	}
	if got := attempts.Load(); got < 2 {
		t.Errorf("panicking handler attempts = %d, want >= 2: the panicked row must be retried, not silently acked as delivered", got)
	}
}

// TestEventBus_PanickingRemoteHandler_BoundedRetryThenLoggedTerminal pins
// that the panicked row's own redelivery is bounded: the panicking handler
// is retried (at-least-once -- never acked-as-delivered) but only up to a
// fixed attempt budget, after which the row settles with a terminal log
// line -- never an unbounded hot loop and never an attempt count that grows
// without settling.
func TestEventBus_PanickingRemoteHandler_BoundedRetryThenLoggedTerminal(t *testing.T) {
	ctx := context.Background()
	pool := startPostgresPool(t, ctx)

	const eventType = "invoice.panicked"
	var attempts atomic.Int64
	publisher := eventbuspostgres.NewEventBus(pool, "panic-budget-publisher")
	subscriber := eventbuspostgres.NewEventBus(pool, "panic-budget-subscriber")
	t.Cleanup(subscriber.Close)
	t.Cleanup(publisher.Close)

	panicOn(t, subscriber, eventType, 100, &attempts)
	spy := &eventSpy{}
	subscriber.Subscribe(eventType, spy.handler())
	warmUp(t, ctx, publisher, eventType, spy)

	// Capture every panic and terminal log line this test's own row
	// produces (slog.Default is what runHandlerRecovered and the budget
	// exhaustion path log through; warm-up rows never panic, so nothing
	// before the publish below is captured).
	captured := &logCapture{}
	previous := slog.Default()
	slog.SetDefault(slog.New(captured))
	defer slog.SetDefault(previous)

	if err := publisher.Publish(ctx, pkgcore.Event{
		Type:     eventType,
		TenantID: pkgcore.TenantID("tenant-panic"),
		Payload:  map[string]any{"sequence": float64(100)},
	}); err != nil {
		t.Fatalf("Publish(100) error = %v, want nil", err)
	}

	// Retried (>= 2 attempts), then settled: the count must stay put
	// through two full idle wake cycles.
	settled := assertEventuallyFlat(t, "the panicking handler's attempt count", 2, attempts.Load)
	if settled > 6 {
		t.Errorf("panicking handler ran %d times, want a bounded budget (at most a handful of retries)", settled)
	}

	// The settlement is honest, never silent: the row's last retry round
	// exhausts the budget and logs a terminal line naming the failure.
	if n := captured.countMatching(func(r slog.Record) bool {
		return strings.Contains(r.Message, "exhausted its retry budget")
	}); n < 1 {
		t.Errorf("no terminal log line after the retry budget was exhausted, want at least 1: the row must settle honestly, never silently")
	}
	// Every invocation was logged (each panic leaves a trace), and the
	// terminal line was logged for exactly this test's row type.
	panicLogs := captured.countMatching(func(r slog.Record) bool {
		return strings.Contains(r.Message, "remote handler panicked")
	})
	if panicLogs != int(attempts.Load()) {
		t.Errorf("captured %d panic log lines for %d handler invocations: every panic must be logged", panicLogs, attempts.Load())
	}
}

// TestEventBus_NoPanickingHandler_ControlDeliveryExactlyOnce is the control
// shape for the suite: without a panicking handler the same publish
// sequence must be delivered to the healthy handler exactly once.
func TestEventBus_NoPanickingHandler_ControlDeliveryExactlyOnce(t *testing.T) {
	ctx := context.Background()
	pool := startPostgresPool(t, ctx)

	const eventType = "invoice.control"
	publisher := eventbuspostgres.NewEventBus(pool, "panic-control-publisher")
	subscriber := eventbuspostgres.NewEventBus(pool, "panic-control-subscriber")
	t.Cleanup(subscriber.Close)
	t.Cleanup(publisher.Close)

	spy := &eventSpy{}
	subscriber.Subscribe(eventType, spy.handler())
	warmUp(t, ctx, publisher, eventType, spy)

	for _, seq := range []float64{100, 200, 201} {
		if err := publisher.Publish(ctx, pkgcore.Event{
			Type:     eventType,
			TenantID: pkgcore.TenantID("tenant-control"),
			Payload:  map[string]any{"sequence": seq},
		}); err != nil {
			t.Fatalf("Publish(%v) error = %v, want nil", seq, err)
		}
	}
	testkit.Eventually(t, "the healthy handler to receive every published row", func() bool {
		return spyCountSeq(spy, 100) >= 1 && spyCountSeq(spy, 200) >= 1 && spyCountSeq(spy, 201) >= 1
	})
	time.Sleep(flatWindow)
	for _, seq := range []float64{100, 200, 201} {
		if got := spyCountSeq(spy, seq); got != 1 {
			t.Errorf("handler received sequence %v %d times, want exactly 1", seq, got)
		}
	}
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
