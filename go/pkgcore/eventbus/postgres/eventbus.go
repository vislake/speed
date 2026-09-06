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
//     earlier drafts having claimed the stronger guarantee: a row's
//     handlers always run BEFORE that row's delivery is recorded --
//     Publish's own local delivery runs its handlers and then records
//     the row id in an in-process mark (locallyDelivered), while
//     deliverPendingForType's catch-up loop, the only cursor advancer,
//     runs a row's handlers (or skips an already-marked row) before
//     advanceCursorAtLeast persists the watermark past it -- so a crash
//     or connection loss landing in either gap loses no event. A crash
//     after the handlers but before the mark costs nothing (the poller
//     simply delivers the row on its next cycle); a crash after the
//     mark loses the in-process mark itself, so the row is delivered
//     once more after restart -- duplicate, never loss; and a crash in
//     the catch-up loop's own handler-then-advance gap redelivers the
//     row for the same reason. advanceCursorAtLeast retries a bounded
//     number of times over pool (see its own doc comment) precisely to
//     shrink the catch-up side of this window -- most single connection
//     blips now recover inside that call instead of surfacing as a
//     duplicate at all -- but a failure that outlasts every retry still
//     redelivers rather than silently drops. This is the same trade
//     eventbus/redis documents for its own cross-process path
//     ("at-least-once-ish but not retried"): a host whose handlers are
//     not idempotent must de-duplicate itself, by event id or by the
//     payload's own natural key, exactly as it would have to for
//     eventbus/redis.
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

	// deliverMu serializes, for this bus instance alone, each short
	// delivery-critical section against every other one: Publish's
	// per-Type in-flight bookkeeping and local-delivery marks (below) and
	// the catch-up poller's per-batch check of that bookkeeping plus its
	// cursor read and outbox fetch, and its per-row mark check and
	// pruning. The lock is deliberately
	// NEVER held while a handler runs: Publish's own synchronous local
	// delivery and deliverPendingForType's catch-up loop both invoke every
	// handler outside it, which is what makes a handler's re-entrant
	// Publish (or Subscribe) on this same bus safe instead of a
	// self-deadlock.
	//
	// What the lock actually buys is the no-double-delivery argument
	// between the two delivery paths for one (replicaID, event Type)
	// cursor row. Publish raises the Type's in-flight count BEFORE its
	// outbox insert can make a row visible (the insert is that visibility
	// point) and drops it only AFTER its local handlers ran and the row id
	// was recorded in locallyDelivered. The poller's in-flight check,
	// cursor read and fetch share one critical section, so a fetch that
	// sees the count at zero can only run entirely before a concurrent
	// Publish's insert commits, or entirely after that Publish recorded
	// its mark -- it can never catch a committed row in the gap between
	// the row's local handlers running and that delivery being recorded.
	// A locally delivered row therefore always reaches the catch-up scan
	// carrying its mark, and the scan skips it (advancing past it without
	// re-running its handlers) instead of redelivering it; a fetch that
	// races a Publish the other way -- seeing the raised count -- skips
	// the batch entirely and lets that Publish's own local delivery handle
	// its row, which the next catch-up cycle picks up if the publish
	// covered less than the batch would have.
	deliverMu sync.Mutex

	// inFlight counts, per event Type, the Publish calls on this instance
	// whose local delivery has not fully completed yet -- the count is
	// raised before the outbox insert and dropped after the local handlers
	// ran and the row's id was recorded in locallyDelivered (the drop is
	// deferred, so even a panicking handler cannot wedge the poller; a
	// panic also skips the mark, so the poller re-delivers the row rather
	// than the panicked local path silently swallowing it). While the
	// count is non-zero the Type's committed rows are being handled
	// synchronously, or are about to be marked, so the catch-up poller
	// must not fetch them; once it reaches zero, every row that publish
	// committed already carries its mark. Guarded by deliverMu.
	inFlight map[string]int

	// locallyDelivered records, per event Type, the outbox row ids whose
	// handlers this instance's own synchronous Publish path has already
	// run (see Publish's own doc comment). A local Publish never advances
	// the persisted cursor -- only the catch-up poller may move a
	// watermark, and only past rows it has itself delivered or skipped --
	// so a locally delivered row stays visible to the poller's scan, which
	// skips it rather than running its handlers a second time. The marks
	// are in-process by design: the row itself is durable in the outbox,
	// so marks lost to a crash cost a redelivery on restart (at-least-once,
	// never loss -- see the package doc's delivery-semantics note), while
	// a mark at or below the persisted cursor is dead -- that row is never
	// fetched again -- and is pruned by the poller as its own advance
	// passes it (see deliverPendingForType). Keyed by event Type rather
	// than holding a flat id set
	// because pruning is per-Type: ids are allocated from one sequence
	// shared across Types, so advancing one Type's cursor can pass rows of
	// another Type whose own cursor -- and need for its marks -- still
	// lags. Guarded by deliverMu.
	locallyDelivered map[string]map[int64]struct{}

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
		pool:             pool,
		replicaID:        replicaID,
		handlers:         make(map[string][]pkgcore.EventHandler),
		inFlight:         make(map[string]int),
		locallyDelivered: make(map[string]map[int64]struct{}),
		ctx:              ctx,
		cancel:           cancel,
		listenDone:       make(chan struct{}),
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
//
// Local handlers run without deliverMu held (see deliverMu's own doc
// comment), so a handler may itself call Publish -- or Subscribe -- on this
// bus re-entrantly; the nested publish is delivered exactly like any other.
//
// When this instance is locally subscribed to evt.Type, its handlers just
// ran for the row the publish committed, and the row id is recorded in the
// in-process locallyDelivered set instead of advancing the persisted
// cursor: only the catch-up poller may move a watermark, and it skips a
// marked row when its scan reaches it (see deliverPendingForType and
// locallyDelivered). A replica with no local subscriber to evt.Type
// records nothing, and the row reaches that replica only through the
// catch-up scan, as usual.
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

	// Raise this Type's in-flight count BEFORE the insert below can make
	// the row visible to the catch-up poller, and drop it only after the
	// local handlers ran and the row id was recorded in locallyDelivered
	// (the block near the end of this function) -- see deliverMu's own doc
	// comment for why this ordering is the no-double-delivery argument.
	// defer guarantees the count is dropped on every path out of this
	// function, including a handler panic, so a wedged publish can never
	// stall the poller; a panic also skips the mark, leaving the poller to
	// deliver the row rather than the panicked local path swallowing it.
	b.deliverMu.Lock()
	b.inFlight[evt.Type]++
	b.deliverMu.Unlock()
	defer func() {
		b.deliverMu.Lock()
		b.inFlight[evt.Type]--
		if b.inFlight[evt.Type] == 0 {
			delete(b.inFlight, evt.Type)
		}
		b.deliverMu.Unlock()
	}()

	id, err := insertOutboxAndNotify(ctx, b.pool, evt.Type, string(evt.TenantID), payload)
	if err != nil {
		return fmt.Errorf("pkgcore/eventbus/postgres: publish event %q: %w", evt.Type, err)
	}

	// Local handlers run WITHOUT deliverMu held (the in-flight count above
	// is what keeps the catch-up poller from racing them), synchronously
	// and in registration order on this goroutine, exactly as on the
	// in-memory bus. A handler's own re-entrant Publish on this bus takes
	// deliverMu only for its brief in-flight bookkeeping, never blocking
	// on this call's lock, because this call no longer holds it.
	handlers := b.handlersFor(evt.Type)
	failures := make([]error, 0, len(handlers))
	for i, h := range handlers {
		if err := h(ctx, evt); err != nil {
			failures = append(failures, fmt.Errorf("pkgcore/eventbus/postgres: handler %d for event %q failed: %w", i, evt.Type, err))
		}
	}
	if len(handlers) > 0 {
		// This replica is locally subscribed to evt.Type, so its own
		// handlers just ran for this row synchronously. Record the row id
		// in the in-process locallyDelivered set instead of advancing the
		// persisted cursor: only the catch-up poller may move a watermark,
		// and a local advance could leap over same-type rows that are
		// committed but not yet picked up by this replica's own (lagging)
		// listener -- the loss shape this package's no-loss integration
		// tests pin. The mark tells the poller's scan to skip this row
		// rather than run its handlers again. The mark must land BEFORE
		// the deferred in-flight drop above runs (it executes as this
		// function returns), so any fetch the drop unblocks can only ever
		// see an already-marked row -- the ordering deliverMu's own doc
		// comment's no-double-delivery argument depends on. A replica with
		// no local subscriber to evt.Type records nothing: the row simply
		// waits in the outbox for whichever OTHER replica (or a later
		// Subscribe on this one) discovers it.
		b.deliverMu.Lock()
		marked := b.locallyDelivered[evt.Type]
		if marked == nil {
			marked = make(map[int64]struct{})
			b.locallyDelivered[evt.Type] = marked
		}
		marked[id] = struct{}{}
		b.deliverMu.Unlock()
	}
	return errors.Join(failures...)
}

// handlersFor returns a private snapshot of the handlers subscribed to
// eventType, copied under the lock exactly like eventbus/redis's own
// handlersFor. The snapshot is what lets a handler call Subscribe
// re-entrantly without mutating the slice being iterated; calling Publish
// re-entrantly is equally safe because no handler ever runs under deliverMu
// (see deliverMu's own doc comment).
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
// instance is currently subscribed to. No lock spans the whole scan: each
// Type's scan takes the brief deliverMu critical section it needs for
// itself (see deliverPendingForType and deliverMu's own doc comment) and
// runs every handler it delivers to outside deliverMu, so a handler may
// Publish (or Subscribe) on this same bus re-entrantly even while a
// catch-up delivery is in progress.
func (b *EventBus) deliverPending(ctx context.Context) {
	types := b.subscribedTypes()
	if len(types) == 0 {
		return
	}

	for _, eventType := range types {
		b.deliverPendingForType(ctx, eventType)
	}
}

// deliverPendingForType delivers every outbox row of eventType newer than
// this replica's persisted cursor for it, in batches of catchUpBatchSize,
// initializing the cursor at the live end on the very first call
// (ensureCursor) and re-reading it from the database before every later
// batch. This scan is the ONLY cursor advancer in the process: Publish's
// local delivery never touches the watermark -- it records in-process
// marks instead (see Publish and locallyDelivered) -- so no local publish
// can ever move the cursor past rows the scan has not handled yet.
//
// Each batch is fetched inside one brief deliverMu critical section: the
// section first skips the Type entirely when a Publish on this same
// instance is delivering it locally right now -- see deliverMu's own doc
// comment for why that check, the cursor read and the fetch must share
// the section to rule out double delivery -- then reads the cursor and
// fetches the batch. Every handler runs outside deliverMu
// (deliverOutboxRow). Rows are then handled one at a time: a row whose id
// the local path already marked is skipped -- that mark IS this instance's
// synchronous delivery of the row, so running its handlers again would
// duplicate it -- while an unmarked row is delivered normally. Every row,
// marked or not, is then advanced past, outside deliverMu; on a failure
// the scan stops and is retried from the unadvanced persisted cursor on
// the next call (at-least-once; a marked row is no worse for it, since
// the next scan skips it again rather than redelivering it -- see the
// package doc's delivery-semantics note). After each successful advance
// the scan prunes, under deliverMu, every mark of THIS type at or below
// the advanced id: those rows now sit behind the cursor and will never be
// fetched again, so their marks are dead. Pruning is deliberately
// per-Type -- a mark of another Type is never touched, because that
// Type's own cursor may still lag behind ids this advance just passed
// (ids come from one sequence shared across Types; see locallyDelivered's
// doc comment) -- and runs only after the advance succeeded, so a failed
// advance never discards marks the next scan still needs. A failure at
// any point here is swallowed rather than propagated -- there is no
// caller left to report it to, the same "handlers on other replicas ...
// errors are not observable by any publisher" contract eventbus/redis
// documents for its own remote delivery path -- and simply retried on the
// next call.
func (b *EventBus) deliverPendingForType(ctx context.Context, eventType string) {
	cursorKnown := false
	for {
		// One critical section per batch: the in-flight check, the cursor
		// read and the fetch must be atomic against Publish's own
		// bookkeeping for the no-double-delivery argument to hold (see
		// deliverMu's own doc comment), so deliverMu stays held across
		// them but is released before any handler runs.
		b.deliverMu.Lock()
		if b.inFlight[eventType] > 0 {
			// A local Publish on this instance is delivering eventType
			// synchronously right now and will mark every row it covers
			// before its in-flight count drops. Anything this scan would
			// have fetched is either already being handled there or not
			// yet committed, so skip the whole batch: the next catch-up
			// cycle (a NOTIFY or the next listenBlock timeout) delivers
			// what that Publish did not, and delivery stays at-least-once.
			b.deliverMu.Unlock()
			return
		}
		var (
			cursor int64
			err    error
		)
		if cursorKnown {
			// Re-read, never reuse the previous batch's value: an earlier
			// batch of this same scan (or a second instance mistakenly
			// sharing this replicaID) may have advanced the cursor since,
			// and fetching from a stale position would redeliver the rows
			// already handled.
			cursor, err = readCursor(ctx, b.pool, b.replicaID, eventType)
		} else {
			cursor, err = ensureCursor(ctx, b.pool, b.replicaID, eventType)
			if err == nil {
				cursorKnown = true
			}
		}
		if err != nil {
			b.deliverMu.Unlock()
			return
		}
		rows, err := fetchOutboxSince(ctx, b.pool, eventType, cursor)
		b.deliverMu.Unlock()
		if err != nil || len(rows) == 0 {
			return
		}
		for _, row := range rows {
			// A brief deliverMu section per row answers the skip question:
			// whether THIS instance's own Publish path already ran this
			// row's handlers (see locallyDelivered). Reading under the
			// lock synchronizes against the concurrent mark and prune
			// writers; the answer itself cannot change underneath this
			// scan, because a row fetched here was committed before this
			// batch's snapshot, so any local publish of it completed its
			// mark before this batch's critical section began -- the
			// argument deliverMu's own doc comment makes.
			b.deliverMu.Lock()
			_, alreadyLocal := b.locallyDelivered[eventType][row.id]
			b.deliverMu.Unlock()

			if !alreadyLocal {
				b.deliverOutboxRow(ctx, row)
			}
			if err := advanceCursorAtLeast(ctx, b.pool, b.replicaID, eventType, row.id); err != nil {
				return // retried from the (unadvanced) persisted cursor next call
			}
			// The advance succeeded, so every mark of this type at or
			// below row.id is dead -- those rows sit behind the cursor and
			// will never be fetched again -- and is pruned here: per-Type,
			// and only after the advance succeeds, for the reasons in the
			// doc comment above.
			b.deliverMu.Lock()
			for markedID := range b.locallyDelivered[eventType] {
				if markedID <= row.id {
					delete(b.locallyDelivered[eventType], markedID)
				}
			}
			if len(b.locallyDelivered[eventType]) == 0 {
				delete(b.locallyDelivered, eventType)
			}
			b.deliverMu.Unlock()
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
