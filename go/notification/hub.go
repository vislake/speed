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
// replica owns exactly one Hub (constructed in NewModule), and no
// platform-staff push consumer exists to carry announcements to a browser
// or device -- the connections Subscribe returns serve the inbox stream
// and tests.
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
//   - Connections are scoped to the announcement's recipient. An
//     announcement names the recipient it was delivered to (see
//     SubscribeFor and PublishFor), and the hub matches before it enqueues:
//     an unscoped connection takes every announcement, while a connection
//     scoped to one (tenant, recipient) takes only the announcements for
//     that pair. The match happens BEFORE the per-connection buffer, which
//     is what keeps one connection's buffer from being flooded by other
//     tenants' or other recipients' announcements -- the volume of the
//     whole platform can never crowd a legitimate client's own
//     announcements out of its own 64-deep buffer. In production every
//     fan-out is scoped (the module's deliverInbox publishes only its own
//     announcements), so this is the property the inbox stream's flood
//     protection rests on.
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
// Hub is safe for concurrent use: Publish, PublishFor, Subscribe,
// SubscribeFor and Close may be called from any goroutine. Data []byte
// handed to Publish/PublishFor is copied per connection, so a caller may
// reuse or mutate its slice immediately after the call returns.
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

// Subscribe registers a new UNscoped connection and returns it: the
// connection takes every announcement the hub fans out -- the shape this
// package's hub tests subscribe with, and the shape the inbox stream
// deliberately does NOT use (it subscribes through SubscribeFor, scoped to
// the caller). The
// connection is live from the moment Subscribe returns: a Publish racing
// Subscribe may already have delivered to it.
func (h *Hub) Subscribe() *HubConn {
	return h.subscribe("", "")
}

// SubscribeFor registers a connection scoped to one recipient of one
// tenant: the connection receives exactly the announcements whose (tenant,
// recipient) pair matches the scope it names -- the inbox stream's
// subscription shape, where the caller's tenant and user id are known
// before the stream opens. A scoped connection never receives another
// recipient's or another tenant's announcement, and never receives an
// unscoped publish, so its buffer is its own announcements' alone. The
// connection is live from the moment SubscribeFor returns, exactly as for
// Subscribe.
func (h *Hub) SubscribeFor(tenantID, recipientUserID string) *HubConn {
	return h.subscribe(tenantID, recipientUserID)
}

// subscribe builds a connection and registers it; an empty scope means the
// connection takes every announcement.
func (h *Hub) subscribe(tenantID, recipientUserID string) *HubConn {
	c := &HubConn{
		hub:  h,
		msgs: make(chan []byte, hubConnBuffer),
		done: make(chan struct{}),
	}
	if tenantID != "" && recipientUserID != "" {
		c.tenantID = tenantID
		c.recipientUserID = recipientUserID
	}
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
	return c
}

// Publish delivers a copy of data to every live UNscoped connection, best
// effort: it never blocks and never reports failure. An unscoped publish
// names no recipient, so it cannot be matched onto a recipient-scoped
// connection (SubscribeFor) at all -- a scoped connection receives only
// announcements PublishFor delivers to its own scope. A connection that
// was closed between the call and its delivery, or whose buffer is full,
// receives no copy and does not hold the others up.
func (h *Hub) Publish(data []byte) {
	h.deliver(data, "", "")
}

// PublishFor delivers a copy of data -- one announcement -- to every
// connection that accepts the (tenant, recipient) pair it names: every
// unscoped connection, plus exactly the connection scoped to that pair.
// This is the delivery path the hub's bus handler (HandleEvent) uses, and
// the pair is the announcement's own, so in production every fan-out is
// matched before it reaches a buffer. Best effort exactly as Publish:
// never blocks, never reports failure.
func (h *Hub) PublishFor(data []byte, tenantID, recipientUserID string) {
	h.deliver(data, tenantID, recipientUserID)
}

// deliver snapshots the connection set and pushes one copy of data to each
// connection that accepts the announcement's (tenant, recipient) scope.
func (h *Hub) deliver(data []byte, tenantID, recipientUserID string) {
	h.mu.Lock()
	conns := make([]*HubConn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()

	for _, c := range conns {
		if !c.accepts(tenantID, recipientUserID) {
			// Another recipient's or another tenant's announcement, or an
			// unscoped frame on a scoped connection: never enqueued, so it
			// cannot occupy the connection's buffer.
			continue
		}
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
// during Register. It marshals the event's payload, reads the
// announcement's own (tenant, recipient) scope out of it, and pushes it to
// the connections that accept that scope -- the scoped fan-out that keeps
// every recipient's connection buffer its own. Because the scope is read
// from the payload the announcement itself carries, the same per-replica
// hub serves every recipient's stream with no per-recipient bookkeeping at
// subscribe time.
//
// It always returns nil, by design: the hub is a latency accelerator in
// front of a durable row, so a push failure must never surface to the
// publisher (which would fail the whole delivery job for an announcement
// whose row is already committed). An event carrying no payload is dropped
// rather than pushed as the literal JSON "null" -- a connection reading
// messages would otherwise have to understand that a four-byte "null"
// means "nothing announced" -- and a payload that cannot name an
// announcement's recipient (an event shape this hub never receives in a
// running system, since it subscribes to one event type whose payload is
// always InboxCreatedPayload) is broadcast to the unscoped connections
// only, never scoped onto a recipient's stream.
func (h *Hub) HandleEvent(_ context.Context, evt pkgcore.Event) error {
	if evt.Payload == nil {
		return nil
	}
	payload, err := json.Marshal(evt.Payload)
	if err != nil {
		return nil
	}
	if string(payload) == "null" {
		return nil
	}
	var ann InboxCreatedPayload
	if err := json.Unmarshal(payload, &ann); err != nil {
		h.Publish(payload)
		return nil
	}
	h.PublishFor(payload, ann.TenantID, ann.RecipientUserID)
	return nil
}

// HubConn is one consumer's connection to a Hub, returned by Hub.Subscribe
// or Hub.SubscribeFor. It is the whole consumer surface: read Messages,
// and Close when done. The inbox stream owns one per open SSE connection,
// scoped through SubscribeFor; tests own them here.
type HubConn struct {
	hub *Hub

	// tenantID and recipientUserID are the connection's scope, both empty
	// for a Subscribe connection that takes every announcement (see
	// accepts). Set once at construction and immutable thereafter, so
	// Publish reads them without the hub lock.
	tenantID        string
	recipientUserID string

	msgs chan []byte
	done chan struct{}
	once sync.Once
}

// accepts reports whether one announcement -- identified by the (tenant,
// recipient) pair it was published for, both empty for an unscoped
// publish -- belongs on this connection. An unscoped connection takes
// everything; a scoped connection takes exactly its own pair.
func (c *HubConn) accepts(tenantID, recipientUserID string) bool {
	if c.tenantID == "" && c.recipientUserID == "" {
		return true
	}
	return tenantID != "" && tenantID == c.tenantID &&
		recipientUserID != "" && recipientUserID == c.recipientUserID
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
