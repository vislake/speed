package pkgcore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// Event is a single domain fact published on an EventBus. It carries the
// routing key (Type), the tenant the fact belongs to, and an opaque payload
// whose concrete type is agreed upon between publisher and subscribers.
type Event struct {
	// Type is the routing key handlers subscribe to. Subscription matching is
	// an exact string comparison; no wildcards or prefixes are interpreted.
	Type string

	// TenantID identifies the tenant the event belongs to. It stays at its
	// zero value for events that are not tenant-scoped (system-wide events).
	TenantID TenantID

	// Payload carries the event data. Handlers are expected to type-assert it
	// to the concrete type documented for the event Type.
	Payload any
}

// EventHandler processes a single published Event. Returning an error reports
// that this handler failed to process the event; it never prevents the
// remaining handlers of the same event from running. Whether the failure
// also reaches the publisher is an implementation property, not a promise of
// the EventBus interface: Publish can surface a handler's error only when it
// ran the handler synchronously on the publisher's own goroutine (see
// EventBus's doc comment for where that line falls in practice).
type EventHandler func(ctx context.Context, evt Event) error

// EventBus decouples modules by letting them exchange domain events instead of
// calling each other directly. Implementations differ in delivery semantics:
// the in-memory bus is synchronous and single-process, while the distributed
// deployment mode's bus, backed by a broker, delivers asynchronously. Error
// reporting follows the delivery path rather than forming a promise of its
// own: Publish can report a handler's failure only when the handler ran
// synchronously on the caller's own goroutine, so whether a given failure
// reaches the caller depends on where the handler ran, never on the
// interface. The in-memory bus runs every handler it delivers to that way
// and reports every failure through its Publish error; a broker-backed bus
// runs its own instance's subscribers that way, while the handlers it
// delivers to on other replicas run on its own reader goroutines, after
// Publish has returned, where no publisher exists to receive a failure --
// each implementation's docs describe how its delivery machinery handles
// those (eventbus/redis's and eventbus/nats's drop them by design,
// acknowledging the delivery; eventbus/postgres's records the delivery and
// retries only a panicked handler). A caller that must treat a subscriber's
// failure as its own therefore cannot rely on the seam to report it: that
// guarantee belongs to the in-memory bus alone, the one implementation whose
// every handler runs in the publisher's own process on the publisher's own
// goroutine.
type EventBus interface {
	// Publish delivers evt to every handler subscribed to evt.Type. What its
	// error reports depends on the delivery path: the failures of the
	// handlers the implementation invoked synchronously on the caller's
	// goroutine (for the in-memory bus that is every handler, and its error
	// is the joined handler failures), plus the implementation's own
	// delivery failures -- a closed bus, a payload that cannot be marshaled,
	// a broker append that failed. A handler failure that occurs on the
	// implementation's own delivery goroutines -- a broker-backed bus's
	// deliveries to other replicas -- happens after Publish has returned and
	// is not observable by any publisher; each implementation's docs say how
	// its machinery handles it. A failing handler never prevents the
	// remaining handlers of the same event from running, whichever delivery
	// path ran it.
	Publish(ctx context.Context, evt Event) error

	// Subscribe registers h for events whose Type equals eventType exactly.
	Subscribe(eventType string, h EventHandler)
}

// memoryEventBus is the standalone deployment mode's EventBus: an in-process,
// synchronous fan-out registry. It is safe for concurrent use by multiple
// goroutines.
type memoryEventBus struct {
	mu       sync.RWMutex
	handlers map[string][]EventHandler
}

// NewMemoryEventBus returns an in-memory EventBus for the single-process
// standalone deployment mode. Publish invokes the subscribed handlers
// synchronously, in registration order, on the calling goroutine, so a
// published event is fully handled by the time Publish returns; a handler
// that panics is contained and logged rather than unwound through the caller
// (see Publish), matching the containment the distributed implementations'
// reader goroutines give a subscriber. The returned bus is safe for
// concurrent Subscribe and Publish calls, and handlers may themselves call
// Subscribe or Publish without deadlocking.
func NewMemoryEventBus() EventBus {
	return &memoryEventBus{handlers: make(map[string][]EventHandler)}
}

// Subscribe registers h for the exact event type eventType. Several handlers
// may subscribe to the same type; all of them are invoked, in the order they
// subscribed. A nil handler is ignored so that a misconfigured caller cannot
// panic an unrelated publisher.
func (b *memoryEventBus) Subscribe(eventType string, h EventHandler) {
	if h == nil {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[eventType] = append(b.handlers[eventType], h)
}

// Publish invokes every handler subscribed to evt.Type synchronously, in
// registration order. A handler that returns an error does not stop the ones
// after it: all failures are collected and returned as a single joined error.
// A handler that panics is contained the same way -- the panic is recovered,
// logged as an Error, and the handlers registered after it still run -- so a
// buggy subscriber can never tear through a Publish call that may be running
// on a background goroutine with no recover of its own (a jobs handler, a
// subscription chain, a periodic scheduler), the containment the distributed
// implementations' delivery machinery gives a subscriber on another replica
// (see eventbus/redis.EventBus's runRemoteHandler and its siblings). The
// recovered panic is logged and dropped, never returned as an error: like
// those implementations' recover blocks it is not a handler failure a caller
// could act on, and a synchronous caller with no other failures gets nil
// exactly as it would if the panicking handler had not been subscribed.
// Publish returns nil when the event has no subscribers or when every handler
// succeeds.
func (b *memoryEventBus) Publish(ctx context.Context, evt Event) error {
	handlers := b.handlersFor(evt.Type)
	if len(handlers) == 0 {
		return nil
	}

	failures := make([]error, 0, len(handlers))
	for i, h := range handlers {
		if err := runMemoryBusHandler(ctx, evt, i, h); err != nil {
			failures = append(failures, fmt.Errorf("pkgcore: handler %d for event %q failed: %w", i, evt.Type, err))
		}
	}

	return errors.Join(failures...)
}

// runMemoryBusHandler invokes one subscribed handler with the containment
// Publish promises. A panic raised by the handler is recovered here rather
// than let to unwind through the publisher: the memory bus is the standalone
// deployment mode's whole delivery machinery, so no reader goroutine of its
// own exists to absorb a panic the way the distributed implementations'
// readers do, and the caller of Publish is frequently a background goroutine
// with no recover of its own. The recovered panic is reported through the
// standard library's log/slog -- pkgcore is the dependency floor of the
// workspace and cannot import go/observability, the same reason
// warnIfNotDurable reaches for slog.Default() directly -- with the handler
// index, the event type and the panic value, and the handler's siblings
// still run.
func runMemoryBusHandler(ctx context.Context, evt Event, i int, h EventHandler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Default().Error("pkgcore: in-process event bus handler panicked; recovered so the event's remaining handlers still run",
				"event_type", evt.Type,
				"handler", i,
				"panic", fmt.Sprintf("%v", r),
			)
		}
	}()
	return h(ctx, evt)
}

// handlersFor returns a private snapshot of the handlers subscribed to
// eventType. Copying the slice under the lock and returning it lets Publish
// release the lock before invoking anything, so a handler is free to call
// Subscribe or Publish re-entrantly, and a concurrent Subscribe cannot mutate
// the slice being iterated.
func (b *memoryEventBus) handlersFor(eventType string) []EventHandler {
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
