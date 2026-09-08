// Package eventbustest verifies that a pkgcore.EventBus implementation
// upholds the contract EventBus's own doc comment describes, independent of
// which backend implements it. It exists so that every EventBus — built-in
// (pkgcore.NewMemoryEventBus, the eventbus/redis subpackage's NewEventBus)
// or host-supplied through pkgcore.WithEventBus — is checked against the
// same suite, the
// same role go/tenancy/tenancytest.AssertIsolated plays for
// dbkit.Repository[T]: with several implementations per seam, drift
// between them is caught here once instead of pairwise.
//
// # The two-instance shape
//
// AssertConforms builds its buses through a factory returning TWO
// instances — the shape of a deployment, not of a single bus. A
// single-process implementation (the in-memory bus) returns the same
// instance twice: a one-replica deployment's "second instance" is its own
// one instance, and every cross-instance assertion below reduces to an
// ordinary local-delivery check it satisfies. A broker-backed
// implementation returns two genuinely independent instances (two consumer
// groups on one Redis, two consumers on one JetStream stream, two
// replica cursors on one PostgreSQL outbox), and the same assertions
// become the checks that make the suite able to see what such a bus exists
// for: that an event published on one instance is delivered to a
// subscriber on another. The two-instance shape exists because a bus that silently delivers nothing to a remote instance would otherwise pass every single-instance check: only an assertion across two genuine instances can see a fan-out that never happens.
//
// The cross-instance assertions are also the contract-suite form of
// verifying the MultiReplicaSafe capability bit an implementation declares
// about itself when it registers: an implementation declaring
// MultiReplicaSafe is claiming exactly that a second instance of the same
// deployment observes what the first one publishes, and these assertions
// check that claim against the pair the factory builds.
//
// # What the suite does NOT assert: handler-error reporting
//
// The seam contract does not promise that Publish reports a failing
// handler's error to the publisher: reporting is possible only for handlers
// an implementation runs synchronously on the publisher's own goroutine,
// while a broker-backed bus's deliveries to other replicas run on its own
// reader goroutines, after Publish has returned, where no publisher exists
// to receive the failure (pkgcore.EventBus's own doc comment draws this
// line, and each implementation's docs describe how its machinery handles
// such failures). This suite therefore never asserts that Publish returns a
// failing handler's error -- an assertion that would reject a conformant
// implementation whose delivery runs on its own goroutines -- and pins only
// the property the contract does promise: a failing handler does not
// prevent the handlers after it from running. Where an implementation
// genuinely reports handler failures (the in-memory bus reports every one,
// being the only shape in which every handler runs on the publisher's own
// goroutine), that behavior is the implementation's own, verified by its
// own tests (pkgcore's eventbus_test.go) rather than asserted here as a
// property every implementation must share, and the suite's own acceptance
// test pins that an implementation which cannot report is still accepted.
//
// # Capability-gated assertions
//
// AssertConforms takes the capability bits the implementation under test
// declares and gates its assertions on them, so a declaration is a promise
// the suite verifies and an implementation's verification follows from its
// own declaration rather than from what a suite author happened to wire:
// the single-instance assertions run for every implementation, the
// cross-instance assertions run only for one declaring MultiReplicaSafe
// (its two-instance pair is the verification — every integration leg whose
// implementation declares the bit runs them against a real pair; an
// implementation that cannot satisfy the claim fails here, the same way
// Kernel.Bootstrap fails an assembly whose resolved implementation cannot
// satisfy the deployment mode's requirements (ErrCapabilityUnsatisfied),
// but at the level of the implementation's actual behaviour rather than its
// declaration).
//
// # SurvivesRestart and why the shared suite runs no restart protocol for it
//
// Of the bits an EventBus implementation can declare, this suite verifies
// MultiReplicaSafe alone. SurvivesRestart promises that the state an
// implementation reads and writes outlives a restart of the SERVICE that
// holds it — the Redis server behind eventbus.redis, the PostgreSQL server
// behind eventbus.postgres, the NATS server behind eventbus.nats — and
// pkgcore.Capability's own doc comment is explicit that restarting the
// application process proves nothing about that service: a consumer that
// merely talks to the service can be restarted freely, and everything it
// wrote through the service comes back. An EventBus is the transport, not
// the record: every byte it reads and writes lives inside the backend
// service (a Redis Streams entry, an outbox row, a JetStream message), and
// the bus API exposes no write-then-read path that could read that state
// back through the bus itself, so no restart protocol exists in this shared
// suite — a declaration of the bit is verified per-leg, in the backend's
// own integration tier, and each leg records exactly what its evidence
// proves and what it does not:
//
//   - A consumer-process restart — the bus's own instance closing and a
//     fresh one reopening, possibly under the same identity — proves the
//     durable-cursor-across-consumer-restart property and nothing else:
//     the events committed while the consumer was gone are delivered when
//     it returns. eventbus/postgres's catch-up proofs are exactly this
//     shape (a replica that closes and reopens under the same replicaID
//     resumes from its persisted cursor), and they are NOT a service-
//     restart proof under pkgcore.Capability's definition: the PostgreSQL
//     server never restarted in them. The same definition is why a fresh
//     consumer that starts at the live end after a crash (every backend's
//     no-catch-up rule, which this suite's own late-subscription check
//     pins) is compatible with a SurvivesRestart declaration rather than a
//     contradiction of one.
//   - A service restart — the backend's own container stopping and
//     starting between the write and the read, the shape
//     kvstoretest.AssertSurvivesRestart drives for KVStore — is the
//     evidence each declaration ultimately rests on, and the eventbus legs
//     that declare the bit run it against their real containers the same
//     way (eventbus/postgres's durable-cursor proof across a genuine
//     PostgreSQL-container restart; eventbus/redis's and eventbus/nats's
//     committed-state survival proofs against genuine Redis and NATS
//     restarts), each recording the configuration premise its backend's
//     persistence stands on — a premise that is the operator's to provide,
//     since no bus implementation can force its server's persistence
//     configuration.
package eventbustest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// conformWait bounds every single-instance assertion that would otherwise
// hang rather than fail if an implementation delivered nothing: a
// synchronous bus (the in-memory one, and a broker-backed bus's own local
// subscribers, see eventbus/redis.EventBus's doc comment) satisfies these
// well within it, so a generous margin never makes a genuinely broken
// implementation look like a slow one.
const conformWait = 5 * time.Second

// crossInstanceBudget bounds every wait in the cross-instance assertions —
// warm-up delivery, counted-event arrival, absence windows — for the same
// reason conformWait bounds the single-instance ones, with an extra margin:
// remote delivery is asynchronous on every distributed backend (a Redis
// consumer-group reader wakes at most every 500ms, a PostgreSQL listener at
// most every 2s), so warm-up alone can legitimately take a couple of
// seconds on a healthy implementation.
const crossInstanceBudget = 5 * time.Second

// crossWarmMarkers is how many warm-up events the receiving instance must
// have delivered before a cross-instance subtest's counted phase begins.
// Delivery is ordered per event type on every backend the suite runs
// against, so once three markers have arrived the instance's delivery
// machinery is provably live and every event published afterwards follows
// them — the counted events cannot race the machinery's creation the way an
// event published immediately after Subscribe could (a Redis consumer group
// is created asynchronously at first Subscribe and starts at the live end
// of its stream; a PostgreSQL replica cursor is initialized by the
// listener's first scan; a NATS consumer is created at first Subscribe).
const crossWarmMarkers = 3

// crossMarkerPace spaces warm-up publishes so a wedged or dead receiving
// instance fails its warm-up within crossInstanceBudget instead of
// flooding the broker with markers until the deadline.
const crossMarkerPace = 25 * time.Millisecond

// crossAbsenceWindow is how long a "this event never arrived" assertion
// waits after the last counted delivery before trusting the absence: a
// delivery that was already committed to a broker can land up to one read
// block (or one listener scan) later, so an absence check that ran
// immediately after a positive delivery could pass for a bus that was
// merely slow.
const crossAbsenceWindow = 800 * time.Millisecond

// conformEventType and conformOtherEventType are the two event types the
// suite subscribes and publishes against. Two distinct types are required
// to prove Subscribe matches on exact type — a bus that delivered every
// event to every handler regardless of type would still pass a
// single-type suite.
const (
	conformEventType      = "eventbustest.recorded"
	conformOtherEventType = "eventbustest.unsubscribed"
)

// crossInstanceTenant, warmUpTenant, panicFollowUpTenant, lateTenant and
// liveTenant tag the classes of event the cross-instance subtests publish,
// so one recorder can tell them apart: warm-up markers, the two counted
// events of the panic-isolation subtest, and the pre- and post-subscription
// events of the late-subscription subtest.
const (
	crossInstanceTenant = pkgcore.TenantID("eventbustest.cross-instance")
	warmUpTenant        = pkgcore.TenantID("eventbustest.warm-up")
	panicFollowUpTenant = pkgcore.TenantID("eventbustest.panic-follow-up")
	lateTenant          = pkgcore.TenantID("eventbustest.late")
	liveTenant          = pkgcore.TenantID("eventbustest.live")
)

// conformPayload is the payload AssertConforms publishes. It carries a
// Sequence so ordering checks do not depend on wall-clock timing, and is a
// plain struct with exported fields and no interface- or channel-typed
// members, because eventbus/redis.EventBus marshals every payload as JSON
// (see its Publish doc comment) — a payload only pkgcore.NewMemoryEventBus
// could carry would silently narrow this suite to one implementation.
type conformPayload struct {
	Sequence int `json:"sequence"`
}

// payloadSequence extracts the Sequence a conformPayload carries, whether
// the implementation handed the original Go value back unchanged (the
// in-memory bus) or reconstructed it from JSON (a broker-backed bus's
// remote-delivery path, which decodes the payload to a map[string]any).
func payloadSequence(payload any) (int, bool) {
	switch p := payload.(type) {
	case conformPayload:
		return p.Sequence, true
	case map[string]any:
		raw, ok := p["sequence"]
		if !ok {
			return 0, false
		}
		asFloat, ok := raw.(float64)
		if !ok {
			return 0, false
		}
		return int(asFloat), true
	default:
		return 0, false
	}
}

// AssertConforms verifies that the pair of EventBus instances the factory
// returns — two instances of one deployment, per the package doc comment —
// satisfies the contract documented on pkgcore.EventBus. caps must carry
// the capability bits the implementation under test declares about itself
// — the same bits its register.go init (or the host's WithEventBus call)
// declares, which the package's own register_test.go pins against the
// registry — and it selects which assertions run: the single-instance
// checks below run for every implementation, and the cross-instance checks
// run only for one that declares MultiReplicaSafe, because an
// implementation claiming that bit is claiming exactly that a second
// instance of the same deployment observes what the first one publishes —
// the claim those checks verify against the pair the factory builds. An
// implementation not declaring MultiReplicaSafe (the in-memory bus) runs
// the single-instance checks only; what a SurvivesRestart declaration is
// verified against is the per-leg evidence the package doc comment records,
// never an assertion this suite gates.
//
// AssertConforms calls factory once per checked property (t.Run subtest),
// never assuming state left by an earlier subtest is visible in the next:
// each subtest subscribes to its own event type variant (see the subscript
// helper), so subtests can run against a shared long-lived pair (as the
// Redis/PostgreSQL/NATS integration legs do, one container per test file
// rather than per case) without their handlers colliding. Subtests may run
// in any order; the single-instance ones use the factory's first instance,
// the cross-instance ones use both.
//
// What AssertConforms checks, in order: a published event with no
// subscribers is a no-op; a single subscriber receives the exact Event
// published; several handlers subscribed to the same type are all invoked,
// in registration order; a handler subscribed to a different type is not
// invoked; and a handler that returns an error does not prevent the
// handlers after it from running -- the suite deliberately does not assert
// that Publish reports the failing handler's error, because reporting is an
// implementation property the seam contract does not promise (a bus whose
// delivery runs on its own goroutines cannot report a handler's failure to
// any publisher; see the package doc comment and pkgcore.EventBus's own).
// Then, for an implementation declaring MultiReplicaSafe, the
// cross-instance assertions: an event published on the first instance is
// delivered to a subscriber on the second; an event published before a
// subscription existed is never replayed to the late subscriber while one
// published after it is delivered; and a handler that panics on the
// receiving instance does not stop later events of the same type from
// reaching the healthy handlers subscribed alongside it.
//
// factory must return a pair of buses ready for immediate use, with no
// subscribers of their own — AssertConforms subscribes only the handlers
// each subtest registers, so a factory returning buses that already have
// other subscribers on conformEventType-derived types would make the "no
// subscribers" and "in registration order" checks unreliable.
func AssertConforms(t *testing.T, caps pkgcore.Capability, factory func() (pkgcore.EventBus, pkgcore.EventBus)) {
	t.Helper()
	assertConforms(t, caps, factory, crossInstanceBudget)
}

// assertConforms is AssertConforms with an injectable cross-instance wait
// budget. The suite's own tests hand it a short budget when they prove —
// against deliberately defective implementations — that the cross-instance
// assertions can fail: with the real budget a rejection takes several
// seconds per subtest (each wait runs to its deadline), and the teeth
// checks would dominate the package's unit-test time for no additional
// certainty.
func assertConforms(t *testing.T, caps pkgcore.Capability, factory func() (pkgcore.EventBus, pkgcore.EventBus), crossBudget time.Duration) {
	t.Helper()

	t.Run("publish_with_no_subscribers_is_a_no_op", func(t *testing.T) {
		t.Helper()
		bus, _ := factory()
		err := bus.Publish(context.Background(), pkgcore.Event{
			Type:    subscript(conformEventType, "no-subscribers"),
			Payload: conformPayload{Sequence: 1},
		})
		if err != nil {
			t.Errorf("Publish() with no subscribers error = %v, want nil", err)
		}
	})

	t.Run("single_subscriber_receives_the_published_event", func(t *testing.T) {
		t.Helper()
		bus, _ := factory()
		eventType := subscript(conformEventType, "single-subscriber")

		received := make(chan pkgcore.Event, 1)
		bus.Subscribe(eventType, func(ctx context.Context, evt pkgcore.Event) error {
			received <- evt
			return nil
		})

		want := pkgcore.Event{
			Type:     eventType,
			TenantID: pkgcore.TenantID("eventbustest-tenant"),
			Payload:  conformPayload{Sequence: 42},
		}
		if err := bus.Publish(context.Background(), want); err != nil {
			t.Fatalf("Publish() error = %v, want nil", err)
		}

		select {
		case got := <-received:
			if got.Type != want.Type {
				t.Errorf("received Event.Type = %q, want %q", got.Type, want.Type)
			}
			if got.TenantID != want.TenantID {
				t.Errorf("received Event.TenantID = %q, want %q", got.TenantID, want.TenantID)
			}
			assertPayloadSequence(t, got.Payload, 42)
		case <-time.After(conformWait):
			t.Fatal("subscribed handler was not invoked within the wait budget")
		}
	})

	t.Run("multiple_handlers_are_all_invoked_in_registration_order", func(t *testing.T) {
		t.Helper()
		bus, _ := factory()
		eventType := subscript(conformEventType, "multiple-handlers")

		var (
			mu    sync.Mutex
			order []string
		)
		record := func(name string) pkgcore.EventHandler {
			return func(ctx context.Context, evt pkgcore.Event) error {
				mu.Lock()
				order = append(order, name)
				mu.Unlock()
				return nil
			}
		}
		bus.Subscribe(eventType, record("first"))
		bus.Subscribe(eventType, record("second"))
		bus.Subscribe(eventType, record("third"))

		if err := bus.Publish(context.Background(), pkgcore.Event{Type: eventType, Payload: conformPayload{}}); err != nil {
			t.Fatalf("Publish() error = %v, want nil", err)
		}

		waitFor(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(order) == 3
		}, "all three subscribed handlers to have been invoked")

		mu.Lock()
		defer mu.Unlock()
		want := []string{"first", "second", "third"}
		if fmt.Sprint(order) != fmt.Sprint(want) {
			t.Errorf("handlers invoked in order %v, want %v", order, want)
		}
	})

	t.Run("a_handler_for_a_different_type_is_not_invoked", func(t *testing.T) {
		t.Helper()
		bus, _ := factory()
		wantedType := subscript(conformEventType, "type-match")
		otherType := subscript(conformOtherEventType, "type-match")

		called := make(chan struct{}, 1)
		bus.Subscribe(otherType, func(ctx context.Context, evt pkgcore.Event) error {
			called <- struct{}{}
			return nil
		})
		done := make(chan struct{}, 1)
		bus.Subscribe(wantedType, func(ctx context.Context, evt pkgcore.Event) error {
			done <- struct{}{}
			return nil
		})

		if err := bus.Publish(context.Background(), pkgcore.Event{Type: wantedType, Payload: conformPayload{}}); err != nil {
			t.Fatalf("Publish() error = %v, want nil", err)
		}

		select {
		case <-done:
		case <-time.After(conformWait):
			t.Fatal("the matching-type handler was not invoked within the wait budget")
		}
		select {
		case <-called:
			t.Error("a handler subscribed to a different event type was invoked")
		case <-time.After(50 * time.Millisecond):
			// No delivery arrived in a short grace window; this is the
			// wanted outcome, not a race, because the matching handler above
			// already confirmed the bus finished delivering this Publish.
		}
	})

	t.Run("a_handler_error_does_not_block_the_next_handler", func(t *testing.T) {
		t.Helper()
		bus, _ := factory()
		eventType := subscript(conformEventType, "handler-error")
		wantErr := errors.New("eventbustest: handler deliberately failed")

		secondRan := make(chan struct{}, 1)
		bus.Subscribe(eventType, func(ctx context.Context, evt pkgcore.Event) error {
			return wantErr
		})
		bus.Subscribe(eventType, func(ctx context.Context, evt pkgcore.Event) error {
			secondRan <- struct{}{}
			return nil
		})

		if err := bus.Publish(context.Background(), pkgcore.Event{Type: eventType, Payload: conformPayload{}}); err != nil {
			// Tolerated, deliberately: whether Publish reports a failing
			// handler is an implementation property, not a promise of the
			// seam contract (pkgcore.EventBus's doc comment draws the line
			// by where the handler ran) -- an implementation that delivers
			// on its own goroutines cannot report a handler's failure to
			// any publisher and still satisfies the contract, so the
			// error's presence is asserted neither way here. What every
			// implementation promises, and what this subtest pins, is the
			// continuation: the failing handler did not stop the handler
			// registered after it. (An implementation's own reporting
			// behavior is verified by its own tests; the in-memory bus's
			// error-joining is pinned in pkgcore's eventbus_test.go.)
			t.Logf("Publish reported the failing handler (%v): reporting is this implementation's own property, exercised but not required", err)
		}

		select {
		case <-secondRan:
		case <-time.After(conformWait):
			t.Fatal("the handler after the failing one was not invoked within the wait budget")
		}
	})

	if !caps.Has(pkgcore.MultiReplicaSafe) {
		return
	}

	t.Run("an_event_published_on_one_instance_is_delivered_to_the_other", func(t *testing.T) {
		t.Helper()
		publisher, receiver := factory()
		if err := checkCrossInstanceDelivery(publisher, receiver, subscript(conformEventType, "cross-instance"), crossBudget); err != nil {
			t.Errorf("%v", err)
		}
	})

	t.Run("a_subscription_made_after_a_publish_receives_nothing_published_before_it", func(t *testing.T) {
		t.Helper()
		publisher, receiver := factory()
		if err := checkNoCatchUpForLateSubscriber(publisher, receiver, subscript(conformEventType, "late-subscription"), crossBudget); err != nil {
			t.Errorf("%v", err)
		}
	})

	t.Run("a_panicking_handler_does_not_stop_later_events_reaching_healthy_handlers", func(t *testing.T) {
		t.Helper()
		publisher, receiver := factory()
		if err := checkPanicDoesNotWedgeDelivery(publisher, receiver, subscript(conformEventType, "panic-isolation"), crossBudget); err != nil {
			t.Errorf("%v", err)
		}
	})
}

// checkCrossInstanceDelivery verifies that an event published on publisher
// is delivered to a subscriber on receiver — the property a distributed
// EventBus exists to provide, which no single-instance check can reach. It
// returns an error describing the first broken property instead of failing
// a test directly, so the suite's subtests and this package's own
// teeth-style tests (see assert_conforms_rejects_defective_buses_test.go)
// can both drive it.
func checkCrossInstanceDelivery(publisher, receiver pkgcore.EventBus, eventType string, budget time.Duration) error {
	rec := &conformRecorder{}
	receiver.Subscribe(eventType, rec.handler())

	if err := warmCrossInstance(publisher, rec, eventType, budget); err != nil {
		return err
	}

	const countedSequence = 900
	if err := publishCounted(publisher, pkgcore.Event{
		Type:     eventType,
		TenantID: crossInstanceTenant,
		Payload:  conformPayload{Sequence: countedSequence},
	}); err != nil {
		return err
	}

	if err := waitForDelivery(budget, func() bool {
		return rec.countByTenant(crossInstanceTenant) == 1
	}, "the event published on one instance to reach the subscriber on the other"); err != nil {
		return err
	}
	evt, ok := rec.firstByTenant(crossInstanceTenant)
	if !ok {
		return errors.New("the counted event was reported but cannot be read back")
	}
	if seq, ok := payloadSequence(evt.Payload); !ok || seq != countedSequence {
		return fmt.Errorf("the delivered event's payload sequence = %v (present=%v), want %d", seq, ok, countedSequence)
	}
	// The counted event must not have been delivered twice: the same
	// delivery machinery that reaches the other instance must also know it
	// already delivered this event. Waiting out the absence window turns a
	// duplicate delivery that would land within one read block into a
	// failure rather than a slow pass.
	time.Sleep(crossAbsenceWindow)
	if got := rec.countByTenant(crossInstanceTenant); got != 1 {
		return fmt.Errorf("the event published on one instance was delivered to the other %d times, want exactly 1", got)
	}
	return nil
}

// checkNoCatchUpForLateSubscriber verifies that an event published before a
// subscription existed is never replayed to the late subscriber, while one
// published after it is delivered — the live-end rule every distributed
// backend documents and the property that keeps a freshly started replica
// from re-processing its deployment's entire history.
func checkNoCatchUpForLateSubscriber(publisher, receiver pkgcore.EventBus, eventType string, budget time.Duration) error {
	// This event is published before the receiver's subscription (and, on
	// every distributed backend, before its delivery machinery) exists: it
	// is history the moment it lands, and a late subscriber must never
	// catch up on it.
	if err := publishCounted(publisher, pkgcore.Event{
		Type:     eventType,
		TenantID: lateTenant,
		Payload:  conformPayload{Sequence: 1},
	}); err != nil {
		return err
	}

	rec := &conformRecorder{}
	receiver.Subscribe(eventType, rec.handler())

	// Prove the subscription is live before publishing anything counted:
	// warm-up markers from the publisher are delivered only once the
	// receiver's machinery exists, and the pre-subscription event above
	// predates all of them, so an implementation that replays history would
	// deliver it in the same first batch as the markers.
	if err := warmCrossInstance(publisher, rec, eventType, budget); err != nil {
		return err
	}
	if got := rec.countByTenant(lateTenant); got != 0 {
		return fmt.Errorf("the late subscriber received the pre-subscription event %d times, want 0 (no catch-up)", got)
	}

	const liveSequence = 2
	if err := publishCounted(publisher, pkgcore.Event{
		Type:     eventType,
		TenantID: liveTenant,
		Payload:  conformPayload{Sequence: liveSequence},
	}); err != nil {
		return err
	}
	if err := waitForDelivery(budget, func() bool {
		return rec.countByTenant(liveTenant) >= 1
	}, "the event published after the subscription to reach the late subscriber"); err != nil {
		return err
	}

	// And the pre-subscription event stays undelivered: nothing arrives
	// late, even after the delivery machinery has demonstrably caught up
	// with the live end.
	time.Sleep(crossAbsenceWindow)
	if got := rec.countByTenant(lateTenant); got != 0 {
		return fmt.Errorf("the late subscriber received the pre-subscription event %d times, want 0 (no catch-up)", got)
	}
	return nil
}

// checkPanicDoesNotWedgeDelivery verifies that a handler panic on the
// receiving instance does not stop later events of the same type from
// reaching the healthy handlers subscribed alongside the panicking one —
// the mechanism-level property whose absence wedged a reader in the eb-3
// defect class (later events of the same type were never delivered again
// after one handler panicked).
func checkPanicDoesNotWedgeDelivery(publisher, receiver pkgcore.EventBus, eventType string, budget time.Duration) error {
	// The healthy handler subscribes first; the panicking one is added only
	// after warm-up, so the counted events below are the first the delivery
	// machinery dispatches with the panicking handler registered.
	// Registration order then guarantees the healthy handler runs before
	// the panicking one on every delivery, for a synchronous implementation
	// (which runs handlers on the caller's goroutine, in order) and an
	// asynchronous one alike.
	rec := &conformRecorder{}
	receiver.Subscribe(eventType, rec.handler())
	if err := warmCrossInstance(publisher, rec, eventType, budget); err != nil {
		return err
	}

	receiver.Subscribe(eventType, func(ctx context.Context, evt pkgcore.Event) error {
		panic("eventbustest: handler deliberately panics")
	})

	// A correct implementation contains the panic wherever the handler
	// actually runs: on its own reader goroutine for a distributed bus's
	// cross-instance delivery, or inside Publish itself for the
	// synchronous local fan-out the in-memory bus and every broker bus
	// share — each recovers and logs a panicking handler rather than
	// unwinding it through the publisher (pkgcore's runMemoryBusHandler,
	// and runLocalHandler in the redis, nats and postgres buses). The one
	// synchronous shape that can still surface the panic out of Publish
	// is a host-supplied implementation that invokes its handlers bare;
	// publishToleratingHandlerPanic absorbs that escape so the assertion
	// keeps holding for such a bus too. Either way, the events published
	// after the panic must still reach the healthy handler.
	for seq, tenant := range []pkgcore.TenantID{crossInstanceTenant, panicFollowUpTenant} {
		if err := publishToleratingHandlerPanic(publisher, pkgcore.Event{
			Type:     eventType,
			TenantID: tenant,
			Payload:  conformPayload{Sequence: 901 + seq},
		}); err != nil {
			return err
		}
	}

	return waitForDelivery(budget, func() bool {
		return rec.countByTenant(crossInstanceTenant) >= 1 && rec.countByTenant(panicFollowUpTenant) >= 1
	}, "both events published after a handler panic to reach the healthy handler")
}

// subscript derives a per-subtest event type from base, so subtests sharing
// one long-lived pair (the integration legs start one container per test
// file, not per case) never see each other's Subscribe registrations.
func subscript(base, suffix string) string {
	return base + "." + suffix
}

// waitFor polls cond until it reports true or conformWait elapses, failing
// t in the latter case with what named. It exists for assertions — like the
// registration-order check — that need every expected side effect to have
// landed before inspecting shared state, without assuming synchronous
// delivery: a broker-backed bus's own local subscribers are documented as
// synchronous (see eventbus/redis.EventBus), but polling keeps this suite
// from silently depending on that being true forever.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	if err := waitForDelivery(conformWait, cond, what); err != nil {
		t.Fatal(err)
	}
}

// waitForDelivery polls cond until it reports true or budget elapses,
// returning an error describing what instead of failing a test directly, so
// the cross-instance checks can be driven both by the suite's subtests and
// by this package's own teeth-style tests.
func waitForDelivery(budget time.Duration, cond func() bool, what string) error {
	deadline := time.Now().Add(budget)
	for {
		if cond() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s within the wait budget", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// conformRecorder accumulates what one instance's handlers saw, for later
// per-tenant count and content assertions. Handlers may run on several
// goroutines (the local publish path of a synchronous bus and the reader
// goroutines of a distributed one both invoke them), so every access is
// mutex-guarded.
type conformRecorder struct {
	mu   sync.Mutex
	evts []pkgcore.Event
}

// handler returns an EventHandler that records every event delivered to it.
func (r *conformRecorder) handler() pkgcore.EventHandler {
	return func(_ context.Context, evt pkgcore.Event) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.evts = append(r.evts, evt)
		return nil
	}
}

// countByTenant reports how many recorded events carry tenant.
func (r *conformRecorder) countByTenant(tenant pkgcore.TenantID) int {
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

// firstByTenant returns the first recorded event carrying tenant, if any.
func (r *conformRecorder) firstByTenant(tenant pkgcore.TenantID) (pkgcore.Event, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, evt := range r.evts {
		if evt.TenantID == tenant {
			return evt, true
		}
	}
	return pkgcore.Event{}, false
}

// warmCrossInstance proves that a subscriber on the receiving instance is
// actually consuming events published on the publishing one before a
// counted phase begins. Every distributed EventBus starts delivering
// asynchronously — a Redis consumer group is created on a background
// goroutine at first Subscribe and starts at the live end of its stream, a
// PostgreSQL replica cursor is initialized by the listener's first scan, a
// NATS consumer is created at first Subscribe — so an event published
// immediately after Subscribe could race the machinery's creation and be
// silently skipped, which would fail the counted assertion of a healthy
// implementation. warmCrossInstance publishes marker events from publisher
// until rec has seen crossWarmMarkers of them; delivery is ordered per
// event type, so once three markers have arrived the machinery is provably
// live and every event published afterwards is delivered after the markers.
// A subscriber that never receives anything is answered with an error
// naming the broken cross-instance delivery once the budget elapses.
func warmCrossInstance(publisher pkgcore.EventBus, rec *conformRecorder, eventType string, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for seq := 1; ; seq++ {
		if err := publishCounted(publisher, pkgcore.Event{
			Type:     eventType,
			TenantID: warmUpTenant,
			Payload:  conformPayload{Sequence: seq},
		}); err != nil {
			return err
		}
		if rec.countByTenant(warmUpTenant) >= crossWarmMarkers {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the subscriber never received %d warm-up events within the budget — cross-instance delivery appears broken", crossWarmMarkers)
		}
		time.Sleep(crossMarkerPace)
	}
}

// publishCounted publishes evt, returning the publish error if any: every
// implementation this suite runs against must accept the
// conformPayload-shaped events it publishes.
func publishCounted(bus pkgcore.EventBus, evt pkgcore.Event) error {
	if err := bus.Publish(context.Background(), evt); err != nil {
		return fmt.Errorf("Publish() of event type %q error = %w, want nil", evt.Type, err)
	}
	return nil
}

// errHandlerPanicSurfaced is the sentinel publishToleratingHandlerPanic
// returns when a Publish call ended in a panic rather than an error. No
// shipped bus produces one any more: the in-memory bus and every broker
// bus's local fan-out contain a panicking handler inside Publish itself
// (pkgcore's runMemoryBusHandler, runLocalHandler in the redis, nats and
// postgres buses), so the escape can only come from a host-supplied
// synchronous implementation that invokes its handlers bare on the
// publisher's own goroutine.
var errHandlerPanicSurfaced = errors.New("eventbustest: a handler panic surfaced out of Publish")

// publishToleratingHandlerPanic publishes evt, tolerating a handler panic
// that escapes out of Publish itself. The tolerance is aimed at a
// host-supplied synchronous implementation: such a bus invokes its
// handlers on the caller's goroutine with no containment of its own, so a
// panicking handler surfaces to the publisher rather than to a delivery
// goroutine of the bus's own — every bus this suite runs against contains
// the same panic instead, recovering and logging it and running the
// handlers registered after it (the in-memory bus through
// runMemoryBusHandler, each broker bus's local fan-out through its own
// runLocalHandler). Tolerating the escape is not weakening the assertion
// that uses this helper — such an implementation has no delivery
// machinery of its own to wedge, so the panic is the publisher's to
// observe — but a Publish that returns an error is still reported: a
// healthy implementation must accept the event.
func publishToleratingHandlerPanic(bus pkgcore.EventBus, evt pkgcore.Event) error {
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = errHandlerPanicSurfaced
			}
		}()
		return bus.Publish(context.Background(), evt)
	}()
	if errors.Is(err, errHandlerPanicSurfaced) {
		return nil // tolerated: the panic was the handler's, on the publisher's goroutine
	}
	if err != nil {
		return fmt.Errorf("Publish() of event type %q error = %w, want nil", evt.Type, err)
	}
	return nil
}

// assertPayloadSequence checks that payload is a conformPayload (or, for an
// implementation that round-trips through JSON, its map[string]any
// decoding) carrying Sequence want, so the check works whether the
// implementation handed the original Go value back unchanged (the in-memory
// bus) or reconstructed it from JSON (the Redis-backed bus's remote delivery
// path — its own local-delivery path used by this suite hands the original
// value back too, but a future subtest exercising cross-instance delivery
// would need this same tolerance).
func assertPayloadSequence(t *testing.T, payload any, want int) {
	t.Helper()
	got, ok := payloadSequence(payload)
	if !ok {
		switch p := payload.(type) {
		case map[string]any:
			t.Errorf("decoded payload has no %q field: %v", "sequence", p)
		default:
			t.Errorf("payload is %T, want conformPayload or its JSON-decoded map[string]any form", payload)
		}
		return
	}
	if got != want {
		t.Errorf("payload.Sequence = %d, want %d", got, want)
	}
}
