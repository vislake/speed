// Package postgres is a distributed deployment mode's pkgcore.EventBus,
// delivering events between the replicas of a deployment through
// PostgreSQL LISTEN/NOTIFY, backed by a durable outbox table for the
// replay guarantee LISTEN/NOTIFY alone cannot give. It exists for a
// deployment that already runs PostgreSQL (go/dbkit's own supported
// dialect) and does not want to stand up Redis or NATS purely to satisfy
// this one seam -- the genuinely zero-extra-infrastructure distributed
// EventBus option, alongside eventbus/redis's Redis Streams
// implementation.
//
// It is split out of go/pkgcore's own package for the identical reason
// eventbus/redis is: a consumer which never wires a PostgreSQL-backed bus
// must not inherit jackc/pgx/v5 in its dependency graph (docs/internal/03-
// deployment-modes.md's implementation-registry section measures the cost
// and names the boundary; this package's own AGENTS.md entry records the
// measured number for pgx specifically).
//
// Importing this package registers "eventbus.postgres" on pkgcore's shared
// EventBusRegistry as a side effect (see register.go) -- the same
// database/sql-style driver-registration pattern eventbus/redis follows,
// applied a second time for a second implementation of the same seam. It
// is not named by pkgcore.PresetDistributed, which still points
// "eventbus" at "eventbus.redis"; a host that wants this implementation
// instead builds its own Preset (a plain map literal, see Preset's own doc
// comment) or bypasses the preset layer entirely by constructing NewEventBus
// and wiring it with pkgcore.WithEventBus, exactly as a host choosing
// eventbus/redis explicitly does today.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vislake/speed/go/pkgcore"
)

// ErrEventBusClosed is returned by Publish on an EventBus after Close, and
// mirrors eventbus/redis.ErrEventBusClosed exactly: closing an EventBus
// only has meaning for an implementation with something to stop, here the
// background listener goroutine Subscribe starts.
var ErrEventBusClosed = errors.New("pkgcore/eventbus/postgres: event bus is closed")

const (
	// pgEventChannel is the single PostgreSQL NOTIFY channel every bus
	// instance LISTENs on, regardless of how many event Types it
	// subscribes to: NOTIFY is a broadcast primitive already, so one
	// channel shared by every instance sharing a database is what makes
	// fan-out to every replica free -- see EventBus's own doc comment for
	// why a shared channel does not mean an event reaches a handler
	// subscribed to a different Type. Every reader treats a notification
	// as nothing more than "something changed, go poll the outbox now";
	// no notification payload is required for correctness, so a fixed,
	// non-caller-controlled constant here carries no collision risk the
	// way eventbus/redis's eventStreamPrefix must guard against for
	// arbitrary event-Type-derived keys.
	pgEventChannel = "pkgcore_eventbus_notify"

	// listenBlock bounds how long each WaitForNotification call waits
	// before this package treats the wake as a plain timeout and polls the
	// outbox anyway. It is this implementation's counterpart of
	// eventbus/redis's eventReaderBlock: a short block keeps Close
	// prompt, and doubles as the periodic anti-loss poll interval described
	// in this package's own AGENTS.md entry (go/config's "anti-loss poller
	// behind the events" is the precedent both share) -- there is no
	// separate ticker, because a WaitForNotification that times out is
	// itself the tick.
	listenBlock = 2 * time.Second

	// reconnectDelay backs off a failed attempt to open or LISTEN on the
	// dedicated connection before retrying, mirroring eventbus/redis's
	// eventRetryDelay.
	reconnectDelay = 200 * time.Millisecond
)

// EventBus is the distributed deployment mode's pkgcore.EventBus,
// delivering events between replicas through the PostgreSQL database pool
// gives them shared access to. A distributed-mode host passes one pool for
// the whole deployment to NewEventBus, together with a name -- replicaID --
// that is stable across that same replica's restarts, and keeps owning the
// pool; the bus never closes it.
//
// # How delivery works
//
// Publish writes one row to pkgcore_eventbus_outbox and issues NOTIFY for
// it inside a single transaction (see insertOutboxAndNotify) -- the commit
// point every replica's catch-up scan can find even if it never receives
// the NOTIFY itself -- and then invokes this instance's own subscribers
// synchronously, in registration order, exactly like pkgcore's in-memory
// bus and eventbus/redis's local-delivery path. Every bus instance sharing
// the pool's database -- this one included -- runs one background reader
// that LISTENs on the shared pgEventChannel and, on every notification (or
// simply every listenBlock timeout, whichever comes first), scans the
// outbox for rows newer than its own persisted watermark, per event Type it
// is locally subscribed to, and delivers them the same way. Every event a
// Publish's transaction commits therefore reaches every subscribed handler
// of every replica AT LEAST once per replica: this is fan-out, not
// load-balancing -- see the delivery-semantics note below for how that was
// determined, why it is at-least-once rather than exactly-once, and why
// that matters.
//
// # Delivery-semantics note (read before choosing this bus)
//
// This implementation was built to match eventbus/redis.EventBus's own
// real, tested cross-replica delivery guarantee exactly, determined from
// two sources: eventbus/redis's own doc comment ("every replica runs a
// per-type reader goroutine ... so each event is delivered to every
// replica exactly once") and go/notification's Redis integration leg
// (TestRedisBus_DeliveredInbox_AnnouncesAcrossReplicas), which boots two
// replicas sharing one Redis client, subscribes both, and asserts the
// SECOND replica's own spy handler receives the event the first replica's
// real delivery pipeline published -- fan-out, not "whichever replica's
// reader happened to claim it". This bus reproduces that shape: two
// EventBus instances built over the same pool, each Subscribed to the same
// Type, both receive every Publish, matching PostgreSQL NOTIFY's own
// native behavior (every LISTENing session receives every matching
// notification) rather than fighting it toward a load-balanced contract
// NOTIFY cannot give for free.
//
// The differences from a purely synchronous bus are deliberate, not
// accidental, and mostly mirror eventbus/redis's own:
//
//   - Cross-process delivery is asynchronous: when Publish returns, other
//     replicas may not have run their handlers yet. A handler's error on
//     the catch-up path is not observable by any publisher -- there is no
//     publisher left in process to receive it -- and a panic inside one is
//     recovered so a buggy handler cannot take down a replica's reader
//     goroutine (see deliverOutboxRow).
//   - Payloads cross the process boundary as JSON, stored as the outbox
//     row's payload column: a struct becomes a map[string]any on every
//     replica except the one that published it, which -- like
//     eventbus/redis -- receives the original Go value untouched through
//     its own synchronous local-delivery path.
//   - Handlers on other replicas run on the bus's own background context,
//     which carries no tenant; a handler needing tenant data must rebuild
//     it from the event with pkgcore.WithTenant.
//   - Delivery is at-least-once, not exactly-once, despite this package's
//     earlier drafts having claimed the stronger guarantee: both
//     deliverPendingForType's catch-up loop and Publish's own local
//     delivery run a row's handlers BEFORE persisting that the row was
//     delivered (advanceCursorAtLeast), so that a crash or connection loss
//     landing in the gap between the two loses no event -- the unadvanced,
//     persisted cursor simply causes the next catch-up cycle to redeliver
//     the same row. advanceCursorAtLeast retries a bounded number of times
//     over pool (see its own doc comment) precisely to shrink this window
//     -- most single connection blips now recover inside that call instead
//     of surfacing as a duplicate at all -- but a failure that outlasts
//     every retry still redelivers rather than silently drops. This is the
//     same trade eventbus/redis documents for its own cross-process path
//     ("at-least-once-ish but not retried"): a host whose handlers are not
//     idempotent must de-duplicate itself, by event id or by the payload's
//     own natural key, exactly as it would have to for eventbus/redis.
//   - Unlike eventbus/redis, THIS implementation genuinely survives a
//     replica's own restart without losing events published while it was
//     down, provided the restarting process is built with the SAME
//     replicaID it used before: the watermark pkgcore_eventbus_cursor
//     persists per (replicaID, event Type) in the database itself, not in
//     an ephemeral per-process instance id the way eventbus/redis's
//     consumer groups do (its own doc comment: "a replica that subscribes
//     late does not catch up ... the consumer group starts at the live
//     end of the stream"). A first-ever Subscribe of a (replicaID, Type)
//     pair still starts at the live end -- exactly like eventbus/redis's
//     "$" -- but every Subscribe after that, on a NEW process instance
//     using the same replicaID, resumes exactly where the previous process
//     left off. Two instances built with the SAME replicaID over the same
//     pool are not two independent replicas by this bus's contract: they
//     share one cursor and race each other's advances, so replicaID must
//     be unique per logical replica, the same way eventbus/redis's random
//     instanceID is (there it is generated for you; here the host supplies
//     it, precisely so it can be stable across a restart).
//   - The outbox table has no automatic retention the way eventbus/redis's
//     stream trims itself: PurgeOutboxBefore is this package's explicit,
//     host-scheduled counterpart, and a replica that stays disconnected
//     longer than a host's purge window loses whatever aged out of it, the
//     same trade eventbus/redis's own trim window makes.
//
// The bus is safe for concurrent use by multiple goroutines. Publish after
// Close returns ErrEventBusClosed, and Subscribe after Close is a no-op;
// both are programming errors, detected rather than silently accepted.
type EventBus struct {
	pool      *pgxpool.Pool
	replicaID string

	mu       sync.RWMutex
	closed   bool
	handlers map[string][]pkgcore.EventHandler

	// deliverMu serializes every delivery-and-cursor-advance sequence
	// against every other one on this instance -- Publish's own
	// synchronous local delivery and the listener goroutine's periodic
	// catch-up scan alike -- so no two of them can race the same event
	// Type's cursor row. See ensureCursor's and advanceCursorAtLeast's own
	// doc comments for what this buys.
	deliverMu sync.Mutex

	ctx    context.Context
	cancel context.CancelFunc

	listenStarted atomic.Bool
	listenOnce    sync.Once
	listenDone    chan struct{}
}

// NewEventBus returns a pkgcore.EventBus that delivers between replicas
// through the given PostgreSQL connection pool: events published on any
// bus sharing pool's database reach the subscribers of every bus built
// over it, including across a replica's own restart -- see EventBus's own
// doc comment for the exact guarantee and the role replicaID plays in it.
//
// replicaID must be non-empty and stable across this logical replica's
// restarts (a hostname, a pod name, a configured identifier -- anything
// the host already has that survives a redeploy); NewEventBus panics on an
// empty one, since a wiring mistake this fundamental to the durability
// guarantee is better caught at startup than silently degrading it. Two
// bus instances sharing a pool must use different replicaIDs, or they
// share one cursor and corrupt each other's watermark.
//
// The schema (pkgcore_eventbus_outbox, pkgcore_eventbus_cursor) is not
// created here: call EnsureSchema against pool first (see its own doc
// comment for why this is a separate step). The returned bus starts no
// goroutine and opens no connection beyond pool until the first Subscribe,
// and its listener stops at Close. A nil pool panics, the same
// unrecoverable-wiring-error treatment eventbus/redis.NewEventBus gives a
// nil client.
func NewEventBus(pool *pgxpool.Pool, replicaID string) *EventBus {
	if pool == nil {
		panic("pkgcore/eventbus/postgres: NewEventBus requires a non-nil *pgxpool.Pool")
	}
	if replicaID == "" {
		panic("pkgcore/eventbus/postgres: NewEventBus requires a non-empty replicaID")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &EventBus{
		pool:       pool,
		replicaID:  replicaID,
		handlers:   make(map[string][]pkgcore.EventHandler),
		ctx:        ctx,
		cancel:     cancel,
		listenDone: make(chan struct{}),
	}
}

// Close stops the bus: Publish fails with ErrEventBusClosed from here on,
// and the listener goroutine (if Subscribe ever started one) shuts down
// within one listenBlock. The pool the bus was built on stays open; the
// host owns it, exactly as eventbus/redis.EventBus.Close leaves its client
// open. Close is idempotent and safe for concurrent use. A closed bus must
// not be used again.
func (b *EventBus) Close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.cancel()
	if b.listenStarted.Load() {
		<-b.listenDone
	}
}

// isClosed reports whether Close has been called.
func (b *EventBus) isClosed() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.closed
}

// Subscribe registers h for the exact event type eventType, mirroring
// pkgcore's in-memory bus and eventbus/redis: several handlers may
// subscribe to the same type, all of them are invoked in registration
// order, and a nil handler is ignored. The first Subscribe call on this
// instance, of any type, starts the background listener goroutine that
// delivers both live notifications and anything this replica's cursor
// shows it has missed.
func (b *EventBus) Subscribe(eventType string, h pkgcore.EventHandler) {
	if h == nil {
		return
	}
	if b.isClosed() {
		return
	}

	b.mu.Lock()
	b.handlers[eventType] = append(b.handlers[eventType], h)
	b.mu.Unlock()

	b.listenOnce.Do(func() {
		b.listenStarted.Store(true)
		go b.run()
	})
}

// Publish delivers evt to every handler subscribed to evt.Type on every bus
// sharing the pool's database. The event is first durably recorded and
// announced by insertOutboxAndNotify -- the commit point every replica's
// catch-up scan can find -- and this instance's own subscribers then run
// synchronously, in registration order, exactly as on the in-memory bus,
// their failures collected into one joined error. A failed write delivers
// nothing anywhere and reports the failure; handlers of this instance do
// not run for an event that never made it into the outbox, so a publish is
// all-or-nothing across the deployment, exactly like eventbus/redis's own
// Publish contract.
//
// The payload must survive JSON encoding, because that is how it crosses
// the process boundary through the outbox's payload column; a payload that
// does not (channels, funcs) fails the publish before anything is written
// or delivered.
func (b *EventBus) Publish(ctx context.Context, evt pkgcore.Event) error {
	if b.isClosed() {
		return ErrEventBusClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	payload, err := json.Marshal(evt.Payload)
	if err != nil {
		return fmt.Errorf("pkgcore/eventbus/postgres: payload of event %q is not JSON-serializable: %w", evt.Type, err)
	}

	id, err := insertOutboxAndNotify(ctx, b.pool, evt.Type, string(evt.TenantID), payload)
	if err != nil {
		return fmt.Errorf("pkgcore/eventbus/postgres: publish event %q: %w", evt.Type, err)
	}

	b.deliverMu.Lock()
	defer b.deliverMu.Unlock()

	handlers := b.handlersFor(evt.Type)
	failures := make([]error, 0, len(handlers))
	for i, h := range handlers {
		if err := h(ctx, evt); err != nil {
			failures = append(failures, fmt.Errorf("pkgcore/eventbus/postgres: handler %d for event %q failed: %w", i, evt.Type, err))
		}
	}
	if len(handlers) > 0 {
		// This replica is locally subscribed to evt.Type, so its own
		// handlers just ran for this row synchronously; advancing the
		// cursor to cover it keeps the catch-up poller from redelivering
		// the very event this call already handled. A replica with no
		// local subscriber to evt.Type has no cursor to advance -- the row
		// simply waits in the outbox for whichever OTHER replica (or a
		// later Subscribe on this one) discovers it.
		if err := advanceCursorAtLeast(ctx, b.pool, b.replicaID, evt.Type, id); err != nil {
			failures = append(failures, fmt.Errorf("pkgcore/eventbus/postgres: advance local cursor for event %q: %w", evt.Type, err))
		}
	}
	return errors.Join(failures...)
}

// handlersFor returns a private snapshot of the handlers subscribed to
// eventType, copied under the lock exactly like eventbus/redis's own
// handlersFor, so a handler is free to call Subscribe or Publish
// re-entrantly and a concurrent Subscribe cannot mutate the slice being
// iterated.
func (b *EventBus) handlersFor(eventType string) []pkgcore.EventHandler {
	b.mu.RLock()
	defer b.mu.RUnlock()

	registered := b.handlers[eventType]
	if len(registered) == 0 {
		return nil
	}
	snapshot := make([]pkgcore.EventHandler, len(registered))
	copy(snapshot, registered)
	return snapshot
}

// subscribedTypes returns a private snapshot of every event Type this
// instance currently has at least one handler for, for the listener
// goroutine's catch-up scan to iterate.
func (b *EventBus) subscribedTypes() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	types := make([]string, 0, len(b.handlers))
	for eventType := range b.handlers {
		types = append(types, eventType)
	}
	return types
}

// run is the background listener goroutine Subscribe starts at most once:
// it holds a dedicated PostgreSQL connection -- opened independently of
// pool, never acquired from it (see connectListener) -- for LISTEN, and
// keeps reconnecting for as long as the bus is open. This is the "raw
// driver-level connection outside GORM's own pool" this package's task
// asked for translated into pgx's own idiom: go/dbkit's precedent (see
// go/dbkit/audit's append-only bypass tests) is a dedicated *sql.Conn
// pulled out of an existing *sql.DB's pool; this package has no *sql.DB or
// *gorm.DB at all (go/pkgcore cannot import go/dbkit -- see root CLAUDE.md's
// module dependency direction), so its own dedicated connection is a wholly
// independent *pgx.Conn dialed from pool's own connection config, rather
// than one borrowed from and never returned to pool -- simpler, and with
// no risk of leaving a LISTENing session inside a pool other queries might
// later acquire.
func (b *EventBus) run() {
	defer close(b.listenDone)
	for {
		if b.ctx.Err() != nil {
			return
		}
		conn, err := b.connectListener(b.ctx)
		if err != nil {
			return // ctx was cancelled while retrying: Close is in progress
		}
		b.readLoop(conn)
		_ = conn.Close(context.Background())
	}
}

// connectListener dials a new, independent *pgx.Conn from pool's own
// connection config and issues LISTEN on pgEventChannel, retrying with
// reconnectDelay between attempts until it succeeds or the bus is closed.
// It never touches pool itself: a connection this function returns is
// never shared with, or returned to, any pool.
func (b *EventBus) connectListener(ctx context.Context) (*pgx.Conn, error) {
	connConfig := b.pool.Config().ConnConfig.Copy()
	for {
		conn, err := pgx.ConnectConfig(ctx, connConfig)
		if err == nil {
			if _, listenErr := conn.Exec(ctx, "LISTEN "+pgEventChannel); listenErr == nil {
				return conn, nil
			}
			_ = conn.Close(context.Background())
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(reconnectDelay):
		}
	}
}

// readLoop drives one connected listener session until it drops or the bus
// is closed: it first runs a catch-up scan (covering whatever this replica
// missed while it had no listener at all -- the connection-establishment
// race and the genuine-restart window both this package's task calls out),
// then alternates between waiting for the next notification and scanning
// again, whichever comes first. A notification's own payload is never
// inspected: it is nothing more than "something changed, go check", so a
// timeout and a real notification are handled identically. It returns,
// letting run's outer loop reconnect, on any connection-level error other
// than the bus being closed.
func (b *EventBus) readLoop(conn *pgx.Conn) {
	b.deliverPending(b.ctx)

	for {
		waitCtx, cancel := context.WithTimeout(b.ctx, listenBlock)
		_, err := conn.WaitForNotification(waitCtx)
		cancel()

		if b.ctx.Err() != nil {
			return
		}
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				b.deliverPending(b.ctx) // periodic anti-loss poll: no notification arrived, check anyway
				continue
			}
			return // a real connection-level error: run() reconnects
		}
		b.deliverPending(b.ctx) // notified: something changed, catch up
	}
}

// deliverPending runs deliverPendingForType for every event Type this
// instance is currently subscribed to, serialized against Publish's own
// local delivery by deliverMu.
func (b *EventBus) deliverPending(ctx context.Context) {
	types := b.subscribedTypes()
	if len(types) == 0 {
		return
	}

	b.deliverMu.Lock()
	defer b.deliverMu.Unlock()
	for _, eventType := range types {
		b.deliverPendingForType(ctx, eventType)
	}
}

// deliverPendingForType reads this replica's persisted cursor for
// eventType, initializing it at the live end on the very first call
// (ensureCursor), and delivers every outbox row newer than it in batches
// of catchUpBatchSize, advancing the cursor after each row. A failure at
// any point here is swallowed rather than propagated -- there is no
// caller left to report it to, the same "handlers on other replicas ...
// errors are not observable by any publisher" contract eventbus/redis
// documents for its own remote delivery path -- and simply retried on the
// next call.
func (b *EventBus) deliverPendingForType(ctx context.Context, eventType string) {
	cursor, err := ensureCursor(ctx, b.pool, b.replicaID, eventType)
	if err != nil {
		return
	}

	for {
		rows, err := fetchOutboxSince(ctx, b.pool, eventType, cursor)
		if err != nil || len(rows) == 0 {
			return
		}
		for _, row := range rows {
			b.deliverOutboxRow(ctx, row)
			cursor = row.id
			if err := advanceCursorAtLeast(ctx, b.pool, b.replicaID, eventType, row.id); err != nil {
				return // retried from the (unadvanced) persisted cursor next call
			}
		}
		if len(rows) < catchUpBatchSize {
			return
		}
	}
}

// deliverOutboxRow decodes one outbox row's JSON payload and hands it to
// every handler currently subscribed to its event Type, recovering a panic
// from any one of them so a buggy handler cannot take down the listener
// goroutine -- mirroring eventbus/redis.EventBus.runRemoteHandler exactly,
// including dropping the handler's returned error: there is no publisher
// left in process for this event to report it to.
func (b *EventBus) deliverOutboxRow(ctx context.Context, row outboxRow) {
	var payload interface{}
	if err := json.Unmarshal(row.payload, &payload); err != nil {
		// A row that does not decode is corrupt (or hand-written); it must
		// not wedge the reader, so it is skipped -- the cursor still
		// advances past it in the caller.
		return
	}
	evt := pkgcore.Event{Type: row.eventType, TenantID: pkgcore.TenantID(row.tenantID), Payload: payload}

	for _, h := range b.handlersFor(row.eventType) {
		b.runHandlerRecovered(ctx, evt, h)
	}
}

// runHandlerRecovered invokes one handler for a catch-up-delivered event,
// containing any panic it raises.
func (b *EventBus) runHandlerRecovered(ctx context.Context, evt pkgcore.Event, h pkgcore.EventHandler) {
	defer func() {
		_ = recover()
	}()
	_ = h(ctx, evt)
}
