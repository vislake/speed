package pkgcore

import (
	"context"
	"errors"
	"fmt"
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
// the failure to the publisher without preventing the remaining handlers of
// the same event from running.
type EventHandler func(ctx context.Context, evt Event) error

// EventBus decouples modules by letting them exchange domain events instead of
// calling each other directly. Implementations differ in delivery semantics:
// the in-memory bus is synchronous and single-process, while a production bus
// backed by a broker delivers asynchronously.
type EventBus interface {
	// Publish delivers evt to every handler subscribed to evt.Type and reports
	// the handler failures, if any.
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
// published event is fully handled by the time Publish returns. The returned
// bus is safe for concurrent Subscribe and Publish calls, and handlers may
// themselves call Subscribe or Publish without deadlocking.
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
// Publish returns nil when the event has no subscribers or when every handler
// succeeds.
func (b *memoryEventBus) Publish(ctx context.Context, evt Event) error {
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
