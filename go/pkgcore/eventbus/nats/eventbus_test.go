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
	metaErr      error // when set, Metadata fails with it

	acked      bool
	terminated bool
	nakCount   int
	nakDelay   time.Duration
}

func (m *fakeJetStreamMsg) Metadata() (*jetstream.MsgMetadata, error) {
	if m.metaErr != nil {
		return nil, m.metaErr
	}
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

// fakeJetStream is a scriptable jetstream.JetStream for the hermetic tests of
// Publish, the reader machinery and Close. It records every call the bus
// makes -- stream creations, consumer creations, publishes, deletions -- and
// answers one-shot scripted failures, so each flow is driven deterministically
// without a server. The methods the bus never calls come from the embedded
// nil interface.
type fakeJetStream struct {
	jetstream.JetStream

	mu sync.Mutex

	createStreamCalls   int
	createConsumerCalls int
	deleteConsumerCalls []string // "stream/name"
	deleteStreamCalls   []string
	published           []*nats.Msg

	createStreamErr    error               // one-shot: the next CreateOrUpdateStream fails
	createConsumerErr  error               // one-shot: the next CreateOrUpdateConsumer fails
	publishMsgFailures int                 // PublishMsg calls to fail before succeeding
	streamErrs         map[string]error    // per-stream failures for Stream()
	namesByStream      map[string][]string // consumer names each stream reports
	listerErrs         map[string]error    // per-stream ConsumerNames lister errors

	consumer   jetstream.Consumer
	consumeCtx jetstream.ConsumeContext
}

func newFakeJetStream() *fakeJetStream {
	f := &fakeJetStream{
		streamErrs:    make(map[string]error),
		namesByStream: make(map[string][]string),
		listerErrs:    make(map[string]error),
	}
	f.consumeCtx = &fakeConsumeContext{}
	f.consumer = &fakeConsumer{consumeCtx: f.consumeCtx}
	return f
}

func (f *fakeJetStream) CreateOrUpdateStream(ctx context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createStreamErr != nil {
		err := f.createStreamErr
		f.createStreamErr = nil
		return nil, err
	}
	f.createStreamCalls++
	return nil, nil
}

func (f *fakeJetStream) Stream(ctx context.Context, name string) (jetstream.Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.streamErrs[name]; ok {
		return nil, err
	}
	return &fakeStream{name: name, js: f}, nil
}

func (f *fakeJetStream) DeleteStream(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteStreamCalls = append(f.deleteStreamCalls, name)
	return nil
}

func (f *fakeJetStream) CreateOrUpdateConsumer(ctx context.Context, stream string, cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createConsumerErr != nil {
		err := f.createConsumerErr
		f.createConsumerErr = nil
		return nil, err
	}
	f.createConsumerCalls++
	return f.consumer, nil
}

func (f *fakeJetStream) DeleteConsumer(ctx context.Context, stream, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteConsumerCalls = append(f.deleteConsumerCalls, stream+"/"+name)
	return nil
}

func (f *fakeJetStream) PublishMsg(ctx context.Context, msg *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.publishMsgFailures > 0 {
		f.publishMsgFailures--
		return nil, errors.New("nats: stream deleted out from under the publisher")
	}
	f.published = append(f.published, msg)
	return &jetstream.PubAck{}, nil
}

// newTestBus builds an EventBus wired to js with a fixed instance id, ready
// for the deterministic tests below; the real constructor is exercised
// separately against a live-but-unconnected connection.
func newTestBus(js jetstream.JetStream) *EventBus {
	ctx, cancel := context.WithCancel(context.Background())
	return &EventBus{
		js:           js,
		instanceID:   "test-bus-instance",
		ctx:          ctx,
		cancel:       cancel,
		handlers:     make(map[string][]pkgcore.EventHandler),
		streamsReady: make(map[string]struct{}),
		readers:      make(map[string]*busReader),
	}
}

// fakeStream is a jetstream.Stream whose ConsumerNames answers from the fake
// JetStream's per-stream scripted names.
type fakeStream struct {
	jetstream.Stream
	name string
	js   *fakeJetStream
}

func (s *fakeStream) ConsumerNames(ctx context.Context) jetstream.ConsumerNameLister {
	s.js.mu.Lock()
	defer s.js.mu.Unlock()
	return &fakeConsumerNameLister{
		names: s.js.namesByStream[s.name],
		err:   s.js.listerErrs[s.name],
	}
}

// fakeConsumerNameLister emits its scripted names once and then reports its
// scripted error.
type fakeConsumerNameLister struct {
	names []string
	err   error
}

func (l *fakeConsumerNameLister) Name() <-chan string {
	ch := make(chan string, len(l.names))
	for _, n := range l.names {
		ch <- n
	}
	close(ch)
	return ch
}

func (l *fakeConsumerNameLister) Err() error { return l.err }

// fakeConsumer is a jetstream.Consumer whose Consume returns the fake
// JetStream's consume context.
type fakeConsumer struct {
	jetstream.Consumer
	consumeCtx jetstream.ConsumeContext
}

func (c *fakeConsumer) Consume(handler jetstream.MessageHandler, opts ...jetstream.PullConsumeOpt) (jetstream.ConsumeContext, error) {
	return c.consumeCtx, nil
}

// fakeConsumeContext is a jetstream.ConsumeContext standing in for a real
// one on reader-teardown paths: the bus calls Stop on a reader's consume
// context, which the embedded nil interface could not answer, so the
// concrete override keeps that call safe.
type fakeConsumeContext struct {
	jetstream.ConsumeContext
}

func (c *fakeConsumeContext) Stop() {}

// TestEventBus_Publish_FakeJetStream_StreamEnsuredOnceAndMessageStamped pins
// the publish half end to end over the fake: the first publish of a type
// creates its stream, steady-state publishes pay no further create call, and
// every message carries the payload JSON plus the instance and tenant headers
// remote replicas skip and reconstruct from.
func TestEventBus_Publish_FakeJetStream_StreamEnsuredOnceAndMessageStamped(t *testing.T) {
	js := newFakeJetStream()
	bus := newTestBus(js)

	evt := pkgcore.Event{Type: "authn.user.created", TenantID: "tenant-7", Payload: "payload-body"}
	if err := bus.Publish(context.Background(), evt); err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}
	if err := bus.Publish(context.Background(), evt); err != nil {
		t.Fatalf("second Publish() error = %v, want nil", err)
	}

	js.mu.Lock()
	defer js.mu.Unlock()
	if js.createStreamCalls != 1 {
		t.Errorf("CreateOrUpdateStream calls = %d, want exactly 1: the second publish must hit the memoized stream", js.createStreamCalls)
	}
	if len(js.published) != 2 {
		t.Fatalf("published messages = %d, want 2", len(js.published))
	}
	for i, msg := range js.published {
		if msg.Subject != eventSubject(evt.Type) {
			t.Errorf("message %d subject = %q, want %q", i, msg.Subject, eventSubject(evt.Type))
		}
		if got := msg.Header.Get(headerSrc); got != bus.instanceID {
			t.Errorf("message %d %s header = %q, want %q", i, headerSrc, got, bus.instanceID)
		}
		if got := msg.Header.Get(headerTenant); got != "tenant-7" {
			t.Errorf("message %d %s header = %q, want %q", i, headerTenant, got, "tenant-7")
		}
		if string(msg.Data) != `"payload-body"` {
			t.Errorf("message %d data = %s, want the JSON-encoded payload", i, msg.Data)
		}
	}
}

// TestEventBus_Publish_UnserializablePayloadFailsBeforeAnyStreamCall pins
// that a payload JSON cannot carry fails the publish before anything reaches
// the server -- no stream is created for an event that can never be
// delivered.
func TestEventBus_Publish_UnserializablePayloadFailsBeforeAnyStreamCall(t *testing.T) {
	js := newFakeJetStream()
	bus := newTestBus(js)

	evt := pkgcore.Event{Type: "some.event", Payload: make(chan int)}
	err := bus.Publish(context.Background(), evt)
	if err == nil || !strings.Contains(err.Error(), "not JSON-serializable") {
		t.Fatalf("Publish() error = %v, want a not-JSON-serializable error", err)
	}
	js.mu.Lock()
	defer js.mu.Unlock()
	if js.createStreamCalls != 0 {
		t.Errorf("CreateOrUpdateStream calls = %d, want 0 for a payload that can never be delivered", js.createStreamCalls)
	}
}

// TestEventBus_Publish_StreamCreationFailureIsReportedAndRecovered pins both
// halves of ensureStream's error surface: a failed create fails that publish
// with a wrapped error, and the failure is not memoized -- the next publish
// retries the create and succeeds.
func TestEventBus_Publish_StreamCreationFailureIsReportedAndRecovered(t *testing.T) {
	js := newFakeJetStream()
	js.createStreamErr = errors.New("nats: no responders")
	bus := newTestBus(js)

	evt := pkgcore.Event{Type: "some.event", Payload: "body"}
	err := bus.Publish(context.Background(), evt)
	if err == nil || !strings.Contains(err.Error(), "ensure stream for event") {
		t.Fatalf("Publish() error = %v, want a wrapped ensure-stream error", err)
	}
	if err := bus.Publish(context.Background(), evt); err != nil {
		t.Fatalf("Publish() after a failed stream create error = %v, want nil: the failure must not be memoized", err)
	}
}

// TestEventBus_Publish_LostStreamIsRecreatedAndRetriedOnce pins Publish's
// forget-and-recreate retry: a publish that fails because the memoized
// stream vanished (Close's cleanup on another instance) is retried exactly
// once after recreating the stream, and succeeds transparently.
func TestEventBus_Publish_LostStreamIsRecreatedAndRetriedOnce(t *testing.T) {
	js := newFakeJetStream()
	js.publishMsgFailures = 1
	bus := newTestBus(js)

	if err := bus.Publish(context.Background(), pkgcore.Event{Type: "some.event", Payload: "body"}); err != nil {
		t.Fatalf("Publish() with one lost stream error = %v, want nil (forget-and-recreate must transparently recover)", err)
	}
	js.mu.Lock()
	defer js.mu.Unlock()
	if js.createStreamCalls != 2 {
		t.Errorf("CreateOrUpdateStream calls = %d, want 2: the retry must recreate the stream", js.createStreamCalls)
	}
	if len(js.published) != 1 {
		t.Errorf("published messages = %d, want exactly 1 (the failed attempt must not be counted)", len(js.published))
	}
}

// TestEventBus_Publish_RetryAfterRecreateAlsoFailsIsReported pins the
// double-failure answer: when the retried publish fails too, the original
// error is reported.
func TestEventBus_Publish_RetryAfterRecreateAlsoFailsIsReported(t *testing.T) {
	js := newFakeJetStream()
	js.publishMsgFailures = 2
	bus := newTestBus(js)

	err := bus.Publish(context.Background(), pkgcore.Event{Type: "some.event", Payload: "body"})
	if err == nil || !strings.Contains(err.Error(), "publish event") {
		t.Fatalf("Publish() error = %v, want the second failure reported", err)
	}
}

// TestEventBus_Publish_LocalHandlersRunInRegistrationOrder pins the local
// half of a publish's delivery: subscribers of the type run synchronously on
// the publishing call, in registration order, and the call joins their
// errors.
func TestEventBus_Publish_LocalHandlersRunInRegistrationOrder(t *testing.T) {
	js := newFakeJetStream()
	bus := newTestBus(js)

	var order []string
	bus.Subscribe("some.event", func(_ context.Context, evt pkgcore.Event) error {
		order = append(order, "first")
		if evt.Payload != "body" {
			t.Errorf("first handler payload = %v, want the published payload", evt.Payload)
		}
		return errors.New("first handler failed")
	})
	bus.Subscribe("some.event", func(_ context.Context, _ pkgcore.Event) error {
		order = append(order, "second")
		return nil
	})
	bus.Subscribe("other.event", func(_ context.Context, _ pkgcore.Event) error {
		order = append(order, "wrong-type")
		return errors.New("must not run")
	})

	err := bus.Publish(context.Background(), pkgcore.Event{Type: "some.event", Payload: "body"})
	if err == nil || !strings.Contains(err.Error(), "handler 0 for event") {
		t.Fatalf("Publish() error = %v, want the failing handler named by index", err)
	}
	if strings.Join(order, ",") != "first,second" {
		t.Errorf("handler run order = %v, want exactly [first second]: handlers run in registration order and only for their own type", order)
	}
}

// TestEventBus_Publish_PanickingLocalHandlerIsContained pins runLocalHandler's
// containment: a local handler that panics neither unwinds the publishing
// caller nor fails the publish -- it is recovered, logged and dropped, and
// the handler's siblings still run.
func TestEventBus_Publish_PanickingLocalHandlerIsContained(t *testing.T) {
	js := newFakeJetStream()
	bus := newTestBus(js)

	captured := &slogCapture{}
	previous := slog.Default()
	slog.SetDefault(slog.New(captured))
	defer slog.SetDefault(previous)

	var siblings int
	bus.Subscribe("some.event", func(context.Context, pkgcore.Event) error {
		panic("local handler bug")
	})
	bus.Subscribe("some.event", func(context.Context, pkgcore.Event) error {
		siblings++
		return nil
	})

	if err := bus.Publish(context.Background(), pkgcore.Event{Type: "some.event", Payload: "body"}); err != nil {
		t.Fatalf("Publish() with a panicking local handler error = %v, want nil: the panic is contained", err)
	}
	if siblings != 1 {
		t.Errorf("sibling handler ran %d times, want 1", siblings)
	}
	if n := captured.countMatching(func(r slog.Record) bool {
		return strings.Contains(r.Message, "local handler panicked")
	}); n != 1 {
		t.Errorf("captured %d local-panic log lines, want exactly 1", n)
	}
}

// TestEventBus_DeliverRemote_OwnMessageAckedWithoutDispatch pins the
// src-header skip: a message this instance itself published is acknowledged
// and never dispatched, because the Publish call already ran the local
// handlers.
func TestEventBus_DeliverRemote_OwnMessageAckedWithoutDispatch(t *testing.T) {
	bus := newTestBus(nil)
	invocations := 0
	bus.handlers["some.event"] = []pkgcore.EventHandler{
		func(context.Context, pkgcore.Event) error { invocations++; return nil },
	}
	ledger := &panicRetryLedger{records: make(map[uint64]*panicRetryRecord)}

	msg := &fakeJetStreamMsg{
		headers:      nats.Header{headerSrc: {bus.instanceID}},
		data:         []byte(`"body"`),
		streamSeq:    1,
		numDelivered: 1,
	}
	bus.deliverRemote("some.event", msg, ledger)
	if !msg.acked || msg.naked() || msg.terminated {
		t.Errorf("own message: acked=%v nak=%v terminated=%v, want exactly an ack", msg.acked, msg.naked(), msg.terminated)
	}
	if invocations != 0 {
		t.Errorf("own message dispatched %d handlers, want 0: the publishing instance already ran them", invocations)
	}
}

// TestEventBus_DeliverRemote_UndecodableAndUnmetadatableMessagesAreAcked pins
// the two corrupt-message guards: a body that is not JSON and a message whose
// metadata cannot be read are acknowledged and dropped -- never wedging the
// reader, never delivered, never retried.
func TestEventBus_DeliverRemote_UndecodableAndUnmetadatableMessagesAreAcked(t *testing.T) {
	bus := newTestBus(nil)
	invocations := 0
	bus.handlers["some.event"] = []pkgcore.EventHandler{
		func(context.Context, pkgcore.Event) error { invocations++; return nil },
	}
	ledger := &panicRetryLedger{records: make(map[uint64]*panicRetryRecord)}
	headers := nats.Header{headerSrc: {"other-instance"}, headerTenant: {"tenant-1"}}

	undecodable := &fakeJetStreamMsg{headers: headers, data: []byte("{not json"), streamSeq: 1, numDelivered: 1}
	bus.deliverRemote("some.event", undecodable, ledger)
	if !undecodable.acked {
		t.Error("undecodable message was not acknowledged, want it dropped with an ack")
	}

	unmetadatable := &fakeJetStreamMsg{headers: headers, data: []byte(`"body"`), streamSeq: 1, numDelivered: 1, metaErr: errors.New("no metadata")}
	bus.deliverRemote("some.event", unmetadatable, ledger)
	if !unmetadatable.acked {
		t.Error("message without metadata was not acknowledged, want it dropped with an ack")
	}

	if invocations != 0 {
		t.Errorf("corrupt messages dispatched %d handlers, want 0", invocations)
	}
}

// TestEventBus_DeliverRemote_NoPanicAcksImmediately pins the happy remote
// path: handlers that all complete acknowledge the message on the spot with
// no redelivery asked for.
func TestEventBus_DeliverRemote_NoPanicAcksImmediately(t *testing.T) {
	bus := newTestBus(nil)
	invocations := 0
	bus.handlers["some.event"] = []pkgcore.EventHandler{
		func(_ context.Context, evt pkgcore.Event) error {
			invocations++
			if evt.TenantID != "tenant-1" {
				t.Errorf("remote handler tenant = %q, want the header's tenant", evt.TenantID)
			}
			return nil
		},
	}
	ledger := &panicRetryLedger{records: make(map[uint64]*panicRetryRecord)}

	msg := &fakeJetStreamMsg{
		headers:      nats.Header{headerSrc: {"other-instance"}, headerTenant: {"tenant-1"}},
		data:         []byte(`{"n":1}`),
		streamSeq:    3,
		numDelivered: 1,
	}
	bus.deliverRemote("some.event", msg, ledger)
	if !msg.acked || msg.naked() || msg.terminated {
		t.Errorf("clean delivery: acked=%v nak=%v terminated=%v, want exactly an ack", msg.acked, msg.naked(), msg.terminated)
	}
	if invocations != 1 {
		t.Errorf("handler ran %d times, want 1", invocations)
	}
	if len(ledger.records) != 0 {
		t.Errorf("ledger holds %d records after a clean delivery, want none", len(ledger.records))
	}
}

// TestEventBus_DeliverRemote_FirstDeliveryAlreadyPastBudgetSettles pins the
// first-seen-past-budget guard: a message whose ledger record was lost with
// an earlier consumer incarnation but which kept being redelivered must not
// re-enter the budget from scratch -- it settles on first sight.
func TestEventBus_DeliverRemote_FirstDeliveryAlreadyPastBudgetSettles(t *testing.T) {
	bus := newTestBus(nil)
	bus.handlers["some.event"] = []pkgcore.EventHandler{
		func(context.Context, pkgcore.Event) error { panic("still panicking") },
	}
	ledger := &panicRetryLedger{records: make(map[uint64]*panicRetryRecord)}

	captured := &slogCapture{}
	previous := slog.Default()
	slog.SetDefault(slog.New(captured))
	defer slog.SetDefault(previous)

	msg := &fakeJetStreamMsg{
		headers:      nats.Header{headerSrc: {"other-instance"}},
		data:         []byte(`{}`),
		streamSeq:    9,
		numDelivered: eventMaxDeliver,
	}
	bus.deliverRemote("some.event", msg, ledger)
	if !msg.terminated || msg.acked || msg.naked() {
		t.Errorf("past-budget first delivery: terminated=%v acked=%v nak=%v, want a Term settlement", msg.terminated, msg.acked, msg.naked())
	}
	if n := captured.countMatching(func(r slog.Record) bool {
		return strings.Contains(r.Message, "exhausted its redelivery budget")
	}); n != 1 {
		t.Errorf("captured %d terminal log lines, want exactly 1", n)
	}
}

// TestEventBus_SubscribeCloseFlow_FakeJetStream drives the reader machinery
// end to end over the fake: Subscribe starts one reader that creates the
// stream and this instance's durable consumer and begins consuming; Close
// stops the reader, deletes the consumer and -- with no other consumer left
// on the stream -- the stream itself; and a publish after Close is refused
// with ErrEventBusClosed.
func TestEventBus_SubscribeCloseFlow_FakeJetStream(t *testing.T) {
	js := newFakeJetStream()
	bus := newTestBus(js)
	eventType := "some.event"

	var invocations int
	bus.Subscribe(eventType, func(context.Context, pkgcore.Event) error { invocations++; return nil })
	bus.Subscribe(eventType, nil) // nil handler ignored, no second reader

	// Wait for the reader goroutine to finish provisioning and start
	// consuming.
	deadline := time.Now().Add(2 * time.Second)
	for {
		bus.readersMu.Lock()
		r := bus.readers[eventType]
		started := r != nil && func() bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.consumeCtx != nil
		}()
		bus.readersMu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reader never started consuming against the fake")
		}
		time.Sleep(time.Millisecond)
	}

	js.mu.Lock()
	streamCalls := js.createStreamCalls
	consumerCalls := js.createConsumerCalls
	js.mu.Unlock()
	if streamCalls != 1 || consumerCalls != 1 {
		t.Fatalf("reader provisioning: stream/consumer creates = %d/%d, want 1/1", streamCalls, consumerCalls)
	}

	bus.Close()

	if err := bus.Publish(context.Background(), pkgcore.Event{Type: eventType, Payload: "x"}); !errors.Is(err, ErrEventBusClosed) {
		t.Errorf("Publish after Close error = %v, want ErrEventBusClosed", err)
	}

	consumerName := busConsumerName(bus.instanceID)
	streamName := streamNameForEventType(eventType)
	js.mu.Lock()
	defer js.mu.Unlock()
	if len(js.deleteConsumerCalls) != 1 || js.deleteConsumerCalls[0] != streamName+"/"+consumerName {
		t.Errorf("consumer deletions = %v, want exactly [%q]", js.deleteConsumerCalls, streamName+"/"+consumerName)
	}
	if len(js.deleteStreamCalls) != 1 || js.deleteStreamCalls[0] != streamName {
		t.Errorf("stream deletions = %v, want exactly [%q]: the last consumer's Close removes its stream", js.deleteStreamCalls, streamName)
	}
}

// TestEventBus_Close_IsIdempotentAndStopsTheReader pins Close's own contract:
// a second Close deletes nothing more, and the reader's consume context is
// stopped exactly once per incarnation.
func TestEventBus_Close_IsIdempotentAndStopsTheReader(t *testing.T) {
	js := newFakeJetStream()
	bus := newTestBus(js)
	eventType := "some.event"

	bus.Subscribe(eventType, func(context.Context, pkgcore.Event) error { return nil })
	deadline := time.Now().Add(2 * time.Second)
	for {
		bus.readersMu.Lock()
		r := bus.readers[eventType]
		started := r != nil && func() bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.consumeCtx != nil
		}()
		bus.readersMu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reader never started consuming against the fake")
		}
		time.Sleep(time.Millisecond)
	}

	bus.Close()
	bus.Close() // idempotent: the once guard swallows the second call

	js.mu.Lock()
	deletions := len(js.deleteConsumerCalls) + len(js.deleteStreamCalls)
	js.mu.Unlock()
	if deletions != 2 {
		t.Errorf("second Close deleted %d more server-side objects, want 0: Close must be idempotent", deletions-2)
	}
}

// TestDestroyConsumers_DeterministicShapes drives destroyConsumers' decision
// surface directly: a reader with no active consume context is still cleaned
// up server-side, a stream with other consumers left on it is not deleted,
// and a stream whose consumer count cannot be read is not deleted either.
func TestDestroyConsumers_DeterministicShapes(t *testing.T) {
	newBus := func() *EventBus {
		ctx, cancel := context.WithCancel(context.Background())
		return &EventBus{
			js:           newFakeJetStream(),
			instanceID:   "id",
			ctx:          ctx,
			cancel:       cancel,
			handlers:     make(map[string][]pkgcore.EventHandler),
			streamsReady: make(map[string]struct{}),
			readers:      make(map[string]*busReader),
		}
	}

	t.Run("empty reader set cleans nothing", func(t *testing.T) {
		bus := newBus()
		js := bus.js.(*fakeJetStream)
		bus.destroyConsumers()
		js.mu.Lock()
		defer js.mu.Unlock()
		if len(js.deleteConsumerCalls) != 0 || len(js.deleteStreamCalls) != 0 {
			t.Error("destroyConsumers with no readers deleted something")
		}
	})

	t.Run("reader without a consume context still cleaned up", func(t *testing.T) {
		bus := newBus()
		js := bus.js.(*fakeJetStream)
		bus.readers["t1"] = &busReader{}
		bus.readers["t2"] = &busReader{consumeCtx: &fakeConsumeContext{}}
		bus.destroyConsumers()

		consumerName := busConsumerName("id")
		js.mu.Lock()
		defer js.mu.Unlock()
		if len(js.deleteConsumerCalls) != 2 {
			t.Fatalf("consumer deletions = %v, want both readers' consumers deleted", js.deleteConsumerCalls)
		}
		if len(js.deleteStreamCalls) != 2 {
			t.Errorf("stream deletions = %v, want both streams deleted once their only consumer is gone", js.deleteStreamCalls)
		}
		_ = consumerName
	})

	t.Run("stream with another consumer left is not deleted", func(t *testing.T) {
		bus := newBus()
		js := bus.js.(*fakeJetStream)
		js.namesByStream[streamNameForEventType("t1")] = []string{"other-instance-consumer"}
		bus.readers["t1"] = &busReader{consumeCtx: &fakeConsumeContext{}}
		bus.destroyConsumers()

		js.mu.Lock()
		defer js.mu.Unlock()
		if len(js.deleteConsumerCalls) != 1 {
			t.Errorf("consumer deletions = %v, want the instance's own consumer deleted", js.deleteConsumerCalls)
		}
		if len(js.deleteStreamCalls) != 0 {
			t.Errorf("stream deletions = %v, want none: another instance's consumer still needs the stream", js.deleteStreamCalls)
		}
	})

	t.Run("stream whose consumer count cannot be read is not deleted", func(t *testing.T) {
		bus := newBus()
		js := bus.js.(*fakeJetStream)
		js.streamErrs[streamNameForEventType("t1")] = errors.New("nats: stream not found")
		bus.readers["t1"] = &busReader{consumeCtx: &fakeConsumeContext{}}
		bus.destroyConsumers()

		js.mu.Lock()
		defer js.mu.Unlock()
		if len(js.deleteStreamCalls) != 0 {
			t.Errorf("stream deletions = %v, want none: the cleanup cannot know it was the last consumer", js.deleteStreamCalls)
		}
	})

	t.Run("consumer-name listing error skips the stream deletion", func(t *testing.T) {
		bus := newBus()
		js := bus.js.(*fakeJetStream)
		js.listerErrs[streamNameForEventType("t1")] = errors.New("nats: listing failed")
		bus.readers["t1"] = &busReader{consumeCtx: &fakeConsumeContext{}}
		bus.destroyConsumers()

		js.mu.Lock()
		defer js.mu.Unlock()
		if len(js.deleteStreamCalls) != 0 {
			t.Errorf("stream deletions = %v, want none when the listing errored", js.deleteStreamCalls)
		}
	})
}

// TestCountConsumers pins countConsumers' two outcomes: a stream that cannot
// be read errors, and the count is whatever the stream's consumer listing
// reports.
func TestCountConsumers(t *testing.T) {
	ctx := context.Background()

	js := newFakeJetStream()
	js.namesByStream["s1"] = []string{"a", "b"}
	n, err := countConsumers(ctx, js, "s1")
	if err != nil || n != 2 {
		t.Fatalf("countConsumers(s1) = (%d, %v), want (2, nil)", n, err)
	}

	js.streamErrs["s2"] = errors.New("nats: stream not found")
	if _, err := countConsumers(ctx, js, "s2"); err == nil {
		t.Fatal("countConsumers on a missing stream error = nil, want the stream error")
	}
}

// TestBus_SubscribeAfterCloseIsANoop pins Subscribe's post-Close contract:
// registering after Close registers nothing.
func TestBus_SubscribeAfterCloseIsANoop(t *testing.T) {
	js := newFakeJetStream()
	bus := newTestBus(js)
	bus.Close()

	bus.Subscribe("some.event", func(context.Context, pkgcore.Event) error { return nil })
	if got := bus.handlersFor("some.event"); got != nil {
		t.Errorf("handlers registered after Close = %v, want none", got)
	}
	js.mu.Lock()
	defer js.mu.Unlock()
	if js.createConsumerCalls != 0 {
		t.Errorf("a Subscribe after Close created %d consumers, want 0", js.createConsumerCalls)
	}
}

// TestEventBus_ReaderRetriesProvisioningFailuresUntilClose pins the reader's
// provisioning retry loop: a stream creation that fails and a consumer
// creation that fails are each retried in the background until they succeed
// or the bus closes -- Subscribe itself never fails -- and a Close landing
// mid-retry stops the loop cleanly.
func TestEventBus_ReaderRetriesProvisioningFailuresUntilClose(t *testing.T) {
	js := newFakeJetStream()
	js.createStreamErr = errors.New("nats: no responders")
	js.createConsumerErr = errors.New("nats: consumer create failed")
	bus := newTestBus(js)
	eventType := "some.event"

	bus.Subscribe(eventType, func(context.Context, pkgcore.Event) error { return nil })

	// The one-shot failures are consumed on the first attempt; the loop must
	// come back and provision successfully on a later attempt.
	deadline := time.Now().Add(3 * time.Second)
	for {
		js.mu.Lock()
		started := js.createConsumerCalls >= 1
		js.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reader never retried past the scripted provisioning failures")
		}
		time.Sleep(10 * time.Millisecond)
	}
	js.mu.Lock()
	streamCalls := js.createStreamCalls
	js.mu.Unlock()
	if streamCalls != 1 {
		t.Errorf("stream creations = %d, want exactly 1: the failed attempt is not memoized, and the retry created the stream once", streamCalls)
	}
	bus.Close()

	// A reader whose provisioning keeps failing must stop cleanly on Close:
	// the retry loop notices the closed context and exits without ever
	// starting a consumer.
	js2 := newFakeJetStream()
	js2.createConsumerErr = errors.New("nats: consumer create failed")
	bus2 := newTestBus(js2)
	bus2.Subscribe("other.event", func(context.Context, pkgcore.Event) error { return nil })
	time.Sleep(250 * time.Millisecond) // let the loop reach its retry sleep
	bus2.Close()
	deadline = time.Now().Add(2 * time.Second)
	for {
		bus2.readersMu.Lock()
		r := bus2.readers["other.event"]
		stopped := r != nil && func() bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.consumeCtx != nil
		}()
		bus2.readersMu.Unlock()
		if stopped {
			// Whatever the race outcome -- the consumer started and was
			// stopped, or the loop exited at the retry -- the reader must
			// have settled.
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reader never settled after Close")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
