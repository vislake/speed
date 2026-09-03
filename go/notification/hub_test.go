package notification

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// TestHub_PublishDeliversOneCopyPerConnection pins the fan-out contract: one
// Publish reaches every subscribed connection, each with its own copy of the
// payload -- and only the payload, with the hub adding no envelope of its
// own.
func TestHub_PublishDeliversOneCopyPerConnection(t *testing.T) {
	h := NewHub()
	c1 := h.Subscribe()
	c2 := h.Subscribe()

	payload := []byte("{\"message_id\":\"m-1\"}")
	h.Publish(payload)

	assertMessage(t, c1, payload)
	assertMessage(t, c2, payload)
	assertNoMessage(t, c1)
}

// TestHub_PublishCopiesPerConnection pins the per-connection copy: mutating
// the slice one connection received must not change what a second connection
// received, and the caller may mutate its own buffer immediately after
// Publish returns.
func TestHub_PublishCopiesPerConnection(t *testing.T) {
	h := NewHub()
	c1 := h.Subscribe()
	c2 := h.Subscribe()

	payload := []byte("original")
	h.Publish(payload)
	payload[0] = 'X' // caller mutation after Publish must not reach the copies

	got1 := assertMessage(t, c1, nil)
	got2 := assertMessage(t, c2, nil)
	if string(got1) != "original" || string(got2) != "original" {
		t.Errorf("copies diverged after caller mutation: %q, %q", got1, got2)
	}
	// A consumer mutating its own copy must not leak into its neighbour's:
	// were the two deliveries sharing one backing array, got2 would have
	// changed with got1.
	got1[0] = 'Y'
	if string(got2) != "original" {
		t.Errorf("neighbour saw %q after sibling mutated its copy", got2)
	}
}

// TestHub_CloseStopsDelivery pins the Close contract: after Close, further
// Publish calls are no-ops for that connection -- including a Publish racing
// the Close itself, which must never panic and never deliver.
func TestHub_CloseStopsDelivery(t *testing.T) {
	h := NewHub()
	c := h.Subscribe()
	c.Close()
	c.Close() // idempotent

	h.Publish([]byte("after close"))
	assertNoMessage(t, c)
}

// TestHub_CloseRacingPublishNeverPanics races Close against Publish from
// many goroutines. The hub's contract is that the pair never blocks and
// never panics; whether the racing message is delivered is deliberately
// unspecified, so the test only asserts the absence of both, under -race.
func TestHub_CloseRacingPublishNeverPanics(t *testing.T) {
	h := NewHub()
	conns := make([]*HubConn, 8)
	for i := range conns {
		conns[i] = h.Subscribe()
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := range conns {
		wg.Add(1)
		go func(c *HubConn) {
			defer wg.Done()
			<-done
			c.Close()
		}(conns[i])
		wg.Add(1)
		go func(c *HubConn) {
			defer wg.Done()
			<-done
			for j := 0; j < 200; j++ {
				h.Publish([]byte("racing"))
			}
		}(conns[i])
	}
	close(done)
	wg.Wait()

	// Everything above either delivered or dropped -- either is legal. A
	// subsequent publish to the empty hub must not panic either.
	h.Publish([]byte("quiet"))
}

// TestHub_ConcurrentPublishDeliversEveryMessageToAFastConsumer pins the
// no-loss guarantee for a consumer that keeps up: while four goroutines
// publish concurrently, every one of the 40 messages must arrive on a
// connection whose reader drains it. The total stays below hubConnBuffer --
// Publish drops for a connection whose buffer is full at the instant of the
// send, so only a total the buffer can hold outright makes "every message
// arrives" a guarantee rather than a scheduling accident; with 40 < 64 the
// buffer can never be full, so the reader may even start after the last
// publish and still see everything. (The hub never closes a connection's
// channel, so nothing here may range over Messages() waiting for an end.)
func TestHub_ConcurrentPublishDeliversEveryMessageToAFastConsumer(t *testing.T) {
	h := NewHub()
	c := h.Subscribe()

	const publishers = 4
	const perPublisher = 10 // 40 total, under hubConnBuffer (64)
	var wg sync.WaitGroup
	for p := 0; p < publishers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perPublisher; i++ {
				h.Publish([]byte("msg"))
			}
		}(p)
	}
	wg.Wait()

	// Read the connection dry: the bound above makes loss impossible, and
	// reading afterwards keeps the reader's scheduling out of the proof.
	for i := 0; i < publishers*perPublisher; i++ {
		assertMessage(t, c, nil)
	}
	assertNoMessage(t, c)
}

// TestHub_SlowConsumerIsDroppedOthersUnaffected pins the slow-consumer rule:
// a connection whose buffer is full loses its copy (Publish never blocks),
// while a connection that keeps up receives every message. Each publish is
// interleaved with a blocking read on fast's connection in the main
// goroutine -- fast's buffer never fills, with no reader goroutine whose
// scheduling the test would have to trust -- while slow is never read, so
// its hubConnBuffer-deep buffer fills and the excess publishes are dropped
// for slow alone.
func TestHub_SlowConsumerIsDroppedOthersUnaffected(t *testing.T) {
	h := NewHub()
	slow := h.Subscribe()
	fast := h.Subscribe()

	// Do not read from slow; overflow its buffer by five.
	const excess = 5
	total := hubConnBuffer + excess
	for i := 0; i < total; i++ {
		h.Publish([]byte("filler"))
		select {
		case <-fast.Messages():
		case <-time.After(5 * time.Second):
			t.Fatalf("fast connection lost message %d of %d", i+1, total)
		}
	}

	// slow's first hubConnBuffer messages are buffered; the excess beyond
	// them was dropped for it alone. fast -- drained after every publish --
	// has all of them.
	for i := 0; i < hubConnBuffer; i++ {
		assertMessage(t, slow, nil)
	}
	assertNoMessage(t, slow)
}

// TestHub_HandleEventPushesThePayloadToConnections pins the hub's bus
// handler: given an EventInboxCreated carrying a payload, every connection
// receives exactly the payload's JSON -- the row announcement, not the
// envelope around it.
func TestHub_HandleEventPushesThePayloadToConnections(t *testing.T) {
	h := NewHub()
	c := h.Subscribe()

	payload := InboxCreatedPayload{
		MessageID:       "inbox-9",
		TenantID:        "tenant-acme",
		RecipientUserID: "user-7",
		TypeKey:         "clinic.appointment_reminder",
	}
	err := h.HandleEvent(context.Background(), pkgcore.Event{
		Type:     EventInboxCreated,
		TenantID: pkgcore.TenantID(payload.TenantID),
		Payload:  payload,
	})
	if err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	msg := assertMessage(t, c, nil)
	want := `{"message_id":"inbox-9","tenant_id":"tenant-acme","recipient_user_id":"user-7","type_key":"clinic.appointment_reminder"}`
	if string(msg) != want {
		t.Errorf("payload JSON = %s, want %s", msg, want)
	}
}

// TestHub_HandleEventAlwaysSucceeds pins the never-fail contract of the bus
// handler: whatever the payload is (here, a nil -- which JSON-marshals to
// "null" and is dropped rather than invented around), HandleEvent returns
// nil and publishes nothing rather than surfacing an error to the delivery
// job that published the event.
func TestHub_HandleEventAlwaysSucceeds(t *testing.T) {
	h := NewHub()
	c := h.Subscribe()

	if err := h.HandleEvent(context.Background(), pkgcore.Event{
		Type:     EventInboxCreated,
		TenantID: pkgcore.TenantID("tenant-acme"),
		Payload:  nil,
	}); err != nil {
		t.Fatalf("HandleEvent with a nil payload returned %v, want nil", err)
	}
	assertNoMessage(t, c)
}

// assertMessage reads one message from c with a timeout, asserting it
// arrived; when want is non-nil the received bytes must equal it. It returns
// the received bytes for the caller's further assertions.
func assertMessage(t *testing.T, c *HubConn, want []byte) []byte {
	t.Helper()
	select {
	case got := <-c.Messages():
		if want != nil && string(got) != string(want) {
			t.Errorf("received %q, want %q", got, want)
		}
		return got
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for a message (want %q)", want)
		return nil
	}
}

// assertNoMessage asserts that no message is currently buffered on c.
func assertNoMessage(t *testing.T, c *HubConn) {
	t.Helper()
	select {
	case got := <-c.Messages():
		t.Fatalf("received unexpected message %q", got)
	default:
	}
}
