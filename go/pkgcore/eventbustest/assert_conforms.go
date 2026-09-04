// Package eventbustest verifies that a pkgcore.EventBus implementation
// upholds the contract EventBus's own doc comment describes, independent of
// which backend implements it. It exists so that every EventBus — built-in
// (pkgcore.NewMemoryEventBus, the eventbus/redis subpackage's NewEventBus)
// or host-supplied through pkgcore.WithEventBus — is checked against the
// same suite, the
// same role go/tenancy/tenancytest.AssertIsolated plays for
// dbkit.Repository[T]: with N implementations per seam under the
// composition retrofit (see docs/internal/03-deployment-modes.md), drift
// between them is caught here once instead of pairwise.
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

// conformWait bounds every assertion that would otherwise hang rather than
// fail if an implementation delivered nothing: a synchronous bus (the
// in-memory one, and a broker-backed bus's own local subscribers, see
// eventbus/redis.EventBus's doc comment) satisfies these well within it, so a
// generous margin never makes a genuinely broken implementation look like a
// slow one.
const conformWait = 5 * time.Second

// conformEventType and conformOtherEventType are the two event types the
// suite subscribes and publishes against. Two distinct types are required
// to prove Subscribe matches on exact type — a bus that delivered every
// event to every handler regardless of type would still pass a
// single-type suite.
const (
	conformEventType      = "eventbustest.recorded"
	conformOtherEventType = "eventbustest.unsubscribed"
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

// AssertConforms verifies that the EventBus factory returns satisfies the
// contract documented on pkgcore.EventBus. It calls factory once per
// checked property (t.Run subtest), never assuming state left by an earlier
// subtest is visible in the next: each subtest subscribes to its own event
// type variant (see the subscript helper), so subtests can run against a
// shared long-lived bus (as the Redis/SMTP integration legs do, one
// container per test file rather than per case) without their handlers
// colliding.
//
// What AssertConforms checks, in order: a published event with no
// subscribers is a no-op; a single subscriber receives the exact Event
// published; several handlers subscribed to the same type are all invoked,
// in registration order; a handler subscribed to a different type is not
// invoked; and a handler that returns an error is reported by Publish
// without preventing the handlers after it from running.
//
// factory must return a bus ready for immediate use, with no subscribers of
// its own — AssertConforms subscribes only the handlers each subtest
// registers, so a factory returning a bus that already has other
// subscribers on conformEventType-derived types would make the "no
// subscribers" and "in registration order" checks unreliable.
func AssertConforms(t *testing.T, factory func() pkgcore.EventBus) {
	t.Helper()

	t.Run("publish_with_no_subscribers_is_a_no_op", func(t *testing.T) {
		t.Helper()
		bus := factory()
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
		bus := factory()
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
		bus := factory()
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
		})

		mu.Lock()
		defer mu.Unlock()
		want := []string{"first", "second", "third"}
		if fmt.Sprint(order) != fmt.Sprint(want) {
			t.Errorf("handlers invoked in order %v, want %v", order, want)
		}
	})

	t.Run("a_handler_for_a_different_type_is_not_invoked", func(t *testing.T) {
		t.Helper()
		bus := factory()
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

	t.Run("a_handler_error_is_reported_without_blocking_the_next_handler", func(t *testing.T) {
		t.Helper()
		bus := factory()
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

		err := bus.Publish(context.Background(), pkgcore.Event{Type: eventType, Payload: conformPayload{}})
		if err == nil {
			t.Error("Publish() error = nil, want a non-nil error reporting the failing handler")
		}

		select {
		case <-secondRan:
		case <-time.After(conformWait):
			t.Fatal("the handler after the failing one was not invoked within the wait budget")
		}
	})
}

// subscript derives a per-subtest event type from base, so subtests sharing
// one long-lived bus (the integration legs start one container per test
// file, not per case) never see each other's Subscribe registrations.
func subscript(base, suffix string) string {
	return base + "." + suffix
}

// waitFor polls cond until it reports true or conformWait elapses, failing t
// in the latter case. It exists for assertions — like the registration-order
// check — that need every expected side effect to have landed before
// inspecting shared state, without assuming synchronous delivery: a
// broker-backed bus's own local subscribers are documented as synchronous
// (see eventbus/redis.EventBus), but polling keeps this suite from silently
// depending on that being true forever.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(conformWait)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("condition was not satisfied within the wait budget")
		}
		time.Sleep(time.Millisecond)
	}
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
	switch p := payload.(type) {
	case conformPayload:
		if p.Sequence != want {
			t.Errorf("payload.Sequence = %d, want %d", p.Sequence, want)
		}
	case map[string]any:
		got, ok := p["sequence"]
		if !ok {
			t.Errorf("decoded payload has no %q field: %v", "sequence", p)
			return
		}
		gotFloat, ok := got.(float64)
		if !ok || int(gotFloat) != want {
			t.Errorf("decoded payload sequence = %v, want %d", got, want)
		}
	default:
		t.Errorf("payload is %T, want conformPayload or its JSON-decoded map[string]any form", payload)
	}
}
