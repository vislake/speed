package notification

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/vislake/speed/go/pkgcore"
)

// hubConnBuffer is the per-connection buffered-message capacity of a Hub.
// It exists so that a modest burst of inbox events can be absorbed while a
// consumer drains, and its bound is what makes Publish's contract -- never
// block, drop for the slow consumer -- implementable at all.
const hubConnBuffer = 64

// Hub is the module's per-replica realtime fan-out: the delivery job writes
// an inbox row and publishes EventInboxCreated on the platform bus, and
// every replica's Hub -- subscribed to that event during Register -- pushes
// the row's announcement to the connections that replica holds. Each
// replica owns exactly one Hub (constructed in NewModule), and the
// platform-staff shell that pushes these announcements to a browser or
// device is a later round's consumer of the connections Subscribe returns.
//
// A Hub is deliberately NOT a delivery channel for the row itself: the row
// is committed to the database before the event goes out, so an
// announcement that reaches nobody loses nothing -- the consumer reads the
// row back through Repository. The hub's only value is latency, and every
// design decision below follows from that:
//
//   - Connections are per replica. There is no cross-replica hub and no
//     Redis Pub/Sub seam: a replica that has no connection for a recipient
//     drops the push, and the recipient's own replica serves them. A
//     distributed deployment therefore pushes through as many hubs as
//     there are replicas, each handling only its local sockets -- the
//     per-replica property that makes "every replica gets every event"
//     (the bus contract) compose with "a socket is owned by exactly one
//     replica" (the reality of load balancing).
//
//   - Publish never blocks and never fails. A connection whose buffer is
//     full is a slow consumer; pushing to it would let one stalled client
//     stall the inbox fan-out of everyone else on the replica, so its copy
//     is dropped instead. The drop is per connection -- other connections
//     on the same replica are unaffected -- and the announcement itself is
//     never retried, because the row behind it is already durable.
//
//   - The connection channel is never closed. Close signals a per-
//     connection done channel and removes the connection from the registry;
//     the buffered channel is left for the garbage collector, because a
//     send racing a close on a channel is precisely the race that panics
//     a concurrent hub. A reader that stops reading after Close simply
//     abandons the buffer.
//
// Hub is safe for concurrent use: Publish, Subscribe and Close may be
// called from any goroutine. Data []byte handed to Publish is copied per
// connection, so a caller may reuse or mutate its slice immediately after
// Publish returns.
type Hub struct {
	mu    sync.Mutex
	conns map[*HubConn]struct{}
}

// NewHub returns an empty Hub. A Module constructs one at NewModule time
// and registers it on the inbox event during Register; a host never
// constructs a Hub of its own.
func NewHub() *Hub {
	return &Hub{conns: make(map[*HubConn]struct{})}
}

// Subscribe registers a new connection and returns it. The connection is
// live from the moment Subscribe returns: a Publish racing Subscribe may
// already have delivered to it.
func (h *Hub) Subscribe() *HubConn {
	c := &HubConn{
		hub:  h,
		msgs: make(chan []byte, hubConnBuffer),
		done: make(chan struct{}),
	}
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
	return c
}

// Publish delivers a copy of data to every live connection, best effort:
// it never blocks and never reports failure. A connection that was closed
// between Subscribe and Publish, or whose buffer is full, receives no copy
// and does not hold the others up.
func (h *Hub) Publish(data []byte) {
	h.mu.Lock()
	conns := make([]*HubConn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()

	for _, c := range conns {
		// One copy per connection: a consumer that mutates its message
		// (or a marshal buffer a test reuses) must not leak into its
		// neighbours on the same replica.
		msg := append([]byte(nil), data...)
		select {
		case <-c.done:
			// Closed between the snapshot above and this send -- drop.
		case c.msgs <- msg:
			// Delivered.
		default:
			// Buffer full: a slow consumer. Drop its copy, deliver
			// to the rest.
		}
	}
}

// HandleEvent is the Hub's bus handler, subscribed to EventInboxCreated
// during Register. It marshals the event's payload and pushes it to every
// connection of this replica.
//
// It always returns nil, by design: the hub is a latency accelerator in
// front of a durable row, so a push failure must never surface to the
// publisher (which would fail the whole delivery job for an announcement
// whose row is already committed). An event carrying no payload is dropped
// rather than pushed as the literal JSON "null" -- a connection reading
// messages would otherwise have to understand that a four-byte "null"
// means "nothing announced" -- and an unmarshalable payload is likewise
// impossible in a running system (every declared payload is a JSON-safe
// struct) and is dropped rather than invented around.
func (h *Hub) HandleEvent(_ context.Context, evt pkgcore.Event) error {
	if evt.Payload == nil {
		return nil
	}
	payload, err := json.Marshal(evt.Payload)
	if err != nil {
		return nil
	}
	h.Publish(payload)
	return nil
}

// HubConn is one consumer's connection to a Hub, returned by Hub.Subscribe.
// It is the whole consumer surface: read Messages, and Close when done.
// The platform-staff shell of a later round owns one per open browser or
// device connection; tests own them here.
type HubConn struct {
	hub  *Hub
	msgs chan []byte
	done chan struct{}
	once sync.Once
}

// Messages returns the connection's message stream. Each received slice is
// this connection's own copy and may be mutated freely. Messages is never
// closed -- Close signals termination through the connection's done
// channel, so a reader detects it either by closing Messages' own
// consumer goroutine or by ranging until the Hub itself is abandoned.
func (c *HubConn) Messages() <-chan []byte {
	return c.msgs
}

// Close removes the connection from its Hub and makes future deliveries to
// it no-ops. It is idempotent and safe to call from any goroutine,
// including one racing Publish.
func (c *HubConn) Close() {
	c.once.Do(func() {
		// Signal first, then unregister: a Publish that snapshotted this
		// connection before the signal selects the done branch and drops;
		// one that runs after unregistration never sees it at all.
		close(c.done)
		c.hub.mu.Lock()
		delete(c.hub.conns, c)
		c.hub.mu.Unlock()
	})
}

// compile-time checks: *HubConn satisfies nothing external, but *Hub stays
// honest about the pkgcore dependency it already carries.
var _ pkgcore.EventHandler = (*Hub)(nil).HandleEvent
