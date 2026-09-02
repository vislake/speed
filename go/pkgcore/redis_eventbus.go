package pkgcore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrEventBusClosed is returned by Publish on an EventBus implementation that
// has been closed. The in-memory bus cannot be closed and never returns it;
// a bus backed by a broker can, because closing one has meaning there: it
// stops the background delivery that keeps its goroutines alive.
var ErrEventBusClosed = errors.New("pkgcore: event bus is closed")

const (
	// redisEventStreamPrefix namespaces the per-type Redis Streams that back
	// the bus. It is a convention, not a guarantee: the keys sit in the same
	// Redis namespace the host stores in, and a host key that happens to
	// equal pkgcore:events:<type> keeps that type's reader down -- the group
	// creation fails with WRONGTYPE and retries until the bus is closed,
	// exactly as if Redis were unreachable. The prefix is the protection;
	// hosts must not store their own keys under pkgcore:*.
	redisEventStreamPrefix = "pkgcore:events:"

	// redisEventGroupPrefix namespaces the consumer-group names the bus
	// creates, one group per stream per bus instance.
	redisEventGroupPrefix = "pkgcore:bus:"

	// redisEventStreamMaxLen bounds each stream, trimming it to this many
	// entries (approximately) on every append. The bus never replays history
	// to a subscriber -- a group starts at "$", the live end -- so trimming
	// only ever discards messages a reader could not have received anyway...
	// unless a reader stays disconnected longer than the stream's capacity,
	// which the delivery-semantics note below owns up to.
	redisEventStreamMaxLen = 4096

	// redisEventReaderBatch is how many stream entries one reader loop takes
	// off at once.
	redisEventReaderBatch = 32

	// redisEventReaderBlock is how long a reader blocks waiting for the next
	// entry before polling again. A short block keeps Close prompt: every
	// reader wakes up and exits within one block of the bus being closed.
	redisEventReaderBlock = 500 * time.Millisecond

	// redisEventRetryDelay backs off transient Redis failures (a restart, a
	// network blip) before a reader tries again.
	redisEventRetryDelay = 200 * time.Millisecond

	// redisEventGroupCleanupTimeout bounds the cleanup Close runs against
	// Redis. Close must not hang a shutdown on a Redis that stopped
	// answering; when the cleanup times out the groups are left behind,
	// which the Close notes tell an operator how to remove.
	redisEventGroupCleanupTimeout = time.Second
)

// RedisEventBus is the distributed deployment mode's EventBus, delivering
// events between the replicas of a deployment through the Redis Streams they
// share. A distributed-mode host passes one client for the whole deployment
// to NewRedisEventBus and keeps owning it; the bus never closes it.
//
// How delivery works:
//
// Each event Type has its own stream. Publish appends the event to the
// stream -- the append is the commit point: every replica, this one
// included, receives exactly the events that made it into the stream -- and
// then invokes this instance's own subscribers synchronously, in
// registration order, exactly like the in-memory bus. Every other replica
// runs a per-type reader goroutine that consumes the stream through its own
// consumer group, so each event is delivered to every replica exactly once.
// A reader skips the events its own instance appended, because the Publish
// call already delivered those locally; the instance id stamped on every
// entry is what tells them apart.
//
// Delivery-semantics note (read before choosing this bus):
//
// The interface contract lets a broker-backed bus deliver asynchronously,
// which is exactly what the cross-process half of this bus does, and the
// differences that follow are deliberate rather than accidental:
//
//   - Cross-process delivery is asynchronous and at-least-once-ish but not
//     retried: when Publish returns, replicas may not have run their
//     handlers yet. Each entry is acknowledged after its handlers ran,
//     whatever they returned, so the bus does not redeliver on failure.
//     Redelivery, retries and dead-letter handling belong to the jobs queue,
//     which is built for them; the bus only guarantees no handler is
//     silently skipped while its replica is connected.
//   - Payloads cross the process boundary as JSON. The shape survives -- a
//     struct becomes a map[string]any, an array becomes []any, numbers
//     become float64 -- but the concrete Go type does not. Subscribers on
//     another replica cannot type-assert the publisher's struct; they must
//     consume the documented JSON shape. Handlers on the publishing replica
//     receive the original payload untouched.
//   - Handlers on other replicas run on the bus's own context, which carries
//     no tenant, and their errors are not observable by any publisher: the
//     publisher has already moved on in another process. A handler that
//     needs tenant data must rebuild the tenant from the event with
//     WithTenant, and a panic inside it is recovered so that one buggy
//     handler cannot take down a replica. (On the publishing replica,
//     handlers run synchronously on the caller's goroutine and behave
//     exactly as they do on the in-memory bus, errors included.)
//   - A replica that subscribes late does not catch up: the consumer group
//     starts at the live end of the stream. And a replica whose reader
//     stays disconnected while its stream fills past its trim window loses
//     the events that scrolled out of it. Either way the event is gone for
//     that replica; the publisher is not told.
//   - Handlers of one event Type run serially on the type's single reader
//     goroutine, in registration order. A slow handler delays the next
//     remote event of the same type, on that replica only.
//
// The bus is safe for concurrent use by multiple goroutines. Publish after
// Close returns ErrEventBusClosed, and Subscribe after Close is a no-op;
// both are programming errors, detected rather than silently accepted.
type RedisEventBus struct {
	client     *redis.Client
	instanceID string

	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once

	mu       sync.RWMutex
	closed   bool
	handlers map[string][]EventHandler

	readersMu sync.Mutex
	readers   map[string]struct{}
}

// NewRedisEventBus returns an EventBus that delivers between replicas through
// the given Redis client: events published on any bus sharing the client
// reach the subscribers of every one of them. The in-memory bus covers the
// standalone deployment mode; this is its distributed counterpart, and a
// distributed-mode host wires it in with WithEventBus.
//
// The returned bus starts no goroutine and touches no network until the
// first Subscribe, and its readers stop at Close. A nil client panics: it is
// an unrecoverable wiring error at startup.
func NewRedisEventBus(client *redis.Client) *RedisEventBus {
	if client == nil {
		panic("pkgcore: NewRedisEventBus requires a non-nil *redis.Client")
	}
	//nolint:gosec // G118 would have this cancel deferred, but the bus's
	// lifetime outlives this constructor: the cancel is stored on the bus and
	// called exactly once by Close (see b.cancel under the once guard), which
	// is what stops the reader goroutines.
	ctx, cancel := context.WithCancel(context.Background())
	return &RedisEventBus{
		client:     client,
		instanceID: newBusInstanceID(),
		ctx:        ctx,
		cancel:     cancel,
		handlers:   make(map[string][]EventHandler),
		readers:    make(map[string]struct{}),
	}
}

// Close stops the bus: Publish fails with ErrEventBusClosed from here on,
// the reader goroutines shut down within one read block, and the consumer
// groups this instance created on its streams are destroyed, along with any
// stream that ends up with no group left on it -- a deployment that shuts
// every replica down gracefully leaves nothing behind on the server. A
// replica that crashes instead of closing leaks its groups, one per stream
// per instance, until an operator removes them with
//
//	XGROUP DESTROY pkgcore:events:<type> pkgcore:bus:<instance-id>
//	DEL pkgcore:events:<type>  # once XINFO GROUPS reports no group left
//
// The cleanup is best-effort and bounded by redisEventGroupCleanupTimeout: a
// Redis that does not answer during Close leaves the groups in place, and
// the same recipe removes them later. A closed bus must not be used again.
// Close is idempotent and safe for concurrent use; the client the bus was
// built on stays open, because the host owns it.
func (b *RedisEventBus) Close() {
	b.once.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.mu.Unlock()
		b.cancel()
		b.destroyGroups()
	})
}

// destroyGroups removes the consumer groups this instance created and the
// streams they were the last group on. It runs after the readers were told
// to stop, and it is best-effort: the bus is already closed, so there is no
// error left to report -- a failure is a leaked group, and the Close notes
// name the operator command that removes it. The event types this instance
// ever subscribed to are the whole scope, read under the readers lock; a
// Subscribe that raced the Close can at worst start a reader whose group
// creation immediately fails on the bus's cancelled context.
func (b *RedisEventBus) destroyGroups() {
	b.readersMu.Lock()
	eventTypes := make([]string, 0, len(b.readers))
	for eventType := range b.readers {
		eventTypes = append(eventTypes, eventType)
	}
	b.readersMu.Unlock()
	if len(eventTypes) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), redisEventGroupCleanupTimeout)
	defer cancel()

	group := redisEventGroupPrefix + b.instanceID
	for _, eventType := range eventTypes {
		stream := redisEventStreamKey(eventType)
		// Destroying this instance's group is the point; deleting the stream
		// is the follow-through for when this instance was its last reader.
		// The stream is deleted only when Redis confirms no group is left on
		// it: another instance may have created its group between the
		// destroy and the check, and that instance's blocked reader must
		// never lose its stream under it.
		_ = b.client.XGroupDestroy(ctx, stream, group).Err()
		groups, err := b.client.XInfoGroups(ctx, stream).Result()
		if err != nil || len(groups) != 0 {
			continue
		}
		_ = b.client.Del(ctx, stream).Err()
	}
}

// Subscribe registers h for the exact event type eventType, mirroring the
// in-memory bus: several handlers may subscribe to the same type, all of
// them are invoked in registration order, and a nil handler is ignored.
//
// The first subscription to a type on this instance starts the type's reader
// goroutine: it creates the type's stream and this instance's consumer group
// when Redis is reachable, retrying in the background until it is, so
// Subscribe itself never blocks on the network and never fails -- an event
// published while its only subscriber's Redis was unreachable is lost for
// that subscriber, which the delivery note above owns up to.
func (b *RedisEventBus) Subscribe(eventType string, h EventHandler) {
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
// sharing the client. The event is first appended to the type's stream --
// the commit point every replica reads from -- and this instance's own
// subscribers then run synchronously, in registration order, exactly as on
// the in-memory bus, their failures collected into one joined error. A
// failed append delivers nothing anywhere and reports the failure; handlers
// of this instance do not run for an event that never made it into the
// stream, so a publish is all-or-nothing across the deployment.
//
// The payload must survive JSON encoding, because that is how it crosses the
// process boundary; a payload that does not (channels, funcs) fails the
// publish before anything is appended or delivered.
func (b *RedisEventBus) Publish(ctx context.Context, evt Event) error {
	if b.isClosed() {
		return ErrEventBusClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	payload, err := json.Marshal(evt.Payload)
	if err != nil {
		return fmt.Errorf("pkgcore: redis event bus: payload of event %q is not JSON-serializable: %w", evt.Type, err)
	}
	if _, err := b.client.XAdd(ctx, &redis.XAddArgs{
		Stream: redisEventStreamKey(evt.Type),
		Values: map[string]interface{}{
			"src":     b.instanceID,
			"tenant":  string(evt.TenantID),
			"payload": string(payload),
		},
		MaxLen: redisEventStreamMaxLen,
		Approx: true,
	}).Result(); err != nil {
		return fmt.Errorf("pkgcore: redis event bus: append event %q failed: %w", evt.Type, err)
	}

	handlers := b.handlersFor(evt.Type)
	if len(handlers) == 0 {
		return nil
	}
	failures := make([]error, 0, len(handlers))
	for i, h := range handlers {
		if err := h(ctx, evt); err != nil {
			failures = append(failures, fmt.Errorf("pkgcore: handler %d for event %q failed: %w", i, evt.Type, err))
		}
	}
	return errors.Join(failures...)
}

// isClosed reports whether Close has been called.
func (b *RedisEventBus) isClosed() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.closed
}

// ensureReader starts the single reader goroutine that delivers remote
// events of eventType on this instance. The first Subscribe of a type on an
// instance owns the start; later ones find the reader already running.
func (b *RedisEventBus) ensureReader(eventType string) {
	b.readersMu.Lock()
	defer b.readersMu.Unlock()
	if _, running := b.readers[eventType]; running {
		return
	}
	b.readers[eventType] = struct{}{}
	go b.runReader(eventType)
}

// runReader consumes the eventType stream on this instance's consumer group
// until the bus is closed. It first creates the group -- retrying in the
// background until Redis answers, so a bus that starts before Redis does
// comes up on its own -- and then reads entries in batches, blocking for
// redisEventReaderBlock when the stream is quiet so that a Close is noticed
// promptly. A stream or group that vanishes under the reader (an operator's
// cleanup, FLUSHALL, a failover) is recreated on the first read that answers
// NOGROUP, so a reader only ever dies with its bus.
func (b *RedisEventBus) runReader(eventType string) {
	stream := redisEventStreamKey(eventType)
	group := redisEventGroupPrefix + b.instanceID

	if err := b.createGroup(b.ctx, stream, group); err != nil {
		return
	}

	for {
		if err := b.ctx.Err(); err != nil {
			return
		}
		entries, err := b.client.XReadGroup(b.ctx, &redis.XReadGroupArgs{
			Group:    group,
			Consumer: b.instanceID,
			Streams:  []string{stream, ">"},
			Count:    redisEventReaderBatch,
			Block:    redisEventReaderBlock,
		}).Result()
		if err != nil {
			// A quiet stream times the block out with no entries (redis.Nil),
			// and a bus closed while the read was waiting is a return at the
			// top of the loop. Everything else is a transient failure worth a
			// short retry -- one case excepted: XREADGROUP answers NOGROUP
			// forever once the stream or this instance's group has vanished
			// (an operator's XGROUP DESTROY or DEL, FLUSHALL, a stream
			// evicted in full, a failover onto a replica that never had it).
			// Treating that as transient would wedge the reader silently, so
			// the group is recreated first; like any group, the recreated one
			// starts at the live end, so entries published while it was gone
			// are not replayed.
			if errors.Is(err, redis.Nil) || b.ctx.Err() != nil {
				continue
			}
			if strings.Contains(err.Error(), "NOGROUP") {
				if err := b.createGroup(b.ctx, stream, group); err != nil {
					return
				}
				continue
			}
			time.Sleep(redisEventRetryDelay)
			continue
		}
		for _, streamEntries := range entries {
			for _, entry := range streamEntries.Messages {
				// A Close that arrived while this batch was in flight ends
				// delivery here. The entries the batch already took off the
				// stream are acknowledged so that, when Close's own cleanup
				// cannot reach Redis to destroy the group, the group it
				// leaves behind carries no pending set nobody will ever
				// process; and none of the batch is delivered: a replica that
				// is shutting down must not keep running handlers for events
				// published after its Close.
				if err := b.ctx.Err(); err != nil {
					b.acknowledge(context.Background(), stream, group, entry.ID)
					return
				}
				b.deliverRemote(b.ctx, eventType, stream, group, entry)
			}
		}
	}
}

// createGroup makes sure the stream and this instance's consumer group on it
// exist, retrying until they do or the bus is closed. The group is created
// at the live end of the stream ("$"), so a group never replays history: a
// subscriber starts receiving events published after its subscription.
func (b *RedisEventBus) createGroup(ctx context.Context, stream, group string) error {
	for {
		if err := b.client.XGroupCreateMkStream(ctx, stream, group, "$").Err(); err == nil {
			return nil
		} else if ctx.Err() != nil {
			return ctx.Err()
		}
		// The instance id makes the group name unique per bus, so a group
		// either exists because this reader created it or does not exist at
		// all; either way, retrying the creation is the whole recovery.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(redisEventRetryDelay):
		}
	}
}

// deliverRemote runs one entry that another instance published: entries this
// instance published itself are acknowledged without dispatch, because the
// Publish call already ran the local handlers synchronously; everything else
// is reconstructed from the JSON envelope and handed to the registered
// handlers, then acknowledged. The per-entry work is deliberately finished
// even when Redis vanished mid-delivery: an entry that could not be
// acknowledged stays pending in the group, which is exactly what an operator
// wants to find when they inspect it.
func (b *RedisEventBus) deliverRemote(ctx context.Context, eventType, stream, group string, entry redis.XMessage) {
	fields := entry.Values
	src, _ := fields["src"].(string)
	if src == b.instanceID {
		b.acknowledge(ctx, stream, group, entry.ID)
		return
	}

	tenant, _ := fields["tenant"].(string)
	rawPayload, _ := fields["payload"].(string)
	var payload interface{}
	if err := json.Unmarshal([]byte(rawPayload), &payload); err != nil {
		// An entry that does not decode is corrupt (or hand-written); it
		// must not wedge the reader, and it must not be delivered, so it is
		// dropped the same way a decoded entry is after its handlers ran.
		b.acknowledge(ctx, stream, group, entry.ID)
		return
	}
	evt := Event{Type: eventType, TenantID: TenantID(tenant), Payload: payload}

	handlers := b.handlersFor(eventType)
	for _, h := range handlers {
		b.runRemoteHandler(ctx, evt, h)
	}
	b.acknowledge(ctx, stream, group, entry.ID)
}

// runRemoteHandler invokes one handler for a remote event, containing the
// panics a handler may raise: on this replica there is no publisher in
// process to receive a panic, so letting it escape would crash the whole
// replica from a reader goroutine. The handler's error is not observable by
// any publisher either; it is dropped by design, see the delivery note on
// RedisEventBus.
func (b *RedisEventBus) runRemoteHandler(ctx context.Context, evt Event, h EventHandler) {
	defer func() {
		_ = recover()
	}()
	_ = h(ctx, evt)
}

// acknowledge removes the entry from the group's pending set. Failures are
// ignored: the entry then simply stays pending, visible to an operator, and
// this reader never reads the pending set again.
func (b *RedisEventBus) acknowledge(ctx context.Context, stream, group, id string) {
	_ = b.client.XAck(ctx, stream, group, id).Err()
}

// handlersFor returns a private snapshot of the handlers subscribed to
// eventType on this instance, copied under the lock so a handler is free to
// call Subscribe or Publish re-entrantly and a concurrent Subscribe cannot
// mutate the slice being iterated.
func (b *RedisEventBus) handlersFor(eventType string) []EventHandler {
	b.mu.RLock()
	defer b.mu.RUnlock()

	registered := b.handlers[eventType]
	if len(registered) == 0 {
		return nil
	}
	snapshot := make([]EventHandler, len(registered))
	copy(snapshot, registered)
	return snapshot
}

// redisEventStreamKey names the stream that carries events of eventType.
func redisEventStreamKey(eventType string) string {
	return redisEventStreamPrefix + eventType
}

// newBusInstanceID returns the random identifier that distinguishes one bus
// instance from every other one sharing a client. It is stamped on every
// entry the instance publishes and names its consumer groups, so a reader
// can skip its own events and two instances never share a group.
func newBusInstanceID() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand failing means the host's entropy source is gone; every
		// later draw would fail the same way, so fail at construction.
		panic(fmt.Sprintf("pkgcore: crypto/rand unavailable: %v", err))
	}
	return hex.EncodeToString(raw[:])
}
