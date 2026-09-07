//go:build integration

package nats_test

// Integration tests for eventbusnats.NewEventBus: mirroring
// eventbus/redis/integration_test/eventbus_test.go's own coverage almost
// test-for-test, adapted to JetStream. Two bus instances -- each built on
// its own independent *nats.Conn, standing in for two replicas of a
// deployment sharing one JetStream-enabled NATS server -- pin the delivery
// contract the implementation documents: handlers on the publishing
// instance run synchronously with the original payload, handlers on every
// other instance eventually run exactly once with the JSON-reconstructed
// payload shape, events never cross type streams, a publish that cannot
// commit delivers nothing anywhere, Close really stops a replica's delivery
// and destroys the durable consumer its instance created (sparing a
// peer's), and a reader whose consumer is removed out from under it
// recreates it instead of wedging.
//
// TestEventBus_FanOutDeliversToEveryReplica is the section of this file that
// answers this round's central design question directly: does JetStream
// deliver fan-out (every replica's own handler runs) or load-balanced (one
// handler, chosen from a shared group, wins)? It proves fan-out over two
// wholly independent client connections, matching eventbus/redis's own
// documented and tested behaviour.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/vislake/speed/go/pkgcore"
	eventbusnats "github.com/vislake/speed/go/pkgcore/eventbus/nats"
	"github.com/vislake/speed/go/pkgcore/eventbustest"
)

// invoicePaid is the concrete payload type used across these tests: a plain
// struct, so the local side of a publish can assert the original type while
// the remote side sees the JSON shape it decodes to.
type invoicePaid struct {
	ID     string  `json:"id"`
	Amount float64 `json:"amount"`
}

// eventRecorder accumulates what one bus instance's handlers saw, for later
// count and content assertions. Handlers run on different goroutines -- the
// local publish path and this type's own JetStream consumer both invoke
// them -- so every access goes through the mutex.
type eventRecorder struct {
	mu   sync.Mutex
	evts []pkgcore.Event
}

func (r *eventRecorder) handler() func(context.Context, pkgcore.Event) error {
	return func(_ context.Context, evt pkgcore.Event) error {
		r.mu.Lock()
		r.evts = append(r.evts, evt)
		r.mu.Unlock()
		return nil
	}
}

func (r *eventRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.evts)
}

func (r *eventRecorder) countByTenant(tenant pkgcore.TenantID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, evt := range r.evts {
		if evt.TenantID == tenant {
			n++
		}
	}
	return n
}

func (r *eventRecorder) at(i int) pkgcore.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.evts[i]
}

func (r *eventRecorder) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evts = nil
}

// eventually polls cond until it holds or the deadline passes, failing the
// test in the latter case. Cross-process delivery is asynchronous: a
// consumer's Consume dispatch fires as soon as JetStream has a message for
// it, so remote delivery of an already-committed event lands well inside
// this five-second window.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// warmUp proves that receiver's durable consumer on the eventType stream
// exists and is actually consuming, by publishing marker events from
// publisher until one of them reaches receiver. Subscribe starts the reader
// asynchronously -- the stream and consumer are created on a background
// goroutine, retrying until the server answers -- so a test that publishes a
// real event immediately after Subscribe could race that creation and lose
// it. Once receiver reports a marker, a short extra wait lets any other
// marker already in flight drain out, so clearing both recorders afterwards
// leaves counts that only the test's own events move.
func warmUp(t *testing.T, publisher *eventbusnats.EventBus, receiver *eventRecorder, eventType string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for seq := 1; ; seq++ {
		if err := publisher.Publish(context.Background(), pkgcore.Event{
			Type:     eventType,
			TenantID: pkgcore.TenantID("warmup"),
			Payload:  map[string]any{"seq": seq},
		}); err != nil {
			t.Fatalf("warm-up publish %d: %v", seq, err)
		}
		for waited := 0; receiver.count() == 0 && waited < 500; waited += 25 {
			time.Sleep(25 * time.Millisecond)
		}
		if receiver.count() > 0 {
			time.Sleep(300 * time.Millisecond) // drain any stragglers already in flight
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("warm-up: no marker event reached the receiver within 10s")
		}
	}
}

// requireRemoteInvoice asserts that a remotely received event carries the
// tenant and the JSON-reconstructed shape of an invoicePaid -- a
// map[string]any, never the original struct, which cannot cross a process
// boundary.
func requireRemoteInvoice(t *testing.T, evt pkgcore.Event, wantID string, wantAmount float64, wantTenant pkgcore.TenantID) {
	t.Helper()
	if evt.TenantID != wantTenant {
		t.Errorf("remote event tenant = %q, want %q", evt.TenantID, wantTenant)
	}
	payload, ok := evt.Payload.(map[string]any)
	if !ok {
		t.Fatalf("remote event payload is %T, want map[string]any (the JSON shape)", evt.Payload)
	}
	if payload["id"] != wantID {
		t.Errorf("remote event id = %v, want %q", payload["id"], wantID)
	}
	if payload["amount"] != wantAmount {
		t.Errorf("remote event amount = %v, want %v", payload["amount"], wantAmount)
	}
}

// streamNameForEventType mirrors the package's unexported function of the
// same name: these tests address the stream and its consumers directly
// (deleting a consumer, counting how many remain, checking the stream is
// gone), which the external test package cannot do through the public API.
// Every event type these tests use is a plain dotted name, so the folding
// rule only ever turns "." into "_" here -- eventbus/redis/integration_test's
// own streamKey helper is the direct precedent for pinning wire format this
// way from the test side.
func streamNameForEventType(eventType string) string {
	return "PKGCORE_EVENTS_" + strings.ReplaceAll(eventType, ".", "_")
}

// soleConsumerName returns the name of the one consumer streamName carries,
// failing the test if there is not exactly one -- every test that calls this
// has just proven, via warmUp, that exactly the consumer(s) it expects exist.
func soleConsumerName(t *testing.T, ctx context.Context, js jetstream.JetStream, streamName string) string {
	t.Helper()
	names := consumerNames(t, ctx, js, streamName)
	if len(names) != 1 {
		t.Fatalf("stream %q carries %d consumers, want exactly 1: %v", streamName, len(names), names)
	}
	return names[0]
}

// consumerNames lists every consumer name currently on streamName.
func consumerNames(t *testing.T, ctx context.Context, js jetstream.JetStream, streamName string) []string {
	t.Helper()
	stream, err := js.Stream(ctx, streamName)
	if err != nil {
		t.Fatalf("js.Stream(%q): %v", streamName, err)
	}
	lister := stream.ConsumerNames(ctx)
	var names []string
	for name := range lister.Name() {
		names = append(names, name)
	}
	if err := lister.Err(); err != nil {
		t.Fatalf("ConsumerNames(%q) error: %v", streamName, err)
	}
	return names
}

// streamExists reports whether streamName still exists on the server.
func streamExists(t *testing.T, ctx context.Context, js jetstream.JetStream, streamName string) bool {
	t.Helper()
	_, err := js.Stream(ctx, streamName)
	if err == nil {
		return true
	}
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return false
	}
	t.Fatalf("js.Stream(%q): %v", streamName, err)
	return false
}

// TestEventBus_FanOutDeliversToEveryReplica is this round's central proof:
// two wholly independent client connections to the same JetStream-enabled
// NATS server, each wrapped in its own EventBus and both subscribed to the
// same event type, both receive one Publish from a third connection -- fan-
// out to every replica, never load-balanced delivery to only one of them.
// This is the exact property go/notification's own Redis integration tier
// proves its cross-replica inbox fan-out depends on (see that module's
// integration_test/redis_leg_test.go), re-proven here against a real NATS
// server rather than assumed to transfer.
func TestEventBus_FanOutDeliversToEveryReplica(t *testing.T) {
	ctx := context.Background()
	// A third, independent connection to the same container publishes:
	// neither replica's own Publish path is what delivers here, so a pass
	// proves the broker itself fans the message out to both durable
	// consumers, not that one replica's synchronous local-delivery path
	// happened to satisfy both recorders.
	conns := startNATSConnN(t, ctx, 3)
	connA, connB, publisherConn := conns[0], conns[1], conns[2]
	busA := eventbusnats.NewEventBus(connA)
	busB := eventbusnats.NewEventBus(connB)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
	})

	busPublisher := eventbusnats.NewEventBus(publisherConn)
	t.Cleanup(busPublisher.Close)

	const eventType = "smilesim.job.completed"
	recA, recB := &eventRecorder{}, &eventRecorder{}
	busA.Subscribe(eventType, recA.handler())
	busB.Subscribe(eventType, recB.handler())

	warmUp(t, busPublisher, recA, eventType)
	warmUp(t, busPublisher, recB, eventType)
	recA.clear()
	recB.clear()

	want := invoicePaid{ID: "job-42", Amount: 42}
	if err := busPublisher.Publish(ctx, pkgcore.Event{
		Type: eventType, TenantID: pkgcore.TenantID("tenant-acme"), Payload: want,
	}); err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}

	// BOTH replicas' handlers must fire: this is the fan-out assertion. A
	// load-balanced (shared consumer group) delivery would have exactly one
	// of these two waits time out.
	eventually(t, "replica A's handler to run", func() bool { return recA.count() == 1 })
	eventually(t, "replica B's handler to run", func() bool { return recB.count() == 1 })
	requireRemoteInvoice(t, recA.at(0), "job-42", 42, pkgcore.TenantID("tenant-acme"))
	requireRemoteInvoice(t, recB.at(0), "job-42", 42, pkgcore.TenantID("tenant-acme"))

	// And no duplicate delivery to either replica: wait out more than one
	// retry interval past the first delivery.
	time.Sleep(700 * time.Millisecond)
	if got := recA.count(); got != 1 {
		t.Errorf("replica A's handler ran %d times, want exactly 1", got)
	}
	if got := recB.count(); got != 1 {
		t.Errorf("replica B's handler ran %d times, want exactly 1", got)
	}
}

// TestEventBus_DeliversExactlyOnceLocallyAndRemotely pins the core contract
// in both directions: the publishing instance's handlers run synchronously
// with the original payload, the other instance's handler runs exactly once
// with the JSON shape, and nothing is delivered twice -- which would happen
// if a reader failed to skip the events its own instance had already
// delivered locally.
func TestEventBus_DeliversExactlyOnceLocallyAndRemotely(t *testing.T) {
	ctx := context.Background()
	connA, connB := startNATSConnPair(t, ctx)
	busA := eventbusnats.NewEventBus(connA)
	busB := eventbusnats.NewEventBus(connB)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
	})

	recA, recB := &eventRecorder{}, &eventRecorder{}
	const paidType = "invoice.paid"
	busA.Subscribe(paidType, recA.handler())
	busB.Subscribe(paidType, recB.handler())

	warmUp(t, busA, recB, paidType)
	warmUp(t, busB, recA, paidType)
	recA.clear()
	recB.clear()

	first := invoicePaid{ID: "inv-1042", Amount: 1042.5}
	if err := busA.Publish(ctx, pkgcore.Event{
		Type: paidType, TenantID: pkgcore.TenantID("tenant-acme"), Payload: first,
	}); err != nil {
		t.Fatalf("busA.Publish() error = %v, want nil", err)
	}

	if got := recA.count(); got != 1 {
		t.Fatalf("local handler ran %d times, want exactly 1", got)
	}
	local := recA.at(0)
	if local.TenantID != pkgcore.TenantID("tenant-acme") {
		t.Errorf("local event tenant = %q, want %q", local.TenantID, "tenant-acme")
	}
	asInvoice, ok := local.Payload.(invoicePaid)
	if !ok {
		t.Fatalf("local event payload is %T, want the original invoicePaid untouched", local.Payload)
	}
	if asInvoice != first {
		t.Errorf("local event payload = %+v, want the published %+v", asInvoice, first)
	}

	eventually(t, "the remote handler on bus B to run once", func() bool {
		return recB.count() == 1
	})
	requireRemoteInvoice(t, recB.at(0), "inv-1042", 1042.5, pkgcore.TenantID("tenant-acme"))

	second := invoicePaid{ID: "inv-9", Amount: 9}
	if err := busB.Publish(ctx, pkgcore.Event{
		Type: paidType, TenantID: pkgcore.TenantID("tenant-beta"), Payload: second,
	}); err != nil {
		t.Fatalf("busB.Publish() error = %v, want nil", err)
	}
	if got := recB.count(); got != 2 {
		t.Errorf("local handler on bus B ran %d times, want exactly 2", got)
	}
	eventually(t, "the remote handler on bus A to run twice", func() bool {
		return recA.count() == 2
	})
	requireRemoteInvoice(t, recA.at(1), "inv-9", 9, pkgcore.TenantID("tenant-beta"))

	time.Sleep(700 * time.Millisecond)
	if got := recA.count(); got != 2 {
		t.Errorf("bus A handler ran %d times in total, want exactly 2: an event was delivered twice", got)
	}
	if got := recB.count(); got != 2 {
		t.Errorf("bus B handler ran %d times in total, want exactly 2: an event was delivered twice", got)
	}
}

// TestEventBus_RoutesEachTypeOnItsOwnStream checks that events of one type
// never reach handlers subscribed to another, in either direction, on the
// stream-per-type design.
func TestEventBus_RoutesEachTypeOnItsOwnStream(t *testing.T) {
	ctx := context.Background()
	connA, connB := startNATSConnPair(t, ctx)
	busA := eventbusnats.NewEventBus(connA)
	busB := eventbusnats.NewEventBus(connB)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
	})

	recA, recB := &eventRecorder{}, &eventRecorder{}
	const teamCreated = "team.created"
	const planChanged = "plan.changed"
	busA.Subscribe(teamCreated, recA.handler())
	busB.Subscribe(planChanged, recB.handler())

	warmUp(t, busB, recA, teamCreated)
	warmUp(t, busA, recB, planChanged)
	recA.clear()
	recB.clear()

	if err := busA.Publish(ctx, pkgcore.Event{
		Type: planChanged, TenantID: pkgcore.TenantID("tenant-acme"), Payload: map[string]any{"plan": "pro"},
	}); err != nil {
		t.Fatalf("busA.Publish(planChanged) error = %v, want nil", err)
	}
	if err := busA.Publish(ctx, pkgcore.Event{
		Type: teamCreated, TenantID: pkgcore.TenantID("tenant-acme"), Payload: map[string]any{"team": "smile"},
	}); err != nil {
		t.Fatalf("busA.Publish(teamCreated) error = %v, want nil", err)
	}

	eventually(t, "the plan.changed handler on bus B to run", func() bool {
		return recB.count() == 1
	})
	if got := recB.at(0).Type; got != planChanged {
		t.Errorf("bus B handler received type %q, want %q", got, planChanged)
	}

	if got := recA.count(); got != 1 {
		t.Errorf("bus A handler ran %d times, want exactly 1 (its own team.created)", got)
	}
	if got := recA.at(0).Type; got != teamCreated {
		t.Errorf("bus A handler received type %q, want %q", got, teamCreated)
	}

	time.Sleep(700 * time.Millisecond)
	if got := recA.count(); got != 1 {
		t.Errorf("bus A handler ran %d times in total, want exactly 1: a plan.changed event crossed into it", got)
	}
	if got := recB.count(); got != 1 {
		t.Errorf("bus B handler ran %d times in total, want exactly 1: a team.created event crossed into it", got)
	}
}

// TestEventBus_NonJSONPayload_FailsBeforeAnythingIsDelivered pins the
// all-or-nothing rule: a payload that cannot survive JSON encoding fails the
// publish before the append, so neither the local handlers nor any remote
// handler ever sees it, and the bus keeps working for events that do encode.
func TestEventBus_NonJSONPayload_FailsBeforeAnythingIsDelivered(t *testing.T) {
	ctx := context.Background()
	connA, connB := startNATSConnPair(t, ctx)
	busA := eventbusnats.NewEventBus(connA)
	busB := eventbusnats.NewEventBus(connB)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
	})

	recA, recB := &eventRecorder{}, &eventRecorder{}
	const paidType = "invoice.paid"
	busA.Subscribe(paidType, recA.handler())
	busB.Subscribe(paidType, recB.handler())

	warmUp(t, busA, recB, paidType)
	recA.clear()
	recB.clear()

	err := busA.Publish(ctx, pkgcore.Event{Type: paidType, Payload: make(chan int)})
	if err == nil || !strings.Contains(err.Error(), "not JSON-serializable") {
		t.Fatalf("Publish(chan int) error = %v, want a not-JSON-serializable failure", err)
	}
	if got := recA.count(); got != 0 {
		t.Errorf("local handler ran %d times for the failed publish, want 0", got)
	}

	time.Sleep(750 * time.Millisecond)
	if got := recB.count(); got != 0 {
		t.Errorf("remote handler ran %d times for the failed publish, want 0: an entry reached the stream", got)
	}

	if err := busA.Publish(ctx, pkgcore.Event{
		Type: paidType, TenantID: pkgcore.TenantID("tenant-acme"), Payload: invoicePaid{ID: "inv-1", Amount: 1},
	}); err != nil {
		t.Fatalf("Publish() after the failed one error = %v, want nil", err)
	}
	eventually(t, "the recovery event to reach bus B", func() bool {
		return recB.count() == 1
	})
}

// TestEventBus_PanickingRemoteHandler_DoesNotWedgeTheReader checks that a
// handler on another instance cannot take the publishing instance down --
// its panic is contained on that type's own dispatch goroutine -- and, just
// as important, cannot take the reader itself down: later events of the same
// type still get delivered to the handlers that follow the panicking one.
func TestEventBus_PanickingRemoteHandler_DoesNotWedgeTheReader(t *testing.T) {
	ctx := context.Background()
	connA, connB := startNATSConnPair(t, ctx)
	busA := eventbusnats.NewEventBus(connA) // publisher only: subscribes no handler
	busB := eventbusnats.NewEventBus(connB)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
	})

	recB := &eventRecorder{}
	const paidType = "invoice.paid"
	busB.Subscribe(paidType, func(context.Context, pkgcore.Event) error {
		panic("remote handler bug")
	})
	busB.Subscribe(paidType, recB.handler())

	warmUp(t, busA, recB, paidType)
	recB.clear()

	for seq := 1; seq <= 2; seq++ {
		if err := busA.Publish(ctx, pkgcore.Event{
			Type: paidType, TenantID: pkgcore.TenantID("tenant-acme"),
			Payload: invoicePaid{ID: "inv-" + strings.Repeat("1", seq), Amount: float64(seq)},
		}); err != nil {
			t.Fatalf("Publish(%d) error = %v, want nil: a remote handler panic must not surface here", seq, err)
		}
	}
	eventually(t, "the healthy handler on bus B to run for both events", func() bool {
		return recB.count() == 2
	})
}

// TestEventBus_Close_StopsPublishAndRemoteDelivery pins what closing a bus
// means: its Publish starts failing with eventbusnats.ErrEventBusClosed, its
// reader stops, and events published afterwards by other instances stop
// reaching it -- the closed instance is out of the deployment.
func TestEventBus_Close_StopsPublishAndRemoteDelivery(t *testing.T) {
	ctx := context.Background()
	connA, connB := startNATSConnPair(t, ctx)
	busA := eventbusnats.NewEventBus(connA)
	busB := eventbusnats.NewEventBus(connB)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
	})

	recB := &eventRecorder{}
	const paidType = "invoice.paid"
	busB.Subscribe(paidType, recB.handler())

	warmUp(t, busA, recB, paidType)
	recB.clear()

	busB.Close()

	err := busB.Publish(ctx, pkgcore.Event{Type: paidType, Payload: invoicePaid{ID: "never", Amount: 0}})
	if !errors.Is(err, eventbusnats.ErrEventBusClosed) {
		t.Fatalf("Publish on the closed bus error = %v, want ErrEventBusClosed", err)
	}

	if err := busA.Publish(ctx, pkgcore.Event{
		Type: paidType, TenantID: pkgcore.TenantID("tenant-acme"), Payload: invoicePaid{ID: "inv-7", Amount: 7},
	}); err != nil {
		t.Fatalf("busA.Publish() after busB.Close() error = %v, want nil", err)
	}

	time.Sleep(1200 * time.Millisecond)
	if got := recB.count(); got != 0 {
		t.Errorf("handler on the closed bus ran %d times, want 0: delivery continued after Close", got)
	}
}

// TestEventBus_SubscribersNeverCatchUpOnHistory pins the live-end rule: an
// event published before an instance subscribed is history for it and is
// never replayed -- a late subscriber's consumer is created with
// DeliverNewPolicy, so it only ever sees events published after that.
func TestEventBus_SubscribersNeverCatchUpOnHistory(t *testing.T) {
	ctx := context.Background()
	connA, connB := startNATSConnPair(t, ctx)
	busA := eventbusnats.NewEventBus(connA)
	busB := eventbusnats.NewEventBus(connB)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
	})

	recA, recB := &eventRecorder{}, &eventRecorder{}
	const paidType = "invoice.paid"
	busA.Subscribe(paidType, recA.handler())

	if err := busA.Publish(ctx, pkgcore.Event{
		Type: paidType, TenantID: pkgcore.TenantID("tenant-early"),
		Payload: invoicePaid{ID: "inv-early", Amount: 0},
	}); err != nil {
		t.Fatalf("Publish(early) error = %v, want nil", err)
	}

	busB.Subscribe(paidType, recB.handler())

	deadline := time.Now().Add(10 * time.Second)
	for seq := 1; recB.count() == 0; seq++ {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for bus B to consume its first event")
		}
		if err := busA.Publish(ctx, pkgcore.Event{
			Type: paidType, TenantID: pkgcore.TenantID("warmup"), Payload: map[string]any{"seq": seq},
		}); err != nil {
			t.Fatalf("warm-up publish %d: %v", seq, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	if got := recB.countByTenant(pkgcore.TenantID("tenant-early")); got != 0 {
		t.Fatalf("late subscriber received the pre-subscription event %d times, want 0 (no catch-up)", got)
	}

	if err := busA.Publish(ctx, pkgcore.Event{
		Type: paidType, TenantID: pkgcore.TenantID("tenant-live"), Payload: invoicePaid{ID: "inv-live", Amount: 2},
	}); err != nil {
		t.Fatalf("Publish(live) error = %v, want nil", err)
	}
	eventually(t, "the live event to reach the late subscriber", func() bool {
		return recB.countByTenant(pkgcore.TenantID("tenant-live")) == 1
	})

	time.Sleep(700 * time.Millisecond)
	if got := recB.countByTenant(pkgcore.TenantID("tenant-early")); got != 0 {
		t.Errorf("late subscriber received the pre-subscription event %d times, want 0 (no catch-up)", got)
	}
	if got := recB.countByTenant(pkgcore.TenantID("tenant-live")); got != 1 {
		t.Errorf("late subscriber received the live event %d times, want exactly 1", got)
	}
}

// TestEventBus_ReaderRecoversFromALostConsumer pins the recovery from a
// vanished durable consumer: an operator's cleanup, a JetStream restore or a
// lost connection the client could not transparently resume can remove this
// instance's consumer while the reader is between deliveries. The bus must
// recreate it rather than leaving the reader silently dead -- the NATS-side
// equivalent of eventbus/redis's NOGROUP recovery test.
func TestEventBus_ReaderRecoversFromALostConsumer(t *testing.T) {
	ctx := context.Background()
	connA, connB := startNATSConnPair(t, ctx)
	busA := eventbusnats.NewEventBus(connA) // publisher only: subscribes no handler
	busB := eventbusnats.NewEventBus(connB)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
	})

	recB := &eventRecorder{}
	const paidType = "invoice.paid"
	busB.Subscribe(paidType, recB.handler())

	warmUp(t, busA, recB, paidType)
	recB.clear()

	js, err := jetstream.New(connA)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	streamName := streamNameForEventType(paidType)
	consumerName := soleConsumerName(t, ctx, js, streamName)
	if err := js.DeleteConsumer(ctx, streamName, consumerName); err != nil {
		t.Fatalf("js.DeleteConsumer(%q, %q): %v", streamName, consumerName, err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for seq := 1; recB.count() == 0; seq++ {
		if time.Now().After(deadline) {
			t.Fatal("timed out: no event reached the reader after its consumer was deleted -- the reader wedged")
		}
		if err := busA.Publish(ctx, pkgcore.Event{
			Type: paidType, TenantID: pkgcore.TenantID("tenant-acme"),
			Payload: invoicePaid{ID: "inv-recovered", Amount: float64(seq)},
		}); err != nil {
			t.Fatalf("Publish(%d) error = %v, want nil", seq, err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	recB.clear()
	if err := busA.Publish(ctx, pkgcore.Event{
		Type: paidType, TenantID: pkgcore.TenantID("tenant-acme"), Payload: invoicePaid{ID: "inv-after", Amount: 1},
	}); err != nil {
		t.Fatalf("Publish(after recovery) error = %v, want nil", err)
	}
	eventually(t, "the recovered reader to keep delivering", func() bool {
		return recB.count() == 1
	})
}

// TestEventBus_Close_LastReaderLeavesNothingBehind pins the graceful
// shutdown cleanup: a closed bus destroys the durable consumer its instance
// created, and when it was the stream's only consumer the stream is deleted
// with it -- a deployment that closes all its replicas leaves no accumulated
// consumer metadata behind on the server.
func TestEventBus_Close_LastReaderLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	connA, connB := startNATSConnPair(t, ctx)
	busA := eventbusnats.NewEventBus(connA) // publisher only: subscribes no handler
	busB := eventbusnats.NewEventBus(connB)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
	})

	recB := &eventRecorder{}
	const paidType = "invoice.paid"
	busB.Subscribe(paidType, recB.handler())
	warmUp(t, busA, recB, paidType)

	busB.Close()

	js, err := jetstream.New(connA)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	streamName := streamNameForEventType(paidType)
	if streamExists(t, ctx, js, streamName) {
		t.Errorf("stream %q still exists after its last reader closed, want it deleted", streamName)
	}
}

// TestEventBus_Close_SparesAPeerConsumer pins the boundary of the close
// cleanup: only the closing instance's own durable consumer is destroyed,
// and a stream that still carries another instance's consumer is not
// deleted -- a peer's blocked reader must never lose its stream under it.
// The surviving reader keeps delivering after the other instance closed.
func TestEventBus_Close_SparesAPeerConsumer(t *testing.T) {
	ctx := context.Background()
	conns := startNATSConnN(t, ctx, 3)
	connA, connB, connC := conns[0], conns[1], conns[2]
	busA := eventbusnats.NewEventBus(connA) // publisher only: subscribes no handler
	busB := eventbusnats.NewEventBus(connB)
	busC := eventbusnats.NewEventBus(connC)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
		busC.Close()
	})

	recB, recC := &eventRecorder{}, &eventRecorder{}
	const paidType = "invoice.paid"
	busB.Subscribe(paidType, recB.handler())
	busC.Subscribe(paidType, recC.handler())
	warmUp(t, busA, recB, paidType)
	warmUp(t, busA, recC, paidType)

	busB.Close()

	js, err := jetstream.New(connA)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	streamName := streamNameForEventType(paidType)
	names := consumerNames(t, ctx, js, streamName)
	if len(names) != 1 {
		t.Fatalf("stream %q carries %d consumers after one of two readers closed, want exactly 1 (bus C's)", streamName, len(names))
	}

	recC.clear()
	if err := busA.Publish(ctx, pkgcore.Event{
		Type: paidType, TenantID: pkgcore.TenantID("tenant-acme"), Payload: invoicePaid{ID: "inv-peer", Amount: 3},
	}); err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}
	eventually(t, "the surviving reader on bus C to deliver after bus B closed", func() bool {
		return recC.count() == 1
	})
}

// TestEventBus_ConformsToEventBusContract proves eventbusnats.NewEventBus
// satisfies the shared contract eventbustest.AssertConforms checks -- the
// same suite eventbus/redis's own integration tier runs against a real
// Redis -- against a real JetStream-enabled NATS server, so drift between
// the two distributed EventBus implementations is caught here once instead
// of pairwise. Every pair of buses AssertConforms's subtests build sits on
// two independent connections to one container (the same two-replica shape
// the cross-replica tests in this file use): each NewEventBus call is a
// genuine bus instance with its own durable consumer on every stream it
// subscribes to, so the suite's cross-instance subtests -- the assertions
// that make the suite able to see remote delivery at all, and the
// contract-suite form of verifying the MultiReplicaSafe bit this
// implementation declares when it registers -- run against a genuinely
// distributed pair. Both buses of every pair are closed on this test's
// cleanup, mirroring how every other test in this file manages an
// EventBus's lifetime.
func TestEventBus_ConformsToEventBusContract(t *testing.T) {
	ctx := context.Background()
	connA, connB := startNATSConnPair(t, ctx)

	eventbustest.AssertConforms(t, func() (pkgcore.EventBus, pkgcore.EventBus) {
		busA := eventbusnats.NewEventBus(connA)
		busB := eventbusnats.NewEventBus(connB)
		t.Cleanup(busA.Close)
		t.Cleanup(busB.Close)
		return busA, busB
	})
}
