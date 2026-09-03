// subscribe_test.go pins the resilience contract of the hub subscription
// Module.Register wires -- reg.Events.Subscribe(EventInboxCreated,
// m.hub.HandleEvent), module.go's third phase -- as a connection on the
// module's replica hub experiences it. hub_test.go proves HandleEvent's
// behaviour in isolation and module_test.go proves that a well-formed inbox
// event reaches a connection; this file pins the two failure-facing halves
// of the same wiring:
//
//  1. The subscription is the ONLY gate between the host's bus and the
//     hub's connections. An event of any other type must never reach a
//     connection, because on a real host this module shares its bus with
//     every module that publishes domain events, and the inbox fan-out
//     must not become their echo chamber.
//  2. An inbox announcement that has no deliverable form -- no payload at
//     all, or a payload no JSON representation exists for -- is dropped,
//     and the drop is total: no partial message, no panicked subscriber,
//     no poisoned connection. The announcements after it still arrive, on
//     the same connection, byte-identical to what a live delivery would
//     publish.
//
// Property 2 is the module's "fail loud" for announcements, read per
// hub.go's contract: HandleEvent's signature cannot fail, because the row
// behind an announcement is already durable and an error could only fail
// the delivery job that published it. Loudness therefore lives in the
// observable promise pinned here -- a bad announcement never takes the good
// ones down with it -- and in the delivery pipeline's own guarantee that
// the row an announcement describes is always recoverable through
// Repository (delivery_test.go pins that half). hub.go's doc comment
// explains why an unmarshalable payload cannot occur in a running system
// (every declared payload is a JSON-safe struct); the tests below deliver
// one anyway, through the in-process bus, to prove the defensive drop
// exists and has no side effects.
package notification

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestHubSubscription_OtherEventTypesNeverReachAConnection proves the
// subscription's type filter from the connection side: an event that is not
// notification's own, published on the host's bus, must not arrive. The
// in-process bus dispatches synchronously, so by the time Publish returns
// the (non-)delivery has already happened and the non-blocking
// assertNoMessage is decisive. A connection that received nothing could
// also mean the wiring is broken, so the test then publishes a real inbox
// announcement and requires it to arrive: the silence on the first event is
// type filtering, not a dead fan-out.
func TestHubSubscription_OtherEventTypesNeverReachAConnection(t *testing.T) {
	db := newTestDB(t)
	module := NewModule(db, testModuleOptions(t)...)
	reg := newHostRegistry(t)

	if err := module.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	conn := module.hub.Subscribe()
	defer conn.Close()

	// An event of another domain -- here the clinic module's own booking
	// event, which on a real host announces something that later becomes a
	// notification through a different subscriber -- rides the same host
	// bus and must pass the hub's subscription by.
	if err := reg.Events.Bus().Publish(context.Background(), pkgcore.Event{
		Type:     "clinic.appointment_booked",
		TenantID: pkgcore.TenantID("tenant-acme"),
		Payload: map[string]any{
			"appointment_id":  "appt-41",
			"patient_user_id": "user-7",
		},
	}); err != nil {
		t.Fatalf("bus publish: %v", err)
	}
	assertNoMessage(t, conn)

	// The wiring is alive: a well-formed inbox announcement still reaches
	// the connection, byte-identical to the shape a live delivery job
	// publishes (events.go's payload contract).
	payload := InboxCreatedPayload{
		MessageID:       "inbox-91",
		TenantID:        "tenant-acme",
		RecipientUserID: "user-7",
		TypeKey:         "clinic.appointment_reminder",
	}
	if err := reg.Events.Bus().Publish(context.Background(), pkgcore.Event{
		Type:     EventInboxCreated,
		TenantID: pkgcore.TenantID(payload.TenantID),
		Payload:  payload,
	}); err != nil {
		t.Fatalf("bus publish: %v", err)
	}

	want := `{"message_id":"inbox-91","tenant_id":"tenant-acme","recipient_user_id":"user-7","type_key":"clinic.appointment_reminder"}`
	msg := assertMessage(t, conn, nil)
	if string(msg) != want {
		t.Errorf("hub payload JSON = %s, want %s", msg, want)
	}
}

// TestHubSubscription_UndeliverableAnnouncementsAreDroppedTotally proves the
// drop side of the never-fail contract through the same wiring a host runs.
// Two announcements that have no deliverable form are dropped -- one with no
// payload at all, one whose payload is a channel value, which JSON cannot
// represent (it reaches the handler unmarshalled through the in-process
// bus, exactly as a misbehaving in-process publisher could deliver it) --
// and a real announcement published after each must still arrive. Running
// the whole sequence on one connection proves the drop poisons neither the
// hub's subscription nor the connection's stream, and the deliberate
// silences in between prove nothing is invented around the bad payloads:
// the connection receives exactly the good announcements and nothing else.
func TestHubSubscription_UndeliverableAnnouncementsAreDroppedTotally(t *testing.T) {
	db := newTestDB(t)
	module := NewModule(db, testModuleOptions(t)...)
	reg := newHostRegistry(t)

	if err := module.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	conn := module.hub.Subscribe()
	defer conn.Close()

	publishInbox := func(payload any) {
		t.Helper()
		if err := reg.Events.Bus().Publish(context.Background(), pkgcore.Event{
			Type:     EventInboxCreated,
			TenantID: pkgcore.TenantID("tenant-acme"),
			Payload:  payload,
		}); err != nil {
			t.Fatalf("bus publish: %v", err)
		}
	}

	// An inbox event with no payload is dropped rather than pushed as the
	// literal JSON "null" (hub.go's doc comment names that rule).
	publishInbox(nil)
	assertNoMessage(t, conn)

	// An inbox event whose payload has no JSON representation is dropped
	// rather than invented around; the publish itself still succeeds, so
	// the publisher cannot tell the announcement was undeliverable -- which
	// is safe only because the row behind it is already durable.
	publishInbox(make(chan int))
	assertNoMessage(t, conn)

	// Both drops were total: the next well-formed announcement arrives
	// intact on the same connection...
	payload := InboxCreatedPayload{
		MessageID:       "inbox-92",
		TenantID:        "tenant-acme",
		RecipientUserID: "user-7",
		TypeKey:         "clinic.appointment_reminder",
	}
	publishInbox(payload)
	want := `{"message_id":"inbox-92","tenant_id":"tenant-acme","recipient_user_id":"user-7","type_key":"clinic.appointment_reminder"}`
	msg := assertMessage(t, conn, nil)
	if string(msg) != want {
		t.Errorf("hub payload JSON = %s, want %s", msg, want)
	}

	// ...and nothing else is buffered behind it.
	assertNoMessage(t, conn)
}
