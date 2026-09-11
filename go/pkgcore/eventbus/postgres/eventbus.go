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
// must not inherit jackc/pgx/v5 in its dependency graph; its measured
// dependency cost is recorded in its package documentation.
//
// Importing this package registers "eventbus.postgres" on pkgcore's shared
// component as a side effect (see component.go) -- the same
// database/sql-style driver-registration pattern eventbus/redis follows,
// applied a second time for a second implementation of the same seam. It
// is not the "eventbus" component the built-in composition names, which still points
// "eventbus" at "eventbus.redis"; a host that wants this implementation
// instead points the "eventbus" module at "eventbus.postgres" in its own
// composition, or bypasses the loader entirely by constructing NewEventBus
// and providing it through the by-type context, exactly as a host choosing
// eventbus/redis explicitly does.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	// prompt, and doubles as the periodic anti-loss poll interval -- there is no
	// separate ticker, because a WaitForNotification that times out is
	// itself the tick.
	listenBlock = 2 * time.Second

	// reconnectDelay backs off a failed attempt to open or LISTEN on the
	// dedicated connection before retrying, mirroring eventbus/redis's
	// eventRetryDelay.
	reconnectDelay = 200 * time.Millisecond

	// maxPanickedRowAttempts bounds the total number of times one outbox
	// row's panicking handlers are invoked -- the first, catch-up delivery
	// included -- before the row settles with a terminal log line (see
	// retryPanickedRows). It is what keeps a permanently panicking handler
	// from becoming an unbounded redelivery loop: the bounded retry runs at
	// the listener's own wake cadence, so even a type with no further
	// traffic exhausts the budget within a few wake cycles and then stops.
	maxPanickedRowAttempts = 4

	// panicRetryDelay is the minimum spacing between two retry rounds of
	// the same row's panicked handlers, so a burst of notifications -- each
	// of which wakes the listener and would otherwise retry every recorded
	// row -- cannot burn a row's whole budget in milliseconds before a
	// transient panic has had a chance to clear.
	panicRetryDelay = time.Second
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
//     shrink the catch-up side of this window -- most single-connection
//     blips recover inside that call instead of surfacing as a duplicate at
//     all. A failure that outlasts every retry still redelivers rather than
//     silently drops. One exception to the
//     never-loss half of that argument exists, and it is the price of
//     the wedge property below: a row whose LOCAL delivery is still in
//     progress when the catch-up scan passes it (the scan skips rows
//     named in the in-flight set rather than re-running handlers that
//     are already running, and its advance cannot stay behind one row
//     while delivering later ones) is never fetched again, so a crash
//     landing in that delivery after the pass is not redelivered on
//     restart (a panicked local handler no longer has this hole: it is
//     contained and the row is marked like any other -- see Publish's
//     defer comment). Rows whose
//     local delivery is not in progress keep the argument in full.
//     This is the same trade
//     eventbus/redis documents for its own cross-process path
//     ("at-least-once-ish but not retried"): a host whose handlers are
//     not idempotent must de-duplicate itself, by event id or by the
//     payload's own natural key, exactly as it would have to for
//     eventbus/redis.
//   - A local handler that never returns (a wedged handler) stalls
//     nothing but its own row. Publish's in-flight bookkeeping spans the
//     synchronous handler loop, but the catch-up poller's gate only
//     withholds a Type while a raised Publish has not yet recorded its
//     row's id -- the insert's own round trip, nothing more. A wedged
//     handler's row id is long since recorded, so the scan proceeds and
//     skips exactly that row -- which is being delivered right now, on
//     the publishing goroutine -- while every other row of the Type,
//     committed by any replica, keeps being delivered and advanced past
//     on the ordinary schedule. The wedged row itself is advanced past
//     too, because one monotone watermark cannot both deliver later
//     rows and stay behind an earlier one; its handlers keep running for
//     as long as the publishing process lives, and the never-loss
//     exception above is what a crash of that delivery means afterwards.
//     This mirrors the wedge every broker-backed bus documents for a
//     handler that never acknowledges, with the difference that here the
//     wedge cannot take the Type's other rows down with it.
//   - A handler that panics while delivering a row is recovered and
//     logged, never fatal to the reader goroutine -- and it never wedges
//     its Type and never re-runs its siblings. The catch-up scan advances
//     the cursor past the panicked row exactly as for a clean delivery,
//     so later rows of the Type keep flowing to every healthy handler,
//     and the still-panicking handler values are recorded for an
//     in-process retry that re-invokes ONLY those values, spaced at least
//     panicRetryDelay apart and capped at maxPanickedRowAttempts
//     attempts, after which the row settles with a terminal log line
//     rather than an unbounded redelivery loop (see retryPanickedRows).
//     Healthy sibling handlers therefore run exactly once even while
//     their sibling panics on every attempt. The broker-backed twins
//     reach comparable ends by their own mechanisms: eventbus/redis's
//     reader consumes only new entries (a ">" cursor), so an unacked
//     panicked entry is never refetched -- no retry, no stall, no sibling
//     re-run; it simply sits pending, visible to an operator -- while
//     eventbus/nats negatively acknowledges the message and JetStream
//     redelivers it at panicRedeliveryDelay intervals, each redelivery
//     re-invoking only the panicked handler values (never the message's
//     whole fan-out) until the eventMaxDeliver budget is exhausted, when
//     the message settles with a terminal log line (see that package's
//     deliverRemote). Each backend therefore bounds a panicked delivery's
//     redelivery by the mechanism that drives it: this one's retry pass is
//     self-driven by the scan, so it needed its own attempt budget, while
//     the broker-backed twins' redeliveries are bounded broker- and
//     ledger-side. This whole treatment belongs to deliveries that run
//     OUTSIDE any publisher's stack -- the catch-up path here, the reader
//     goroutines of the twins. A row whose LOCAL delivery panics gets the
//     in-memory bus's treatment instead: Publish contains the panic
//     (runLocalHandler recovers and logs it, the row is marked delivered
//     like any other), so a buggy subscriber can never unwind the
//     publishing caller -- the same containment the twins' local fan-out
//     now applies and pkgcore's own in-memory bus always had.
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
	// cursor read and outbox fetch, and its per-row check and pruning.
	// The lock is deliberately NEVER held while a handler runs: Publish's
	// own synchronous local delivery and deliverPendingForType's catch-up
	// loop both invoke every handler outside it, which is what makes a
	// handler's re-entrant Publish (or Subscribe) on this same bus safe
	// instead of a self-deadlock.
	//
	// What the lock actually buys is the no-double-delivery argument
	// between the two delivery paths for one (replicaID, event Type)
	// cursor row. Publish raises the Type's in-flight count BEFORE its
	// outbox insert can make a row visible (the insert is that visibility
	// point) and records the committed row's id in inFlightRows once the
	// insert has returned and the publish has handlers to run, dropping
	// the count and the id only when Publish returns -- after the local
	// handlers ran and the row id was recorded in locallyDelivered. The
	// poller's in-flight check, cursor read and fetch share one critical
	// section, so a fetch that passes the check can only run entirely
	// before a concurrent Publish's insert commits, or entirely after that
	// Publish recorded its mark, or -- the case the per-row skip exists
	// for -- while that Publish's handlers are running, with the row's id
	// sitting in inFlightRows so the scan skips exactly that row instead
	// of delivering it a second time. The one gap the per-row record
	// cannot cover is a row committed while its publisher's insert has not
	// returned yet: the id is unknown until it does, so the row would be
	// visible and yet unidentifiable. That gap is what the count closes --
	// while it exceeds the number of recorded ids, at least one such
	// publish is out, and the check withholds the Type. A locally
	// delivered row therefore always reaches the catch-up scan carrying
	// its mark (or an in-flight record the scan skips), and the scan
	// advances past it without re-running its handlers; a fetch that
	// races a Publish the other way -- seeing a raised count with
	// unrecorded ids -- skips the batch entirely and lets that Publish's
	// own local delivery handle its row, which the next catch-up cycle
	// picks up if the publish covered less than the batch would have.
	deliverMu sync.Mutex

	// inFlight counts, per event Type, this instance's Publish calls for
	// that Type that have raised the guard below but not yet returned:
	// raised before the outbox insert can make a row visible, dropped by a
	// deferred function when Publish returns, so the count spans the
	// insert and the local handler loop. Its only poller-facing role is
	// the unrecorded-id signal: while the count exceeds the number of ids
	// recorded in inFlightRows for the Type, at least one raised Publish
	// has not yet learned its row's id, so its row could be committed and
	// visible while this instance still cannot name it -- a fetch could
	// catch such a row between its commit and the local delivery that will
	// mark it, so the poller withholds the Type for exactly that window
	// (see deliverPendingForType). The count is deliberately NOT a wedge
	// guard: a Publish wedged inside a local handler has long since
	// recorded its row's id, so count and recorded ids agree and the
	// poller proceeds, skipping exactly the wedged row. The deferred drop
	// means a panic cannot leave a count behind (a panic also skips the
	// mark, leaving the row for the poller -- see Publish's own doc
	// comment). Guarded by deliverMu.
	inFlight map[string]int

	// inFlightRows records, per event Type, the ids of committed outbox
	// rows whose synchronous local delivery -- Publish's own handler loop
	// -- is in progress. An id is recorded once the row's INSERT has
	// returned (the row is committed by then, which is what makes it
	// fetchable) and the Publish found at least one local handler to run,
	// and it is removed by the same deferred drop that lowers the count,
	// only after the row was recorded in locallyDelivered. While an id is
	// recorded, the catch-up poller skips exactly that row -- its handlers
	// are running, or are about to run and then mark it -- rather than
	// abandoning the whole Type, which is what lets a wedged local handler
	// (one that never returns) stall nothing but its own row: every other
	// row of the Type keeps being delivered, and the wedged row is not
	// re-fetched once the scan has advanced past it (see
	// deliverPendingForType's doc comment for the trade that advance
	// makes). A Publish that found no local handler records nothing: it
	// will never deliver the row itself, so the row must stay visible to
	// the poller, which is its delivery. Guarded by deliverMu.
	inFlightRows map[string]map[int64]struct{}

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

	// panicRetries records, per event Type and outbox row id, the bounded
	// retry state of a row whose catch-up delivery invoked at least one
	// panicking handler: the handlers -- values from the subscription
	// snapshot that delivery ran -- whose invocation of that row has not
	// completed, the decoded event retries re-invoke them with, and the
	// attempt accounting that keeps the retry bounded. The record is what
	// preserves the "a panicked delivery is not acked as delivered" intent
	// without wedging the type: the scan advances
	// its cursor past a panicked row exactly like a clean one (see
	// deliverPendingForType), so the row is never fetched again and never
	// re-fanned out to the healthy siblings, while the still-panicking
	// handlers are retried from this record alone by the pass at the top of
	// every deliverPendingForType call, up to maxPanickedRowAttempts
	// attempts, and settle with a terminal log line when the budget is
	// exhausted (see retryPanickedRows). Both writers -- the scan, which
	// creates a record after advancing past a panicked row, and the retry
	// pass, which consumes records -- run on the listener goroutine, but
	// the map is guarded by deliverMu like its siblings so a future writer
	// on another goroutine cannot race it. In-process by design: a record
	// lost to a crash costs the bounded retry of an already-delivered-once
	// row, whose panics were logged before the crash -- never a row the
	// healthy handlers have not seen.
	panicRetries map[string]map[int64]*panicRetry

	ctx    context.Context
	cancel context.CancelFunc

	listenStarted atomic.Bool
	listenOnce    sync.Once
	listenDone    chan struct{}
}

// panicRetry is one outbox row's bounded redelivery state (see
// EventBus.panicRetries): which handlers still owe their delivery of the
// row, the event they are retried with, and how many attempts have been
// spent. Guarded by deliverMu wherever it is read or written.
type panicRetry struct {
	handlers    []pkgcore.EventHandler
	evt         pkgcore.Event
	attempts    int
	lastAttempt time.Time
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
		inFlightRows:     make(map[string]map[int64]struct{}),
		locallyDelivered: make(map[string]map[int64]struct{}),
		panicRetries:     make(map[string]map[int64]*panicRetry),
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
	// the row visible to the catch-up poller, and drop it (with the row's
	// id, once recorded) only when this function returns -- after the
	// local handlers ran and the row id was recorded in locallyDelivered
	// (the block near the end of this function). The count's role is the
	// no-double-delivery argument's transient-insert half: while it
	// exceeds the ids recorded in inFlightRows, the poller withholds the
	// Type, and a row committed in that window is never fetched before its
	// local delivery is recorded (see deliverMu's own doc comment). The
	// defer makes sure neither the count nor a recorded id outlives this
	// call on ANY path out of it; a panicked local handler cannot unwind
	// this function at all, because the loop below invokes every handler
	// through runLocalHandler's recover, which contains and logs the
	// panic the way the in-memory bus contains a subscriber's panic (see
	// runLocalHandler's own doc comment for why the escape this code
	// predates was a hole). The catch-up poller's own per-row delivery
	// still recovers panicking handlers and bounded-retries the still-
	// panicking VALUES (deliverOutboxRow/retryPanickedRows): that path
	// runs outside any publisher's stack, which is where such a retry
	// belongs -- the local path's recovered-and-dropped panic is exactly
	// the treatment pkgcore's in-memory bus gives the same situation.
	// What the defer does NOT buy is immunity from a handler that never
	// returns: the count spans the synchronous handler loop below, so a
	// wedged handler keeps this call's count and recorded id up for as
	// long as it runs. That is deliberate -- it is exactly what makes the
	// poller skip the wedged row rather than re-run its handlers -- and it
	// cannot stall the poller's OTHER deliveries, because the poller's
	// gate only withholds the Type while an id is unrecorded, and this
	// call's id is recorded before its handlers run (see below).
	recordedInFlightID := false
	var id int64
	b.deliverMu.Lock()
	b.inFlight[evt.Type]++
	b.deliverMu.Unlock()
	defer func() {
		b.deliverMu.Lock()
		if recordedInFlightID {
			ids := b.inFlightRows[evt.Type]
			delete(ids, id)
			if len(ids) == 0 {
				delete(b.inFlightRows, evt.Type)
			}
		}
		b.inFlight[evt.Type]--
		if b.inFlight[evt.Type] == 0 {
			delete(b.inFlight, evt.Type)
		}
		b.deliverMu.Unlock()
	}()

	id, err = insertOutboxAndNotify(ctx, b.pool, evt.Type, string(evt.TenantID), payload)
	if err != nil {
		return fmt.Errorf("pkgcore/eventbus/postgres: publish event %q: %w", evt.Type, err)
	}

	// Local handlers run WITHOUT deliverMu held (the in-flight record
	// below is what keeps the catch-up poller from racing them),
	// synchronously and in registration order on this goroutine, exactly
	// as on the in-memory bus. A handler's own re-entrant Publish on this
	// bus takes deliverMu only for its brief in-flight bookkeeping, never
	// blocking on this call's lock, because this call no longer holds it.
	handlers := b.handlersFor(evt.Type)
	if len(handlers) > 0 {
		// Record the committed row's id in inFlightRows before its handlers
		// run, naming it for the poller's per-row skip while this call's
		// synchronous delivery is in progress (the window between this
		// record and the mark below) -- and only when there IS a local
		// delivery in progress: a publish that found no handler will never
		// deliver the row itself, so the row must stay visible to the
		// poller, which is its delivery. Between the row's commit and this
		// record the raised count exceeds the recorded ids, so the poller
		// withholds the Type (the count's role above; see also deliverMu's
		// own doc comment).
		b.deliverMu.Lock()
		ids := b.inFlightRows[evt.Type]
		if ids == nil {
			ids = make(map[int64]struct{})
			b.inFlightRows[evt.Type] = ids
		}
		ids[id] = struct{}{}
		recordedInFlightID = true
		b.deliverMu.Unlock()
	}
	failures := make([]error, 0, len(handlers))
	for i, h := range handlers {
		err, panicked := b.runLocalHandler(ctx, evt, i, h)
		if panicked {
			// Recovered, logged and dropped -- exactly as on the in-memory
			// bus, where a panicked handler is not a handler failure a
			// caller could act on (see pkgcore's runMemoryBusHandler doc
			// comment for the full rationale); the handler's siblings
			// still run, this publish stays a success, and the row's
			// local delivery is still marked below exactly as if the
			// panicked handler had never been subscribed.
			continue
		}
		if err != nil {
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
		// function returns), so whatever the drop unblocks -- a fetch the
		// count had been withholding -- can only ever see an already-marked
		// row, never a row whose local delivery is done but unrecorded;
		// for a row the poller already passed while this delivery was in
		// flight, the mark is simply dead on arrival (see
		// deliverPendingForType). A replica with no local subscriber to
		// evt.Type records nothing: the row simply waits in the outbox for
		// whichever OTHER replica (or a later Subscribe on this one)
		// discovers it.
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
// *gorm.DB at all (go/pkgcore cannot import go/dbkit: the dependency graph
// runs pkgcore -> dbkit -> ...), so its own dedicated connection is a wholly
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
// section first withholds the Type while a Publish on this same instance
// has raised its in-flight count without yet recording its row's id --
// see deliverMu's own doc comment for why that check, the cursor read and
// the fetch must share the section to rule out double delivery. That
// window is the insert's own round trip and nothing more: a Publish whose
// row is recorded in inFlightRows -- a wedged handler included -- does
// NOT withhold the Type. The section then reads the cursor and fetches
// the batch. Every handler runs outside deliverMu (deliverOutboxRow).
// Rows are then handled one at a time: a row whose id the local path
// already marked is skipped -- that mark IS this instance's synchronous
// delivery of the row, so running its handlers again would duplicate it
// -- and so is a row whose id sits in inFlightRows, whose local delivery
// is running right now on the publishing goroutine (see Publish); a row
// that is neither is delivered normally. Every row, marked or in flight
// or not, is then advanced past, outside deliverMu. That advance past an
// in-flight row is the deliberate trade this per-row gate makes, and the
// reason it can pass the row while its local delivery is still running:
// one monotone watermark cannot both advance past the later rows of the
// Type -- which must keep being delivered while an earlier row's local
// handler is wedged -- and stay behind the wedged row. A wedged local
// handler therefore stalls nothing but its own row: the scan skips the
// row (re-running its handlers would invoke a handler that is already
// running) and the row's delivery is owed to the local path that is
// still running it, while every other row of the Type is delivered and
// advanced past normally. The cost is on the crash side: a row whose
// local delivery crashes or panics AFTER the scan passed it is not
// redelivered on restart -- the scan already covered it -- whereas rows
// whose local delivery is not in flight keep the full handle-then-advance
// argument below. On an advance failure
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
//
// A row whose delivery invokes a panicking handler is advanced past
// exactly like a clean one -- a panic must not wedge the Type's cursor --
// and the still-panicking handler values are recorded in panicRetries
// (once the advance succeeded) for retryPanickedRows' bounded,
// cursor-independent retry pass at the top of each call: the retry
// re-invokes only those handlers, never the row's whole fan-out, so a
// panicking sibling never stalls the Type's later rows and never re-runs
// its healthy siblings, and the attempt budget settles the row with a
// terminal log line instead of an unbounded redelivery loop (see
// retryPanickedRows and EventBus's own doc comment's delivery note).
func (b *EventBus) deliverPendingForType(ctx context.Context, eventType string) {
	// One due retry round per recorded panicked row of this Type runs on
	// every wake, before the scan, outside deliverMu: the retry is
	// independent of the cursor -- the scan has already advanced past every
	// recorded row -- and re-invokes only the handlers that still owe their
	// delivery, so a panicking handler can neither stall this Type's later
	// rows nor re-run its healthy siblings (see retryPanickedRows).
	b.retryPanickedRows(ctx, eventType)

	cursorKnown := false
	for {
		// One critical section per batch: the in-flight check, the cursor
		// read and the fetch must be atomic against Publish's own
		// bookkeeping for the no-double-delivery argument to hold (see
		// deliverMu's own doc comment), so deliverMu stays held across
		// them but is released before any handler runs.
		b.deliverMu.Lock()
		if b.inFlight[eventType] > len(b.inFlightRows[eventType]) {
			// At least one local Publish of eventType has raised its
			// in-flight count but not yet recorded its row's id: its
			// INSERT has not returned, so its row may be committed and
			// visible at any moment while this instance still cannot name
			// it -- a fetch right now could catch such a row between its
			// commit and the local delivery that will mark it, so the
			// whole batch is withheld. The window is the insert's own
			// round trip, and the next catch-up cycle (a NOTIFY or the
			// next listenBlock timeout) delivers what that Publish did
			// not. A Publish wedged inside its local handlers does NOT
			// land here -- its row's id is recorded, so the count and the
			// recorded set agree and the scan proceeds, skipping exactly
			// that row below -- which is what keeps a wedged handler from
			// stalling the Type's other rows (see this function's doc
			// comment for the trade the skip's advance makes).
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
			// row's handlers (the locallyDelivered mark) or is running them
			// right now (the inFlightRows record). Reading under the lock
			// synchronizes against the concurrent mark, record and prune
			// writers. A row fetched here was committed before this batch's
			// critical section began, so a local publish that covered it is
			// in exactly one of three states at this check: still in flight
			// (its id is recorded -- skip it: its handlers are running, or
			// about to run, on the publishing goroutine and will mark it),
			// already completed and marked (skip it: running its handlers
			// again would duplicate the delivery), or completed WITHOUT
			// marking -- the crash-in-the-gap case of the at-least-once
			// argument above (a process that died between its local
			// handlers completing and the mark, or whose in-process mark
			// was lost to a restart; Publish's own panicked local handlers
			// no longer land here, since runLocalHandler contains them and
			// the mark still lands -- see Publish's defer comment). Such a
			// row is delivered here, once: a delivery that never completed
			// must not be silently skipped, but it must not wedge the type
			// either, so the delivery's panicked handlers are recorded
			// below (once this row's cursor advance has succeeded) for
			// retryPanickedRows' bounded, in-process retry -- delivered
			// without re-running the row's healthy siblings.
			b.deliverMu.Lock()
			_, alreadyLocal := b.locallyDelivered[eventType][row.id]
			_, localDeliveryInFlight := b.inFlightRows[eventType][row.id]
			b.deliverMu.Unlock()

			var evt pkgcore.Event
			var owed []pkgcore.EventHandler
			if !alreadyLocal && !localDeliveryInFlight {
				evt, owed = b.deliverOutboxRow(ctx, row)
			}
			if err := advanceCursorAtLeast(ctx, b.pool, b.replicaID, eventType, row.id); err != nil {
				return // retried from the (unadvanced) persisted cursor next call
			}
			if len(owed) > 0 {
				// The row's delivery panicked and its row is now behind the
				// cursor -- advanced past exactly like a clean delivery, so
				// this Type's later rows keep flowing and no later scan can
				// re-fan the row out to every subscriber. Record the
				// still-panicking handler values for the bounded retry pass
				// (each panic is logged, see runHandlerRecovered): a
				// permanently panicking handler is a programming bug an
				// operator must fix or remove, and after
				// maxPanickedRowAttempts attempts the row settles with a
				// terminal log line -- never an unbounded hot loop, and never a wedge:
				// returning before the advance would stall the type (the broker-backed twins bound their own
				// panicked-delivery redeliveries by their own mechanisms:
				// eventbus/nats's negatively-acknowledged messages are
				// redelivered only up to the eventMaxDeliver budget --
				// re-invoking just the panicked handler values each round --
				// and eventbus/redis reads only new entries with a ">"
				// cursor, so its unacked panicked entries are never
				// refetched at all -- the reader stalls nothing and re-runs
				// nothing).
				b.deliverMu.Lock()
				rec := b.panicRetries[eventType][row.id]
				if rec == nil {
					rec = &panicRetry{evt: evt}
					if b.panicRetries[eventType] == nil {
						b.panicRetries[eventType] = make(map[int64]*panicRetry)
					}
					b.panicRetries[eventType][row.id] = rec
				}
				// A record already in place means this same row was fetched
				// again -- possible only after an earlier advance failure
				// re-opened the row -- so the fresh fan-out that just ran
				// supersedes the old record's round: replace its owed
				// handlers and restart the attempt accounting from this
				// delivery.
				rec.handlers = owed
				rec.attempts = 1
				rec.lastAttempt = time.Now()
				b.deliverMu.Unlock()
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
// left in process for this event to report it to. It returns the decoded
// event and the handlers -- values from the subscription snapshot it ran --
// whose invocation panicked. A non-empty result tells the caller that the
// row's delivery did not complete: those handlers' side effects never ran,
// so the row must not be treated as acked-as-delivered. The caller keeps
// the cursor moving (a panicked delivery must not wedge the type) and hands
// the returned handlers to the bounded retry machinery, which re-invokes
// only them rather than re-running the row's whole fan-out (see
// deliverPendingForType and retryPanickedRows). A row that does not decode
// is corrupt (or hand-written); it must not wedge the reader, so it is
// skipped -- the cursor still advances past it in the caller -- and is
// reported as clean, with nothing owed.
func (b *EventBus) deliverOutboxRow(ctx context.Context, row outboxRow) (pkgcore.Event, []pkgcore.EventHandler) {
	var payload interface{}
	if err := json.Unmarshal(row.payload, &payload); err != nil {
		return pkgcore.Event{}, nil
	}
	evt := pkgcore.Event{Type: row.eventType, TenantID: pkgcore.TenantID(row.tenantID), Payload: payload}

	var owed []pkgcore.EventHandler
	for _, h := range b.handlersFor(row.eventType) {
		if b.runHandlerRecovered(ctx, evt, h) {
			owed = append(owed, h)
		}
	}
	return evt, owed
}

// runLocalHandler invokes one locally-subscribed handler on the publishing
// goroutine with the containment Publish promises. A panic raised by the
// handler is recovered here rather than let to unwind through the
// publisher: the local fan-out runs on the CALLER's goroutine -- frequently
// a background goroutine with no recover of its own (a jobs handler, a
// subscription chain, a periodic scheduler), exactly the caller pkgcore's
// in-memory bus names as the reason runMemoryBusHandler exists -- and this
// bus's own reader machinery, whose per-handler recovery
// (runHandlerRecovered) protects handlers on the catch-up path, is not on
// this stack. The recovered panic is reported through log/slog (pkgcore is
// the dependency floor and cannot import go/observability, the same reason
// runMemoryBusHandler reaches for slog.Default()) with the handler index,
// the event type and the panic value, and is DROPPED, never returned as an
// error: like the in-memory bus's recover block it is not a handler failure
// a caller could act on, and the handler's siblings still run. This is the
// containment that lets Publish's local delivery -- a row whose handlers
// ran synchronously -- be marked delivered exactly like a panic-free one;
// a permanently panicking handler is a programming bug the logged panic
// names for an operator, and retrying its delivery is the catch-up
// machinery's job only when a handler panics on THAT path (outside any
// publisher's stack), never this one.
func (b *EventBus) runLocalHandler(ctx context.Context, evt pkgcore.Event, i int, h pkgcore.EventHandler) (err error, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			slog.Default().Error("pkgcore/eventbus/postgres: local handler panicked; recovered so the publishing caller is unaffected and the event's remaining handlers still run",
				"event_type", evt.Type,
				"handler", i,
				"panic", fmt.Sprintf("%v", r),
			)
		}
	}()
	err = h(ctx, evt)
	return err, false
}

// runHandlerRecovered invokes one handler for a catch-up-delivered event,
// containing any panic it raises. A recovered panic is reported (and
// logged): the caller records the handler as owing its delivery of the
// event rather than a delivery whose side effects never ran being silently
// treated as done (see deliverOutboxRow and retryPanickedRows).
//
// pkgcore is the dependency floor of the workspace and cannot import
// go/observability, so this reaches for log/slog directly, the same
// precedent warnIfNotDurable sets in pkgcore's own root package.
func (b *EventBus) runHandlerRecovered(ctx context.Context, evt pkgcore.Event, h pkgcore.EventHandler) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			slog.Default().Error("pkgcore/eventbus/postgres: remote handler panicked; the row's delivery is retried for this handler on a bounded budget",
				"event_type", evt.Type,
				"panic", fmt.Sprintf("%v", r),
			)
		}
	}()
	_ = h(ctx, evt)
	return false
}

// retryPanickedRows runs one due retry round for every outbox row of
// eventType recorded in panicRetries. It is invoked at the top of every
// deliverPendingForType call -- once per listener wake per subscribed Type,
// on the listener goroutine, never from the scan's own row loop -- so a
// panicked row's retry is entirely independent of the cursor: the scan has
// already advanced past the row (that advance is what created its record),
// and no part of this pass consults or moves the watermark. A retry round
// re-invokes ONLY the recorded handler values that have not completed yet,
// never the row's whole fan-out, so a panicking sibling can never make a
// healthy one re-run (each invocation is recovered and logged by
// runHandlerRecovered). Rounds are spaced at least panicRetryDelay apart
// and bounded: once a row's handlers have been invoked
// maxPanickedRowAttempts times in total with at least one still panicking,
// the row settles -- its record is dropped with a terminal log line, the
// dead-letter of this mechanism -- and is never retried again, so a
// permanently panicking handler cannot become an unbounded hot loop. A
// row whose handlers all complete on some round resolves silently (the
// record is dropped): the at-least-once delivery of the row is then
// complete.
func (b *EventBus) retryPanickedRows(ctx context.Context, eventType string) {
	b.deliverMu.Lock()
	type dueRow struct {
		id  int64
		rec *panicRetry
	}
	due := make([]dueRow, 0, len(b.panicRetries[eventType]))
	for id, rec := range b.panicRetries[eventType] {
		if time.Since(rec.lastAttempt) >= panicRetryDelay {
			due = append(due, dueRow{id: id, rec: rec})
		}
	}
	b.deliverMu.Unlock()
	if len(due) == 0 {
		return
	}

	for _, row := range due {
		stillOwed := make([]pkgcore.EventHandler, 0, len(row.rec.handlers))
		for _, h := range row.rec.handlers {
			if b.runHandlerRecovered(ctx, row.rec.evt, h) {
				stillOwed = append(stillOwed, h)
			}
		}
		b.deliverMu.Lock()
		if len(stillOwed) == 0 {
			// Every still-owed handler completed on this round: the row's
			// delivery is complete.
			delete(b.panicRetries[eventType], row.id)
			if len(b.panicRetries[eventType]) == 0 {
				delete(b.panicRetries, eventType)
			}
			b.deliverMu.Unlock()
			continue
		}
		row.rec.handlers = stillOwed
		row.rec.attempts++
		row.rec.lastAttempt = time.Now()
		if row.rec.attempts < maxPanickedRowAttempts {
			b.deliverMu.Unlock()
			continue
		}
		// Budget exhausted: the row settles honestly -- logged terminal,
		// dropped from the records, never retried again.
		delete(b.panicRetries[eventType], row.id)
		if len(b.panicRetries[eventType]) == 0 {
			delete(b.panicRetries, eventType)
		}
		b.deliverMu.Unlock()
		slog.Default().Error("pkgcore/eventbus/postgres: panicking remote handler exhausted its retry budget; row abandoned",
			"event_type", eventType,
			"row_id", row.id,
			"attempts", row.rec.attempts,
		)
	}
}
