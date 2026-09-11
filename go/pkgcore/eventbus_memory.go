package pkgcore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

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
