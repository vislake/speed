package audit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// EventRecorded is the pkgcore.Event.Type Emit publishes on every
// successful call, following the "<module>.<entity>.<action>" convention
// (backend coding standard §8). Its Payload is a RecordedEvent.
const EventRecorded = "audit.event.recorded"

// eventRecordedPayloadType names RecordedEvent for pkgcore.EventDecl's
// PayloadType, so a subscriber (and the event catalog) knows what concrete
// type to expect in Event.Payload without importing this package just to
// read a string -- matching go/config's identical
// eventConfigItemChangedPayloadType convention.
const eventRecordedPayloadType = "audit.RecordedEvent"

// ErrActionNotRegistered is returned by Emit when Input.Action was never
// declared on the given pkgcore.AuditActionRegistrar through Add. It
// mirrors pkgcore.ErrSystemPurposeNotRegistered's gate on SystemReason.
// Purpose: an unregistered action string is a caller bug, not a
// recoverable runtime condition, so Emit rejects it loudly rather than
// silently recording an event under a vocabulary nobody declared.
var ErrActionNotRegistered = errors.New("audit: action is not registered on the given AuditActionRegistrar")

// Diff carries an optional before/after change set for an explicitly
// emitted audit event -- the "Changes" element of the six-element
// AuditEvent shape (model.go). Both fields are freeform, caller-supplied
// maps (never auto-derived the way the GORM write-capture plugin's After
// snapshot is): a caller using Emit is expected to know, and state,
// exactly what changed.
//
// Deliberately no json struct tags: every event payload type in this
// package and in dbkit (WriteCapturedEvent, RecordedEvent, Actor,
// Resource, Result) relies on encoding/json's default behavior --
// marshaling each exported field under its own Go name -- so that a
// module.go wire-decode helper faced with a distributed-bus JSON map can
// look fields up by one consistent capitalized key ("Before", "After")
// everywhere, rather than tracking a different convention per type.
type Diff struct {
	Before map[string]any
	After  map[string]any
}

// Input is what a caller passes to Emit: everything about an audited
// action except who performed it, which Emit reads from ctx instead (see
// Emit's own doc comment) so every caller populates identity the same way.
type Input struct {
	// Action is the audit action string, validated against actions.
	// Actions() before anything is published.
	Action string
	// Resource is what the action was performed on.
	Resource Resource
	// Result is the action's outcome.
	Result Result
	// Changes is an optional before/after diff. Nil when the action has no
	// diff worth recording (a read, a state transition with no field-level
	// change).
	Changes *Diff
}

// RecordedEvent is the Payload carried by an EventRecorded event. Like
// dbkit.WriteCapturedEvent, every field a subscriber needs is embedded
// directly rather than left to be re-derived from ctx: the distributed
// deployment mode's EventBus delivers across a real network hop, where a
// subscriber's ctx is not the publisher's ctx.
type RecordedEvent struct {
	// Actor is the acting identity, read from ctx via
	// pkgcore.ActorFromContext. The zero pkgcore.Actor when ctx carried
	// none.
	Actor pkgcore.Actor
	// OnBehalfOf is the real administrator behind an impersonated Actor,
	// read from ctx via pkgcore.OnBehalfOfFromContext. Nil when ctx
	// carried none.
	OnBehalfOf *pkgcore.Actor
	// TenantID is read from ctx via pkgcore.TenantFromContext, empty for a
	// platform-level action.
	TenantID string
	// Action, Resource, Result and Changes are copied verbatim from Input.
	Action   string
	Resource Resource
	Result   Result
	Changes  *Diff
	// OccurredAt is when Emit was called.
	OccurredAt time.Time
}

// Emit is the declarative-secondary collection mechanism
// docs/internal/10-compliance-and-audit.md describes: a business module
// calls it directly, at the point it already knows a qualified action name
// ("org.member.remove") and, optionally, a rich before/after diff -- the
// two things the automatic GORM write-capture plugin (dbkit's
// audit_capture.go) cannot generically infer.
//
// Emit reads the acting identity from ctx exactly the way the automatic
// capture plugin does -- pkgcore.ActorFromContext, pkgcore.
// OnBehalfOfFromContext, pkgcore.TenantFromContext -- so the dual-identity
// shape (root CLAUDE.md's impersonation rule) is populated identically
// regardless of which of the two mechanisms produced a given AuditEvent.
//
// in.Action is validated against actions.Actions() before anything is
// published: an action string no module ever registered through
// AuditActionRegistrar.Add is a caller bug, and Emit rejects it with
// ErrActionNotRegistered rather than silently recording an event under an
// undeclared vocabulary -- mirroring how pkgcore.RegisterSystemPurpose
// gates pkgcore.SystemReason.Purpose. This is what closes the loop
// go/config's own AuditActionConfigSet declaration left open: declaring an
// action was always a documentation and mapping contract, and Emit is now
// the thing that contract gates.
//
// A publish failure is returned to the caller, never swallowed -- per
// docs/internal/10-compliance-and-audit.md's rule that an audit-write
// failure must alert and never be silently dropped, matching the automatic
// capture plugin's own loud-failure contract.
func Emit(ctx context.Context, bus pkgcore.EventBus, actions pkgcore.AuditActionRegistrar, in Input) error {
	if !slices.Contains(actions.Actions(), in.Action) {
		return fmt.Errorf("%w: %q", ErrActionNotRegistered, in.Action)
	}

	evt := RecordedEvent{
		Action:     in.Action,
		Resource:   in.Resource,
		Result:     in.Result,
		Changes:    in.Changes,
		OccurredAt: time.Now(),
	}
	if actor, ok := pkgcore.ActorFromContext(ctx); ok {
		evt.Actor = actor
	}
	if onBehalfOf, ok := pkgcore.OnBehalfOfFromContext(ctx); ok {
		copyOf := onBehalfOf
		evt.OnBehalfOf = &copyOf
	}
	if tenant, ok := pkgcore.TenantFromContext(ctx); ok {
		evt.TenantID = string(tenant)
	}

	err := bus.Publish(ctx, pkgcore.Event{
		Type:     EventRecorded,
		TenantID: pkgcore.TenantID(evt.TenantID),
		Payload:  evt,
	})
	if err != nil {
		return fmt.Errorf("audit: publish %s: %w", EventRecorded, err)
	}
	return nil
}
