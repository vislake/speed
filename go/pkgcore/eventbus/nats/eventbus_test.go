package nats

// Hermetic unit tests for the NATS-backed EventBus: everything here runs
// without a real NATS server, mirroring eventbus/redis's own eventbus_test.go
// split -- what belongs here is local to the bus itself, what needs a real
// broker lives in the integration tier (integration_test/eventbus_test.go).

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/vislake/speed/go/pkgcore"
)

// TestNewEventBus_PanicsOnNilConn pins that a nil connection is a wiring
// error reported at construction, not a failure deferred to the first
// publish -- the direct analogue of eventbus/redis's identical nil-client
// pin.
func TestNewEventBus_PanicsOnNilConn(t *testing.T) {
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
// any JetStream call, mirroring pkgcore's memory bus and eventbus/redis's
// identical pin. The connection dials a closed port with
// RetryOnFailedConnect so construction itself never blocks or fails.
func TestEventBus_PublishOnCancelledContext(t *testing.T) {
	t.Parallel()

	nc, err := nats.Connect("127.0.0.1:1", nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1))
	if err != nil {
		t.Fatalf("nats.Connect() error = %v, want nil (RetryOnFailedConnect must not fail synchronously)", err)
	}
	t.Cleanup(nc.Close)
	bus := NewEventBus(nc)
	t.Cleanup(bus.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = bus.Publish(ctx, pkgcore.Event{Type: "some.event", Payload: "payload"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish on a cancelled context error = %v, want context.Canceled", err)
	}
}

// TestStreamNameForEventType pins the "." (and other invalid-rune) folding
// rule the package doc comment documents, including the many-to-one
// collision it owns up to.
func TestStreamNameForEventType(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"authn.user.created": "PKGCORE_EVENTS_authn_user_created",
		"a.b":                "PKGCORE_EVENTS_a_b",
		"a_b":                "PKGCORE_EVENTS_a_b", // deliberate collision with "a.b" above
		"plain":              "PKGCORE_EVENTS_plain",
	}
	for eventType, want := range tests {
		if got := streamNameForEventType(eventType); got != want {
			t.Errorf("streamNameForEventType(%q) = %q, want %q", eventType, got, want)
		}
	}
}

// TestBusConsumerName pins the durable consumer name shape every bus
// instance creates on every stream it subscribes to.
func TestBusConsumerName(t *testing.T) {
	t.Parallel()

	if got, want := busConsumerName("abc123"), "pkgcore-bus-abc123"; got != want {
		t.Errorf("busConsumerName(%q) = %q, want %q", "abc123", got, want)
	}
}

// TestNewBusInstanceID_ReturnsDistinctIDs pins that two instances never
// collide on the identifier that keeps their consumers -- and therefore
// their fan-out delivery -- independent of one another.
func TestNewBusInstanceID_ReturnsDistinctIDs(t *testing.T) {
	t.Parallel()

	a, b := newBusInstanceID(), newBusInstanceID()
	if a == "" {
		t.Fatal("newBusInstanceID() returned an empty string")
	}
	if a == b {
		t.Errorf("newBusInstanceID() returned %q twice, want two distinct instance ids", a)
	}
}

// TestDeliverRemote_PanickingHandler_BoundedPerHandlerRedelivery drives
// deliverRemote's redelivery state machine for one message whose delivery
// panics, without a server: the first delivery runs the whole fan-out and
// negatively acknowledges the message; every redelivery re-invokes ONLY the
// panicked handler value (the healthy sibling is never re-run); and the
// still-panicking handler exhausts the eventMaxDeliver budget on the last
// delivery, which settles the message with a terminal log line and a Term --
// never an unbounded redelivery loop. This is the deterministic,
// broker-free half of the bounded-redelivery regressions; the integration
// tier re-proves the same shape against a real NATS server.
func TestDeliverRemote_PanickingHandler_BoundedPerHandlerRedelivery(t *testing.T) {
	const eventType = "invoice.panicked"
	panickedInvocations, healthyInvocations := 0, 0
	bus := &EventBus{
		instanceID: "bus-under-test",
		ctx:        context.Background(),
		cancel:     func() {},
		handlers: map[string][]pkgcore.EventHandler{
			eventType: {
				func(context.Context, pkgcore.Event) error {
					panickedInvocations++
					panic("remote handler bug")
				},
				func(context.Context, pkgcore.Event) error {
					healthyInvocations++
					return nil
				},
			},
		},
	}
	ledger := &panicRetryLedger{records: make(map[uint64]*panicRetryRecord)}

	// Capture the panic and terminal log lines deliverRemote produces.
	captured := &slogCapture{}
	previous := slog.Default()
	slog.SetDefault(slog.New(captured))
	defer slog.SetDefault(previous)

	deliver := func(numDelivered uint64) *fakeJetStreamMsg {
		msg := &fakeJetStreamMsg{
			headers:      nats.Header{headerSrc: {"other-instance"}},
			data:         []byte(`{"sequence":1}`),
			streamSeq:    7,
			numDelivered: numDelivered,
		}
		bus.deliverRemote(eventType, msg, ledger)
		return msg
	}

	// First delivery: the whole fan-out runs, the message is not acked as
	// delivered (a handler whose side effects never ran was not delivered),
	// and a redelivery is asked for at panicRedeliveryDelay.
	first := deliver(1)
	if panickedInvocations != 1 || healthyInvocations != 1 {
		t.Fatalf("first delivery ran panicking/healthy handler %d/%d times, want 1/1", panickedInvocations, healthyInvocations)
	}
	if first.nakDelay != panicRedeliveryDelay || first.nakCount != 1 {
		t.Errorf("first delivery: nak count/delay = %d/%v, want 1/%v", first.nakCount, first.nakDelay, panicRedeliveryDelay)
	}
	if first.acked || first.terminated {
		t.Errorf("first delivery: acked=%v terminated=%v, want neither (not acked as delivered, budget not yet spent)", first.acked, first.terminated)
	}

	// Redeliveries two through eventMaxDeliver-1: only the panicked handler
	// value is re-invoked -- the healthy sibling must not re-run -- and the
	// budget is not yet spent, so each delivery asks for one more.
	for numDelivered := uint64(2); numDelivered < eventMaxDeliver; numDelivered++ {
		msg := deliver(numDelivered)
		if panickedInvocations != int(numDelivered) {
			t.Fatalf("after %d deliveries the panicking handler ran %d times, want %d", numDelivered, panickedInvocations, numDelivered)
		}
		if healthyInvocations != 1 {
			t.Fatalf("after %d deliveries the healthy sibling ran %d times, want exactly 1: a panicking handler must not re-run its siblings", numDelivered, healthyInvocations)
		}
		if msg.nakCount != 1 || msg.acked || msg.terminated {
			t.Fatalf("delivery %d: nakCount=%d acked=%v terminated=%v, want one nak and no settlement yet", numDelivered, msg.nakCount, msg.acked, msg.terminated)
		}
	}

	// The eventMaxDeliver-th delivery is the budget's last: the still-
	// panicking handler settles the message with a logged terminal and a
	// Term, and the ledger record is gone.
	last := deliver(eventMaxDeliver)
	if panickedInvocations != eventMaxDeliver {
		t.Errorf("panicking handler ran %d times, want exactly the %d-attempt budget", panickedInvocations, eventMaxDeliver)
	}
	if healthyInvocations != 1 {
		t.Errorf("healthy sibling ran %d times, want exactly 1", healthyInvocations)
	}
	if !last.terminated || last.acked {
		t.Errorf("final delivery: terminated=%v acked=%v, want a Term settlement, never an ack", last.terminated, last.acked)
	}
	if len(ledger.records) != 0 {
		t.Errorf("ledger still holds %d records after the budget was exhausted, want none", len(ledger.records))
	}
	if n := captured.countMatching(func(r slog.Record) bool {
		return strings.Contains(r.Message, "exhausted its redelivery budget")
	}); n != 1 {
		t.Errorf("captured %d terminal log lines, want exactly 1", n)
	}
	if n := captured.countMatching(func(r slog.Record) bool {
		return strings.Contains(r.Message, "remote handler panicked")
	}); n != eventMaxDeliver {
		t.Errorf("captured %d panic log lines for %d panicking invocations, want one per invocation", n, eventMaxDeliver)
	}
}

// TestDeliverRemote_PanickingHandler_ResolvedOnRetryAcks pins the resolve-
// mid-budget shape: a handler whose panic was transient (it succeeds on the
// first redelivery) completes the message's delivery -- acknowledged, ledger
// record dropped, no further redelivery asked for -- without the healthy
// sibling ever re-running.
func TestDeliverRemote_PanickingHandler_ResolvedOnRetryAcks(t *testing.T) {
	const eventType = "invoice.panicked"
	invocations, healthyInvocations := 0, 0
	bus := &EventBus{
		instanceID: "bus-under-test",
		ctx:        context.Background(),
		cancel:     func() {},
		handlers: map[string][]pkgcore.EventHandler{
			eventType: {
				func(context.Context, pkgcore.Event) error {
					invocations++
					if invocations == 1 {
						panic("transient remote handler bug")
					}
					return nil
				},
				func(context.Context, pkgcore.Event) error {
					healthyInvocations++
					return nil
				},
			},
		},
	}
	ledger := &panicRetryLedger{records: make(map[uint64]*panicRetryRecord)}

	newMsg := func(numDelivered uint64) *fakeJetStreamMsg {
		return &fakeJetStreamMsg{
			headers:      nats.Header{headerSrc: {"other-instance"}},
			data:         []byte(`{"sequence":1}`),
			streamSeq:    7,
			numDelivered: numDelivered,
		}
	}

	first := newMsg(1)
	bus.deliverRemote(eventType, first, ledger)
	if !first.naked() {
		t.Fatalf("first delivery was not negatively acknowledged, want a redelivery for the panicked handler")
	}
	if len(ledger.records) != 1 {
		t.Fatalf("ledger holds %d records after the first delivery, want 1", len(ledger.records))
	}

	second := newMsg(2)
	bus.deliverRemote(eventType, second, ledger)
	if invocations != 2 || healthyInvocations != 1 {
		t.Fatalf("after the redelivery the handler ran %d times and the healthy sibling %d times, want 2/1 (only the panicked value is retried)", invocations, healthyInvocations)
	}
	if !second.acked || second.terminated || second.naked() {
		t.Errorf("resolving redelivery: acked=%v terminated=%v nak=%v, want an ack and no further redelivery", second.acked, second.terminated, second.naked())
	}
	if len(ledger.records) != 0 {
		t.Errorf("ledger still holds %d records after the handler resolved, want none", len(ledger.records))
	}
}

// fakeJetStreamMsg is a scriptable jetstream.Msg for the hermetic
// deliverRemote tests: it answers Metadata from fields the test controls and
// records every ack settlement it receives.
type fakeJetStreamMsg struct {
	headers      nats.Header
	data         []byte
	streamSeq    uint64
	numDelivered uint64

	acked      bool
	terminated bool
	nakCount   int
	nakDelay   time.Duration
}

func (m *fakeJetStreamMsg) Metadata() (*jetstream.MsgMetadata, error) {
	return &jetstream.MsgMetadata{
		Sequence:     jetstream.SequencePair{Stream: m.streamSeq},
		NumDelivered: m.numDelivered,
	}, nil
}
func (m *fakeJetStreamMsg) Data() []byte                    { return m.data }
func (m *fakeJetStreamMsg) Headers() nats.Header            { return m.headers }
func (m *fakeJetStreamMsg) Subject() string                 { return "pkgcore.events.test" }
func (m *fakeJetStreamMsg) Reply() string                   { return "" }
func (m *fakeJetStreamMsg) Ack() error                      { m.acked = true; return nil }
func (m *fakeJetStreamMsg) DoubleAck(context.Context) error { m.acked = true; return nil }
func (m *fakeJetStreamMsg) Nak() error                      { m.nakCount++; return nil }
func (m *fakeJetStreamMsg) NakWithDelay(d time.Duration) error {
	m.nakCount++
	m.nakDelay = d
	return nil
}
func (m *fakeJetStreamMsg) InProgress() error           { return nil }
func (m *fakeJetStreamMsg) Term() error                 { m.terminated = true; return nil }
func (m *fakeJetStreamMsg) TermWithReason(string) error { m.terminated = true; return nil }

func (m *fakeJetStreamMsg) naked() bool { return m.nakCount > 0 }

// slogCapture is a slog.Handler that records every record it handles for the
// panic/terminal log assertions above.
type slogCapture struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (c *slogCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *slogCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, r.Clone())
	return nil
}

func (c *slogCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *slogCapture) WithGroup(string) slog.Handler      { return c }

func (c *slogCapture) countMatching(match func(slog.Record) bool) int {
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
