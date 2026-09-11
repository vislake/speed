package pkgcore

import (
	"context"
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
