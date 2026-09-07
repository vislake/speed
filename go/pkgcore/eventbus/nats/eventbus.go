// Package nats is a second distributed deployment mode EventBus, delivering
// events between the replicas of a deployment through a NATS JetStream
// server. docs/internal/03-deployment-modes.md's own seam table used to name
// NATS a candidate for this seam alongside the already-real
// eventbus/redis implementation; this package makes it real, following the
// identical split eventbus/redis established: it lives in its own subpackage
// rather than in go/pkgcore's own root -- Go resolves dependencies per
// package, so a consumer that never wires a NATS-backed bus does not inherit
// nats.go in its dependency graph (see the "Dependency cost" section below).
//
// Importing this package registers "eventbus.nats" on pkgcore's shared
// EventBusRegistry as a side effect (see register.go) -- the same
// database/sql-style driver-registration pattern eventbus/redis's own
// register.go uses, one seam now backed by two registered names. Neither
// name is what PresetDistributed points the "eventbus" seam at today (it
// still names "eventbus.redis"); a host that wants this implementation
// instead calls NewEventBus directly and wires it with pkgcore.WithEventBus,
// or builds it by name through pkgcore.EventBusRegistry.Build("eventbus.nats", cfg).
//
// # Delivery semantics: fan-out, not load-balanced
//
// This is the one decision that matters most about this implementation, and
// it is deliberately identical to eventbus/redis's: an event published by any
// replica is delivered to EVERY replica's own subscribed handlers, not to
// one replica chosen from the group. eventbus/redis gets this by giving each
// bus instance its own uniquely-named consumer group on a shared Redis
// Streams key -- since a consumer group is itself the unit Redis
// load-balances *within*, and every instance's group is unique, no two
// instances ever compete for the same message, so every group (and every
// instance) sees every message. This package reaches the identical
// fan-out-to-every-replica property with JetStream's analogous primitive:
// each bus instance creates its own uniquely-named *durable pull consumer*
// (see busConsumerName) on the event type's stream. JetStream's own
// load-balancing behaviour only kicks in when two callers pull from the
// *same* named consumer; because every instance's consumer name embeds its
// own random instance id, no two instances ever share one, so every
// instance's consumer independently receives every message on the stream --
// exactly the semantic go/notification's own Redis integration tier proves
// its cross-replica inbox fan-out depends on (see that module's
// integration_test/redis_leg_test.go), and exactly what this package's own
// integration_test/eventbus_test.go proves again here, against a real NATS
// server, with two independent client connections standing in for two
// replicas subscribed to the same event type.
//
// How delivery works, mirroring eventbus/redis's own shape almost exactly:
//
// Each event Type has its own JetStream stream (see streamNameForEventType),
// bound to that type's own subject (see eventSubject) -- the analogue of
// eventbus/redis's one-stream-per-event-type design, chosen for the
// identical reason: it lets each type be trimmed and inspected
// independently, and it means a reader for one type is never woken by
// traffic on another. Publish appends the event to the type's stream -- the
// commit point every replica reads from, this one included -- and then
// invokes this instance's own subscribers synchronously, in registration
// order, exactly like the in-memory bus and eventbus/redis. Every other
// replica's own durable consumer on the same stream delivers the event to
// its reader, asynchronously; a reader skips the events its own instance
// appended (recognised by the "Pkgcore-Src" header stamped on every publish,
// the direct analogue of eventbus/redis's "src" stream field), because the
// Publish call already delivered those locally.
//
// One structural difference from eventbus/redis is unavoidable and is
// documented here rather than papered over: Redis Streams auto-vivify a
// stream key on its first XADD, so eventbus/redis.Publish never has to think
// about stream existence. JetStream requires a stream to be explicitly
// declared, bound to the subject a publish targets, before that publish can
// succeed -- there is no server-side auto-creation. Publish therefore calls
// ensureStream itself (idempotent, and memoized per event type after the
// first successful call so steady-state publishes pay no extra round trip)
// before every first-ever publish of a type, exactly the one added step a
// JetStream-backed publisher must take that a Redis Streams one does not.
// The memoization self-heals rather than going stale forever: Close deletes
// a stream once its last reader (on any instance) disconnects (see
// destroyConsumers), so a pure publisher that never subscribes can find its
// memoized belief wrong on its very next Publish. That call's
// PublishMsg failure triggers exactly one forget-and-recreate retry (see
// Publish's own comment), so a stream deleted out from under a publisher is
// transparently recreated rather than wedging that event type forever --
// the same resilience Redis's auto-vivification gets for free.
//
// Delivery-semantics note (read before choosing this bus -- mirrors
// eventbus/redis's own note point for point):
//
//   - Cross-process delivery is asynchronous and at-least-once-ish but not
//     retried: when Publish returns, replicas may not have run their
//     handlers yet. Every entry is acknowledged after its handlers ran,
//     whatever they returned -- deliberately forgoing JetStream's own
//     stronger MaxDeliver/AckWait redelivery machinery for handler errors,
//     in order to keep this implementation's observable behaviour identical
//     to eventbus/redis's documented contract, which the same
//     eventbustest.AssertConforms suite checks for both. A message whose
//     remote handler panicked is the one exception: it is left
//     unacknowledged rather than acked as delivered, its panic is logged
//     (see runRemoteHandler), and JetStream's own redelivery then re-
//     delivers it after the consumer's AckWait window. Redelivery, retries
//     and dead-letter handling belong to the jobs queue, built for them.
//   - Payloads cross the process boundary as JSON. The shape survives -- a
//     struct becomes a map[string]any -- but the concrete Go type does not.
//     Handlers on the publishing replica receive the original payload
//     untouched.
//   - Handlers on other replicas run on the bus's own root context, which
//     carries no tenant, and their errors are not observable by any
//     publisher. A handler that needs tenant data must rebuild it from the
//     event with pkgcore.WithTenant, and a panic inside it is recovered --
//     and logged, never silently swallowed -- so one buggy handler cannot
//     take a replica down.
//   - A replica that subscribes late does not catch up: its consumer is
//     created with DeliverNewPolicy, JetStream's live-end-only equivalent of
//     eventbus/redis's "$" starting id. Unlike Redis's approximate MAXLEN
//     trim, a JetStream stream's MaxMsgs limit (see eventStreamMaxMsgs) is
//     enforced exactly, but the effect for a disconnected replica is the
//     same: events that scrolled out of the stream's retention window before
//     it reconnects are gone for it, silently.
//   - Handlers of one event Type run serially, in registration order, on
//     that type's own dispatch goroutine (the one nats.go's Consumer.Consume
//     keeps internally) -- a slow handler delays the next remote event of
//     the same type, on that replica only, exactly like eventbus/redis.
//   - A consumer or stream removed out from under a running reader (an
//     operator's cleanup, a JetStream restore) surfaces as a consume error;
//     this implementation reacts by recreating both from scratch rather than
//     wedging, the same recovery eventbus/redis's NOGROUP handling performs.
//
// # SurvivesRestart: why this bus honestly declares it
//
// A JetStream stream defaults to file storage (jetstream.FileStorage is the
// zero value of StreamConfig.Storage, and this package never overrides it),
// so events already committed to a stream survive a restart of the NATS
// server process itself -- the identical honesty eventbus/redis's own
// SurvivesRestart declaration rests on for Redis Streams' AOF/RDB
// persistence. Neither declaration is a promise about *this process*
// restarting with the same in-memory bookkeeping: a fresh process gets a
// fresh random instance id (see newBusInstanceID) and therefore a fresh
// consumer starting at the stream's live end, exactly like eventbus/redis --
// SurvivesRestart is about the broker's own durability, not about this
// bus's own reader cursor surviving an app restart.
//
// # Dependency cost (root CLAUDE.md's mandatory measurement)
//
// Measured the way root CLAUDE.md's "Adding a built-in implementation
// requires measuring what it costs consumers" rule prescribes: a throwaway
// module that requires only github.com/nats-io/nats.go and blank-imports its
// jetstream subpackage, then run under `go mod tidy` with GOWORK=off, picks
// up 5 "// indirect" entries (github.com/klauspost/compress,
// github.com/nats-io/nkeys, github.com/nats-io/nuid, golang.org/x/crypto,
// golang.org/x/sys) -- against the identical measurement for
// github.com/redis/go-redis/v9, eventbus/redis's own dependency, which costs
// 3 (github.com/cespare/xxhash/v2, go.uber.org/atomic, golang.org/x/sys).
// Both numbers describe the dependency in total isolation, per the
// prescribed methodology; go/pkgcore's own go.mod already carries several of
// nats.go's transitive dependencies at an equal or newer version for other
// reasons (klauspost/compress among them), so the marginal lines this
// package's own go.mod addition actually adds are fewer than 5 -- the
// isolated number above is the one this repository's convention asks to be
// recorded, matching how eventbus/redis's own AGENTS.md census entry reports
// its own isolated cost rather than a merged one.
//
// The bus is safe for concurrent use by multiple goroutines. Publish after
// Close returns ErrEventBusClosed, and Subscribe after Close is a no-op;
// both are programming errors, detected rather than silently accepted.
package nats

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/vislake/speed/go/pkgcore"
)

// ErrEventBusClosed is returned by Publish on an EventBus after Close. It
// mirrors eventbus/redis.ErrEventBusClosed -- a distinct sentinel per
// subpackage, deliberately, since nothing outside a given implementation
// ever needs to compare across the two.
var ErrEventBusClosed = errors.New("pkgcore/eventbus/nats: event bus is closed")

const (
	// eventSubjectPrefix namespaces the subjects this bus publishes and
	// subscribes to. A host's own subjects must not collide with it, the
	// same convention eventbus/redis's eventStreamPrefix documents for its
	// own Redis keys.
	eventSubjectPrefix = "pkgcore.events."

	// eventStreamNamePrefix namespaces the JetStream stream names this bus
	// creates. Unlike a Redis key, a JetStream stream name cannot contain
	// ".", so it cannot simply be the subject; see streamNameForEventType.
	eventStreamNamePrefix = "PKGCORE_EVENTS_"

	// eventConsumerNamePrefix namespaces the per-instance durable consumer
	// name every bus instance creates on every stream it subscribes to,
	// mirroring eventbus/redis's eventGroupPrefix.
	eventConsumerNamePrefix = "pkgcore-bus-"

	// eventStreamMaxMsgs bounds each stream to approximately this many
	// entries, trimmed on ingest -- JetStream enforces this exactly, unlike
	// eventbus/redis's approximate Redis MAXLEN trim, but the two are sized
	// identically for parity.
	eventStreamMaxMsgs = 4096

	// eventRetryDelay backs off a transient failure -- NATS unreachable, a
	// stream or consumer create call failing -- before the next attempt,
	// mirroring eventbus/redis's eventRetryDelay.
	eventRetryDelay = 200 * time.Millisecond

	// eventCleanupTimeout bounds the best-effort consumer/stream cleanup
	// Close runs against the server, mirroring eventbus/redis's
	// eventGroupCleanupTimeout.
	eventCleanupTimeout = time.Second

	// headerSrc and headerTenant are the message headers every publish
	// stamps, the direct analogue of eventbus/redis's "src"/"tenant" stream
	// fields.
	headerSrc    = "Pkgcore-Src"
	headerTenant = "Pkgcore-Tenant"
)

// EventBus is a second distributed deployment mode pkgcore.EventBus,
// delivering events between the replicas of a deployment through the
// JetStream-enabled NATS connection they share. A distributed-mode host
// passes one already-connected *nats.Conn for the whole deployment to
// NewEventBus and keeps owning it; the bus never closes it.
//
// See the package doc comment for the full delivery contract this type
// implements -- fan-out to every replica, not load-balanced delivery to one.
type EventBus struct {
	js         jetstream.JetStream
	instanceID string

	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once

	mu       sync.RWMutex
	closed   bool
	handlers map[string][]pkgcore.EventHandler

	streamsMu    sync.Mutex
	streamsReady map[string]struct{}

	readersMu sync.Mutex
	readers   map[string]*busReader
}

// busReader tracks the currently-active ConsumeContext for one event type on
// this instance, so Close (and an error-triggered recreation) can stop
// whichever one is live right now.
type busReader struct {
	mu         sync.Mutex
	consumeCtx jetstream.ConsumeContext
}

// NewEventBus returns a pkgcore.EventBus that delivers between replicas
// through the given, already-connected NATS connection: events published on
// any bus sharing the connection's JetStream account reach the subscribers
// of every one of them. pkgcore's in-memory bus covers the standalone
// deployment mode; eventbus/redis and this package are its two distributed
// counterparts, and a distributed-mode host wires whichever it wants with
// pkgcore.WithEventBus.
//
// Unlike eventbus/redis.NewEventBus, which accepts a lazily-dialling
// *redis.Client, nc must already be connected: nats.go's own client model
// dials synchronously inside nats.Connect, so there is no lazy equivalent to
// hand this constructor. register.go's own built-in "eventbus.nats"
// constructor calls nats.Connect with nats.RetryOnFailedConnect(true) for
// exactly this reason -- so that building the seam through
// pkgcore.EventBusRegistry never blocks or fails just because NATS is not
// reachable yet at that moment, the same "never fails at build time"
// property eventbus/redis's lazy client gets for free.
//
// The returned bus starts no reader goroutine and creates no stream or
// consumer until the first Subscribe of a given event type (Publish creates
// a type's stream lazily too, on its own first call for that type -- see the
// package doc comment). A nil connection panics: it is an unrecoverable
// wiring error at startup.
func NewEventBus(nc *nats.Conn) *EventBus {
	if nc == nil {
		panic("pkgcore/eventbus/nats: NewEventBus requires a non-nil *nats.Conn")
	}
	js, err := jetstream.New(nc)
	if err != nil {
		// jetstream.New performs no network call and, called with no
		// options (as here), has no failure path in the version this
		// package is pinned to -- this branch exists so a future version
		// that adds one fails loudly at construction rather than silently
		// handing back a JetStream that can never work.
		panic(fmt.Sprintf("pkgcore/eventbus/nats: jetstream.New: %v", err))
	}
	//nolint:gosec // G118 would have this cancel deferred, but the bus's
	// lifetime outlives this constructor: the cancel is stored on the bus and
	// called exactly once by Close, which is what stops every reader.
	ctx, cancel := context.WithCancel(context.Background())
	return &EventBus{
		js:           js,
		instanceID:   newBusInstanceID(),
		ctx:          ctx,
		cancel:       cancel,
		handlers:     make(map[string][]pkgcore.EventHandler),
		streamsReady: make(map[string]struct{}),
		readers:      make(map[string]*busReader),
	}
}

// Close stops the bus: Publish fails with ErrEventBusClosed from here on,
// every reader's ConsumeContext is stopped, and the durable consumers this
// instance created are deleted, along with any stream that ends up with no
// consumer left on it -- a deployment that shuts every replica down
// gracefully leaves nothing behind on the server. A replica that crashes
// instead of closing leaks its consumers, one per stream per instance, until
// an operator removes them; the NATS CLI recipe is
//
//	nats consumer rm PKGCORE_EVENTS_<sanitized-type> pkgcore-bus-<instance-id>
//	nats stream rm PKGCORE_EVENTS_<sanitized-type>  # once no consumer is left
//
// The cleanup is best-effort and bounded by eventCleanupTimeout: a NATS
// server that does not answer during Close leaves the consumers in place,
// and the same recipe removes them later. A closed bus must not be used
// again. Close is idempotent and safe for concurrent use; the connection the
// bus was built on stays open, because the host owns it.
func (b *EventBus) Close() {
	b.once.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.mu.Unlock()
		b.cancel()
		b.destroyConsumers()
	})
}

// destroyConsumers stops every reader's ConsumeContext and removes the
// durable consumers this instance created, deleting a stream once it carries
// no consumer at all. It runs after the readers were told to stop via
// b.cancel, and it is best-effort: the bus is already closed, so there is no
// error left to report -- a failure is a leaked consumer, and the Close docs
// name the operator command that removes it.
func (b *EventBus) destroyConsumers() {
	b.readersMu.Lock()
	eventTypes := make([]string, 0, len(b.readers))
	for eventType, r := range b.readers {
		r.mu.Lock()
		if r.consumeCtx != nil {
			r.consumeCtx.Stop()
		}
		r.mu.Unlock()
		eventTypes = append(eventTypes, eventType)
	}
	b.readersMu.Unlock()
	if len(eventTypes) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), eventCleanupTimeout)
	defer cancel()

	consumerName := busConsumerName(b.instanceID)
	for _, eventType := range eventTypes {
		streamName := streamNameForEventType(eventType)
		_ = b.js.DeleteConsumer(ctx, streamName, consumerName)
		remaining, err := countConsumers(ctx, b.js, streamName)
		if err != nil || remaining != 0 {
			continue
		}
		_ = b.js.DeleteStream(ctx, streamName)
	}
}

// countConsumers returns how many consumers streamName currently carries, so
// destroyConsumers can tell whether this instance's was the last one.
func countConsumers(ctx context.Context, js jetstream.JetStream, streamName string) (int, error) {
	stream, err := js.Stream(ctx, streamName)
	if err != nil {
		return 0, err
	}
	lister := stream.ConsumerNames(ctx)
	n := 0
	for range lister.Name() {
		n++
	}
	return n, lister.Err()
}

// Subscribe registers h for the exact event type eventType, mirroring
// eventbus/redis and the in-memory bus: several handlers may subscribe to
// the same type, all of them are invoked in registration order, and a nil
// handler is ignored.
//
// The first subscription to a type on this instance starts the type's
// reader goroutine: it creates the type's stream and this instance's own
// durable consumer when NATS is reachable, retrying in the background until
// it is, so Subscribe itself never blocks on the network and never fails --
// an event published while its only subscriber's NATS was unreachable is
// lost for that subscriber, which the package doc comment's delivery note
// owns up to.
func (b *EventBus) Subscribe(eventType string, h pkgcore.EventHandler) {
	if h == nil {
		return
	}
	if b.isClosed() {
		return
	}
	b.ensureReader(eventType)

	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[eventType] = append(b.handlers[eventType], h)
}

// Publish delivers evt to every handler subscribed to evt.Type on every bus
// sharing this bus's NATS connection. The event is first appended to the
// type's stream -- the commit point every replica reads from, creating the
// stream on this call if it is the first-ever publish of this type (see the
// package doc comment's note on this being the one place this
// implementation must do work eventbus/redis's Redis auto-vivification
// makes unnecessary) -- and this instance's own subscribers then run
// synchronously, in registration order, exactly as on the in-memory bus,
// their failures collected into one joined error. A failed append delivers
// nothing anywhere and reports the failure; handlers of this instance do not
// run for an event that never made it into the stream, so a publish is
// all-or-nothing across the deployment.
//
// The payload must survive JSON encoding, because that is how it crosses the
// process boundary; a payload that does not (channels, funcs) fails the
// publish before anything is appended or delivered.
func (b *EventBus) Publish(ctx context.Context, evt pkgcore.Event) error {
	if b.isClosed() {
		return ErrEventBusClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	payload, err := json.Marshal(evt.Payload)
	if err != nil {
		return fmt.Errorf("pkgcore/eventbus/nats: payload of event %q is not JSON-serializable: %w", evt.Type, err)
	}

	streamName := streamNameForEventType(evt.Type)
	subject := eventSubject(evt.Type)
	if err := b.ensureStream(ctx, streamName, subject); err != nil {
		return fmt.Errorf("pkgcore/eventbus/nats: ensure stream for event %q failed: %w", evt.Type, err)
	}

	msg := &nats.Msg{
		Subject: subject,
		Data:    payload,
		Header: nats.Header{
			headerSrc:    {b.instanceID},
			headerTenant: {string(evt.TenantID)},
		},
	}
	if _, err := b.js.PublishMsg(ctx, msg); err != nil {
		// The stream this event type maps to may have been deleted out from
		// under this instance's memoized ensureStream cache: Close's own
		// cleanup removes a stream once its last reader (on any instance)
		// disconnects (see destroyConsumers), and an instance that only ever
		// publishes this type -- subscribing no handler of its own -- never
		// notices until its next publish, because ensureStream's memoization
		// (the one deliberate extra step this implementation carries over
		// eventbus/redis's own XADD auto-vivification, per the package doc
		// comment) skips recreating a stream it already believes exists.
		// Forgetting the memoized entry and retrying once, forcing the
		// stream to be recreated, is what makes Publish just as resilient to
		// this as Redis's implementation is for free; a second failure is
		// reported as-is.
		b.forgetStream(streamName)
		if ensureErr := b.ensureStream(ctx, streamName, subject); ensureErr != nil {
			return fmt.Errorf("pkgcore/eventbus/nats: publish event %q failed: %w (stream recreation also failed: %w)", evt.Type, err, ensureErr)
		}
		if _, retryErr := b.js.PublishMsg(ctx, msg); retryErr != nil {
			return fmt.Errorf("pkgcore/eventbus/nats: publish event %q failed: %w", evt.Type, retryErr)
		}
	}

	handlers := b.handlersFor(evt.Type)
	if len(handlers) == 0 {
		return nil
	}
	failures := make([]error, 0, len(handlers))
	for i, h := range handlers {
		if err := h(ctx, evt); err != nil {
			failures = append(failures, fmt.Errorf("pkgcore/eventbus/nats: handler %d for event %q failed: %w", i, evt.Type, err))
		}
	}
	return errors.Join(failures...)
}

// isClosed reports whether Close has been called.
func (b *EventBus) isClosed() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.closed
}

// forgetStream clears streamName's memoized ensureStream entry, so the next
// call recreates it rather than trusting a belief that may no longer hold --
// see Publish's retry-after-recreate comment for why this exists.
func (b *EventBus) forgetStream(streamName string) {
	b.streamsMu.Lock()
	delete(b.streamsReady, streamName)
	b.streamsMu.Unlock()
}

// ensureStream makes sure the JetStream stream backing eventType exists,
// bound to subject, memoizing success locally so a steady-state Publish or
// Subscribe of an already-known type pays no extra round trip. It is called
// synchronously from Publish (so a caller's ctx governs how long it is
// willing to wait) and in a retry loop from startConsuming (so Subscribe
// itself never blocks).
func (b *EventBus) ensureStream(ctx context.Context, streamName, subject string) error {
	b.streamsMu.Lock()
	_, ready := b.streamsReady[streamName]
	b.streamsMu.Unlock()
	if ready {
		return nil
	}

	_, err := b.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{subject},
		Retention: jetstream.LimitsPolicy,
		MaxMsgs:   eventStreamMaxMsgs,
		Discard:   jetstream.DiscardOld,
		Storage:   jetstream.FileStorage,
	})
	if err != nil {
		return err
	}

	b.streamsMu.Lock()
	b.streamsReady[streamName] = struct{}{}
	b.streamsMu.Unlock()
	return nil
}

// ensureReader starts the single reader goroutine that delivers remote
// events of eventType on this instance. The first Subscribe of a type on an
// instance owns the start; later ones find the reader already running.
func (b *EventBus) ensureReader(eventType string) {
	b.readersMu.Lock()
	if _, running := b.readers[eventType]; running {
		b.readersMu.Unlock()
		return
	}
	r := &busReader{}
	b.readers[eventType] = r
	b.readersMu.Unlock()
	go b.runReader(eventType, r)
}

// runReader owns one event type's consumer for the lifetime of the bus: it
// creates the stream and this instance's durable consumer (retrying in the
// background until NATS answers or the bus is closed), starts consuming,
// and -- if the consumer ever reports an error (the NATS-side equivalent of
// eventbus/redis's NOGROUP recovery: an operator's cleanup, a JetStream
// restore, a lost connection the client could not transparently resume) --
// stops the failed ConsumeContext and recreates everything from scratch,
// rather than leaving the reader silently dead.
func (b *EventBus) runReader(eventType string, r *busReader) {
	streamName := streamNameForEventType(eventType)
	subject := eventSubject(eventType)
	consumerName := busConsumerName(b.instanceID)
	handler := b.remoteHandlerFor(eventType)

	for {
		if b.ctx.Err() != nil {
			return
		}

		restart := make(chan struct{}, 1)
		consumeCtx, err := b.startConsuming(streamName, subject, consumerName, handler, restart)
		if err != nil {
			// b.ctx was cancelled while retrying: the bus closed under us.
			return
		}

		r.mu.Lock()
		r.consumeCtx = consumeCtx
		r.mu.Unlock()

		select {
		case <-b.ctx.Done():
			consumeCtx.Stop()
			return
		case <-restart:
			consumeCtx.Stop()
			// Loop back: stream and consumer are recreated from scratch.
		}
	}
}

// startConsuming retries creating streamName, this instance's consumerName
// on it, and starting Consume, until all three succeed or b.ctx is
// cancelled. A later consume-time error signals restart exactly once (the
// channel is buffered 1 and writes are non-blocking) so runReader's loop can
// recreate everything; it never calls back into runReader directly, since
// that would run inside nats.go's own dispatch goroutine.
func (b *EventBus) startConsuming(streamName, subject, consumerName string, handler jetstream.MessageHandler, restart chan struct{}) (jetstream.ConsumeContext, error) {
	for {
		if err := b.ctx.Err(); err != nil {
			return nil, err
		}

		if err := b.ensureStream(b.ctx, streamName, subject); err != nil {
			b.sleepRetry()
			continue
		}

		consumer, err := b.js.CreateOrUpdateConsumer(b.ctx, streamName, jetstream.ConsumerConfig{
			Durable:       consumerName,
			FilterSubject: subject,
			DeliverPolicy: jetstream.DeliverNewPolicy,
			AckPolicy:     jetstream.AckExplicitPolicy,
		})
		if err != nil {
			b.sleepRetry()
			continue
		}

		consumeCtx, err := consumer.Consume(handler, jetstream.ConsumeErrHandler(
			func(_ jetstream.ConsumeContext, _ error) {
				select {
				case restart <- struct{}{}:
				default:
				}
			},
		))
		if err != nil {
			b.sleepRetry()
			continue
		}
		return consumeCtx, nil
	}
}

// sleepRetry waits eventRetryDelay, or returns early if the bus closes.
func (b *EventBus) sleepRetry() {
	select {
	case <-b.ctx.Done():
	case <-time.After(eventRetryDelay):
	}
}

// remoteHandlerFor returns the jetstream.MessageHandler that delivers
// eventType's remote messages to this instance's subscribers.
func (b *EventBus) remoteHandlerFor(eventType string) jetstream.MessageHandler {
	return func(msg jetstream.Msg) {
		b.deliverRemote(eventType, msg)
	}
}

// deliverRemote runs one message another instance published: a message this
// instance published itself is acknowledged without dispatch, because the
// Publish call already ran the local handlers synchronously; everything else
// is reconstructed from the JSON body and handed to the registered
// handlers, then acknowledged regardless of what they returned -- unless a
// handler panicked, in which case the message is deliberately left
// unacknowledged: a handler whose side effects never ran (or only partly
// ran) must not be acked as delivered, and its panic is logged so the
// failure leaves a trace an operator can act on (see runRemoteHandler).
// JetStream's own redelivery machinery then redelivers the unacked message
// after the consumer's AckWait, which is exactly the honest at-least-once
// shape for a panicked handler -- see the package doc comment's delivery
// note.
func (b *EventBus) deliverRemote(eventType string, msg jetstream.Msg) {
	headers := msg.Headers()
	if headers.Get(headerSrc) == b.instanceID {
		_ = msg.Ack()
		return
	}

	tenant := headers.Get(headerTenant)
	var payload any
	if err := json.Unmarshal(msg.Data(), &payload); err != nil {
		// A message that does not decode is corrupt (or hand-written); it
		// must not wedge the reader, and it must not be delivered, so it is
		// dropped the same way a decoded message is after its handlers ran.
		_ = msg.Ack()
		return
	}
	evt := pkgcore.Event{Type: eventType, TenantID: pkgcore.TenantID(tenant), Payload: payload}

	panicked := false
	for _, h := range b.handlersFor(eventType) {
		if b.runRemoteHandler(evt, h) {
			panicked = true
		}
	}
	if panicked {
		// Not acknowledged: the message stays unacked on the consumer, so
		// JetStream redelivers it after the consumer's AckWait window -- and
		// each panic is logged. A permanently panicking handler is a
		// programming bug an operator must fix or remove; meanwhile the
		// redelivery cadence keeps the failure visible instead of acked
		// away.
		return
	}
	_ = msg.Ack()
}

// runRemoteHandler invokes one handler for a remote event, containing the
// panics a handler may raise: on this replica there is no publisher in
// process to receive a panic, so letting it escape would crash the whole
// replica from nats.go's own dispatch goroutine. The handler's error is not
// observable by any publisher either; it is dropped by design, see the
// package doc comment's delivery note. A recovered panic is reported (and
// logged): the caller leaves the message unacknowledged rather than acking
// a delivery whose side effects never ran.
//
// pkgcore is the dependency floor of the workspace and cannot import
// go/observability, so this reaches for log/slog directly, the same
// precedent warnIfNotDurable sets in pkgcore's own root package.
func (b *EventBus) runRemoteHandler(evt pkgcore.Event, h pkgcore.EventHandler) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			slog.Default().Error("pkgcore/eventbus/nats: remote handler panicked; message left unacknowledged",
				"event_type", evt.Type,
				"panic", fmt.Sprintf("%v", r),
			)
		}
	}()
	_ = h(b.ctx, evt)
	return false
}

// handlersFor returns a private snapshot of the handlers subscribed to
// eventType on this instance, copied under the lock so a handler is free to
// call Subscribe or Publish re-entrantly and a concurrent Subscribe cannot
// mutate the slice being iterated.
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

// eventSubject names the subject that carries events of eventType. Event
// types are documented (pkgcore.Event.Type) as exact strings with "no
// wildcards or prefixes interpreted", which is exactly the property a
// literal NATS subject needs: this function assumes eventType never
// contains the wildcard characters "*" or ">", the two characters that
// would turn this into something other than the literal subject it looks
// like.
func eventSubject(eventType string) string {
	return eventSubjectPrefix + eventType
}

// streamNameForEventType derives the JetStream stream name for eventType. A
// stream name cannot contain whitespace, ".", "*", ">", or a path separator,
// so any such character in eventType (a literal "." is the expected case,
// for a dotted event type like "authn.user.created") is replaced with "_".
//
// This is a many-to-one mapping: two event types that differ only in
// characters this function folds to the same "_" (for instance "a.b" and
// "a_b") collide on one stream name. eventbus/redis carries the mirror-image
// risk for its own eventStreamPrefix (a host key that happens to collide
// with it) and owns up to it in the same way: this is a naming convention
// event types are expected to respect (stick to letters, digits, "_", "-"
// and "." as a hierarchy separator), not a runtime-enforced guarantee.
func streamNameForEventType(eventType string) string {
	var b strings.Builder
	b.Grow(len(eventStreamNamePrefix) + len(eventType))
	b.WriteString(eventStreamNamePrefix)
	for _, r := range eventType {
		if isValidStreamNameRune(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

// isValidStreamNameRune reports whether r may appear in a JetStream stream
// or durable consumer name unescaped.
func isValidStreamNameRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		return true
	default:
		return false
	}
}

// busConsumerName is the durable consumer name every bus instance creates on
// every stream it subscribes to -- the direct analogue of eventbus/redis's
// eventGroupPrefix+instanceID consumer group name. The same name is reused
// verbatim across every stream this instance reads, exactly as a Redis
// consumer group name is; uniqueness across instances comes entirely from
// instanceID, not from any per-stream qualifier.
func busConsumerName(instanceID string) string {
	return eventConsumerNamePrefix + instanceID
}

// newBusInstanceID returns the random identifier that distinguishes one bus
// instance from every other one sharing a connection. It is stamped on
// every message the instance publishes and names its durable consumers, so
// a reader can skip its own events and two instances never share a
// consumer -- which is exactly what makes delivery fan out to every
// instance instead of load-balancing across them (see the package doc
// comment).
func newBusInstanceID() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand failing means the host's entropy source is gone; every
		// later draw would fail the same way, so fail at construction.
		panic(fmt.Sprintf("pkgcore/eventbus/nats: crypto/rand unavailable: %v", err))
	}
	return hex.EncodeToString(raw[:])
}
