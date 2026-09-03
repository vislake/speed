package config

import (
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// EventConfigItemChanged is the pkgcore.Event.Type published after every
// successful Set, following the "<module>.<entity>.<action>" convention
// (backend coding standard §8; go/pkgcore/registry.go's EventDecl.Type doc
// comment). Its Payload is an ItemChangedEvent.
//
// The event is the module's hot-update signal (docs/internal/
// 11-cross-cutting.md's dynamic-config section: a change is broadcast
// over the event bus and every instance refreshes its local cache) and
// its audit trail in one: a future
// audit-log consumer (the compliance module) can record who changed what,
// when, and from which value to which value purely by subscribing to this
// one event type -- which is why every field an audit record needs is on
// the payload rather than left to be re-derived by the subscriber.
const EventConfigItemChanged = "config.item.changed"

// eventConfigItemChangedPayloadType names ItemChangedEvent for
// EventDecl.PayloadType, so a subscriber (and the future event catalog)
// knows what concrete type to expect in Event.Payload without importing
// this package just to read a string.
const eventConfigItemChangedPayloadType = "config.ItemChangedEvent"

// AuditActionConfigSet is the module's audit action string, declared on the
// registry's audit-action registrar the way notes declares its own. It
// records what operation was performed -- "set" -- deliberately distinct
// from EventConfigItemChanged's past-tense fact "changed": the audit trail
// records the operation, the event records what became true as a result
// (the same distinction notes documents for its own pair). Nothing
// persists audit records in this milestone; declaring the enumeration is
// what keeps the future audit-log consumer's vocabulary complete.
const AuditActionConfigSet = "config.item.set"

// redactedMarker is what ItemChangedEvent carries in place of a Sensitive
// item's actual old or new canonical value. The marker is a constant so a
// subscriber can recognize it; the actual value never leaves this process
// on the bus (see redactIf).
const redactedMarker = "[redacted]"

// ItemChangedEvent is the concrete type carried in the pkgcore.Event.Payload
// of every EventConfigItemChanged event -- the payload shape
// eventConfigItemChangedPayloadType names for EventDecl.PayloadType.
//
// OldValue and NewValue hold the canonical forms of the previous and the
// new value (see values.go). For a Sensitive item both carry the
// redactedMarker constant instead -- the values themselves never travel on
// the bus, because the bus is a log-adjacent artifact and a secret on it is
// a secret in a log. A subscriber that needs the actual value re-reads it
// through the Service, which decrypts in process.
//
// Sensitive is the item's own flag, carried separately from the redacted
// markers so that a subscriber decides on "was this a Sensitive item's
// change?" from the declaration, never by pattern-matching marker text
// against values (a plain string item could legitimately hold text that
// merely looks like the marker).
type ItemChangedEvent struct {
	// Key is the configuration key that changed, whether a ConfigItem key
	// or a FeatureFlag key (they share one key space and one table).
	Key string

	// Scope is the scope tier the row was written at: ScopeSystem or
	// ScopeTenant. ScopeUser rows cannot exist in this milestone.
	Scope Scope

	// TenantID is the owning tenant when Scope is ScopeTenant, empty
	// otherwise. Carried as a plain string since the payload is a
	// wire-shaped event type, not a pkgcore-typed one.
	TenantID string

	// Actor is the actor of the Set that produced this change -- who the
	// audit trail must be able to blame. It mirrors the row's updated_by
	// column.
	Actor string

	// OldValue is the canonical form of the value before the Set, or "" on
	// a first write of a key that had no row (and no cached absence).
	// Redacted for Sensitive items.
	OldValue string

	// NewValue is the canonical form of the value after the Set. Redacted
	// for Sensitive items.
	NewValue string

	// Sensitive reports whether the changed item is a Sensitive one. When
	// true, OldValue and NewValue both hold the redactedMarker and no part
	// of the payload carries the real value.
	Sensitive bool

	// ChangedAt is when the Set happened (the row's updated_at).
	ChangedAt time.Time
}

// redactIf replaces canonical with the redactedMarker when sensitive is
// true, and passes it through otherwise. It is the single place the module
// decides whether a value may leave the process on the bus; every event
// construction goes through it.
func redactIf(sensitive bool, canonical string) string {
	if sensitive {
		return redactedMarker
	}
	return canonical
}

// eventDecl is the declaration this module registers for its one event
// type. It lives here next to the payload so the type string, the payload
// type name and the description cannot drift apart.
var eventDecl = pkgcore.EventDecl{
	Type:        EventConfigItemChanged,
	PayloadType: eventConfigItemChangedPayloadType,
	Description: "Published after a configuration value is set, carrying the actor, scope and old-to-new canonical values (redacted for sensitive items) for hot-update cache invalidation and the audit trail.",
}
