//go:build integration

package pkgcore_test

// Integration tests for NewRedisEventBus: two bus instances sharing one
// client stand in for two replicas of a deployment, and the tests pin the
// delivery contract the implementation documents -- handlers on the
// publishing instance run synchronously with the original payload, handlers
// on every other instance eventually run exactly once with the
// JSON-reconstructed payload shape, events never cross type streams, a
// publish that cannot commit delivers nothing anywhere, and Close really
// stops a replica's delivery.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
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
// local publish path and the reader goroutines both invoke them -- so every
// access goes through the mutex.
type eventRecorder struct {
	mu   sync.Mutex
	evts []pkgcore.Event
}

// handler returns an EventHandler that records every event delivered to it.
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
// test in the latter case. Cross-process delivery is asynchronous: a reader
// goroutine wakes up at most every redisEventReaderBlock (500ms) to take new
// entries off the stream, so remote delivery of an already-committed event
// lands well inside this five-second window.
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

// warmUp proves that receiver's consumer group on the eventType stream exists
// and its reader goroutine is actually consuming, by publishing marker events
// from publisher until one of them reaches receiver. Subscribe starts the
// reader asynchronously -- the group is created on a background goroutine and
// starts at the live end of the stream -- so a test that publishes a real
// event immediately after Subscribe could race the group's creation and lose
// it. Once receiver reports a marker, the wait for one full read block (600ms)
// lets any other marker that was already appended drain out, so clearing both
// recorders afterwards leaves counts that only the test's own events move.
func warmUp(t *testing.T, publisher *pkgcore.RedisEventBus, receiver *eventRecorder, eventType string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
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
			time.Sleep(600 * time.Millisecond) // one full read block: drain stragglers
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("warm-up: no marker event reached the receiver within 5s")
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

// TestRedisEventBus_DeliversExactlyOnceLocallyAndRemotely pins the core
// contract in both directions: the publishing instance's handlers run
// synchronously with the original payload, the other instance's handler runs
// exactly once with the JSON shape, and nothing is delivered twice -- which
// would happen if a reader failed to skip the events its own instance had
// already delivered locally.
func TestRedisEventBus_DeliversExactlyOnceLocallyAndRemotely(t *testing.T) {
	ctx := context.Background()
	client := startRedisClient(t, ctx)
	busA := pkgcore.NewRedisEventBus(client)
	busB := pkgcore.NewRedisEventBus(client)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
	})

	recA, recB := &eventRecorder{}, &eventRecorder{}
	const paidType = "invoice.paid"
	busA.Subscribe(paidType, recA.handler())
	busB.Subscribe(paidType, recB.handler())

	// Both instances' readers must be consuming before any counted event is
	// published (see warmUp). The marker events land in both recorders -- the
	// publisher's local handlers see them synchronously, the remote reader
	// delivers them asynchronously -- so both recorders are cleared once both
	// directions are warm, and only the test's own events move the counts.
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

	// The local side is synchronous: by the time Publish returned, the local
	// handler had run once, with the original concrete type and tenant.
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

	// The remote side is asynchronous and gets the JSON-reconstructed shape.
	eventually(t, "the remote handler on bus B to run once", func() bool {
		return recB.count() == 1
	})
	requireRemoteInvoice(t, recB.at(0), "inv-1042", 1042.5, pkgcore.TenantID("tenant-acme"))

	// And the reverse direction: bus B publishes, bus A receives remotely.
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

	// No duplicates: wait out more than a full read block on each side; any
	// second delivery would have happened within one block of the publish.
	time.Sleep(700 * time.Millisecond)
	if got := recA.count(); got != 2 {
		t.Errorf("bus A handler ran %d times in total, want exactly 2: an event was delivered twice", got)
	}
	if got := recB.count(); got != 2 {
		t.Errorf("bus B handler ran %d times in total, want exactly 2: an event was delivered twice", got)
	}
}

// TestRedisEventBus_RoutesEachTypeOnItsOwnStream checks that events of one
// type never reach handlers subscribed to another, in either direction, on
// the stream-per-type design.
func TestRedisEventBus_RoutesEachTypeOnItsOwnStream(t *testing.T) {
	ctx := context.Background()
	client := startRedisClient(t, ctx)
	busA := pkgcore.NewRedisEventBus(client)
	busB := pkgcore.NewRedisEventBus(client)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
	})

	recA, recB := &eventRecorder{}, &eventRecorder{}
	const teamCreated = "team.created"
	const planChanged = "plan.changed"
	busA.Subscribe(teamCreated, recA.handler())
	busB.Subscribe(planChanged, recB.handler())

	warmUp(t, busB, recA, teamCreated) // bus B publishes, bus A's reader consumes
	warmUp(t, busA, recB, planChanged) // bus A publishes, bus B's reader consumes
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

	// bus A published both events locally but only subscribed to team.created,
	// and team.created must not leak to bus B's plan.changed handler.
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

// TestRedisEventBus_NonJSONPayload_FailsBeforeAnythingIsDelivered pins the
// all-or-nothing rule: a payload that cannot survive JSON encoding fails the
// publish before the append, so neither the local handlers nor any remote
// handler ever sees it, and the bus keeps working for events that do encode.
func TestRedisEventBus_NonJSONPayload_FailsBeforeAnythingIsDelivered(t *testing.T) {
	ctx := context.Background()
	client := startRedisClient(t, ctx)
	busA := pkgcore.NewRedisEventBus(client)
	busB := pkgcore.NewRedisEventBus(client)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
	})

	recA, recB := &eventRecorder{}, &eventRecorder{}
	const paidType = "invoice.paid"
	busA.Subscribe(paidType, recA.handler())
	busB.Subscribe(paidType, recB.handler())

	warmUp(t, busA, recB, paidType) // bus B's reader is provably consuming
	recA.clear()
	recB.clear()

	err := busA.Publish(ctx, pkgcore.Event{Type: paidType, Payload: make(chan int)})
	if err == nil || !strings.Contains(err.Error(), "not JSON-serializable") {
		t.Fatalf("Publish(chan int) error = %v, want a not-JSON-serializable failure", err)
	}
	if got := recA.count(); got != 0 {
		t.Errorf("local handler ran %d times for the failed publish, want 0", got)
	}

	// Nothing was appended to the stream either: bus B's reader, proven alive
	// above, would have delivered within one read block of an append.
	time.Sleep(750 * time.Millisecond)
	if got := recB.count(); got != 0 {
		t.Errorf("remote handler ran %d times for the failed publish, want 0: an entry reached the stream", got)
	}

	// The failure left nothing behind: the next well-formed event flows end to
	// end as usual.
	if err := busA.Publish(ctx, pkgcore.Event{
		Type: paidType, TenantID: pkgcore.TenantID("tenant-acme"), Payload: invoicePaid{ID: "inv-1", Amount: 1},
	}); err != nil {
		t.Fatalf("Publish() after the failed one error = %v, want nil", err)
	}
	eventually(t, "the recovery event to reach bus B", func() bool {
		return recB.count() == 1
	})
}

// TestRedisEventBus_PanickingRemoteHandler_DoesNotWedgeTheReader checks that
// a handler on another instance cannot take the publishing instance down --
// its panic is contained on the reader goroutine -- and, just as important,
// cannot take the reader itself down: later events of the same type still get
// delivered to the handlers that follow the panicking one.
func TestRedisEventBus_PanickingRemoteHandler_DoesNotWedgeTheReader(t *testing.T) {
	ctx := context.Background()
	client := startRedisClient(t, ctx)
	busA := pkgcore.NewRedisEventBus(client)
	busB := pkgcore.NewRedisEventBus(client)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
	})

	recB := &eventRecorder{}
	const paidType = "invoice.paid"
	// bus A only publishes: it subscribes no handler, so every delivery of its
	// events happens on bus B's reader goroutine, where the panic must be
	// contained.
	// The panicking handler registers first, so delivery to the healthy one
	// below only happens if the reader survives the panic.
	busB.Subscribe(paidType, func(context.Context, pkgcore.Event) error {
		panic("remote handler bug")
	})
	busB.Subscribe(paidType, recB.handler())

	warmUp(t, busA, recB, paidType) // warm-up events already survive the panic
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

// TestRedisEventBus_Close_StopsPublishAndRemoteDelivery pins what closing a
// bus means: its Publish starts failing with ErrEventBusClosed, its reader
// goroutines shut down, and events published afterwards by other instances
// stop reaching it -- the closed instance is out of the deployment.
func TestRedisEventBus_Close_StopsPublishAndRemoteDelivery(t *testing.T) {
	ctx := context.Background()
	client := startRedisClient(t, ctx)
	busA := pkgcore.NewRedisEventBus(client)
	busB := pkgcore.NewRedisEventBus(client)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
	})

	recB := &eventRecorder{}
	const paidType = "invoice.paid"
	busB.Subscribe(paidType, recB.handler())

	warmUp(t, busA, recB, paidType) // bus B's reader is provably consuming
	recB.clear()

	busB.Close()

	err := busB.Publish(ctx, pkgcore.Event{Type: paidType, Payload: invoicePaid{ID: "never", Amount: 0}})
	if !errors.Is(err, pkgcore.ErrEventBusClosed) {
		t.Fatalf("Publish on the closed bus error = %v, want ErrEventBusClosed", err)
	}

	// bus A is untouched by bus B's close and keeps publishing...
	if err := busA.Publish(ctx, pkgcore.Event{
		Type: paidType, TenantID: pkgcore.TenantID("tenant-acme"), Payload: invoicePaid{ID: "inv-7", Amount: 7},
	}); err != nil {
		t.Fatalf("busA.Publish() after busB.Close() error = %v, want nil", err)
	}

	// ...but nothing reaches the closed instance: its reader wakes within one
	// read block, sees the bus closed, and exits without consuming.
	time.Sleep(1200 * time.Millisecond)
	if got := recB.count(); got != 0 {
		t.Errorf("handler on the closed bus ran %d times, want 0: delivery continued after Close", got)
	}
}

// TestRedisEventBus_SubscribersNeverCatchUpOnHistory pins the live-end rule:
// an event published before an instance subscribed is history for it and is
// never replayed -- a late subscriber only sees events published after its
// consumer group was created.
func TestRedisEventBus_SubscribersNeverCatchUpOnHistory(t *testing.T) {
	ctx := context.Background()
	client := startRedisClient(t, ctx)
	busA := pkgcore.NewRedisEventBus(client)
	busB := pkgcore.NewRedisEventBus(client)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
	})

	recA, recB := &eventRecorder{}, &eventRecorder{}
	const paidType = "invoice.paid"
	busA.Subscribe(paidType, recA.handler())

	// bus B does not exist yet: this event is history the moment it lands.
	if err := busA.Publish(ctx, pkgcore.Event{
		Type: paidType, TenantID: pkgcore.TenantID("tenant-early"),
		Payload: invoicePaid{ID: "inv-early", Amount: 0},
	}); err != nil {
		t.Fatalf("Publish(early) error = %v, want nil", err)
	}

	busB.Subscribe(paidType, recB.handler())

	// Wait until bus B consumes, using markers whose tenant identifies them.
	// Had bus B's group started at the stream's beginning instead of its live
	// end, the early event would arrive in the same first batch as the
	// markers, and the assertion below would catch it.
	deadline := time.Now().Add(5 * time.Second)
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

	// And the early event stays undelivered; nothing arrives late.
	time.Sleep(700 * time.Millisecond)
	if got := recB.countByTenant(pkgcore.TenantID("tenant-early")); got != 0 {
		t.Errorf("late subscriber received the pre-subscription event %d times, want 0 (no catch-up)", got)
	}
	if got := recB.countByTenant(pkgcore.TenantID("tenant-live")); got != 1 {
		t.Errorf("late subscriber received the live event %d times, want exactly 1", got)
	}
}
