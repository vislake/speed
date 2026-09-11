package audit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/vislake/speed/go/dbkit"
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
// The completeness expectation has one hard limit, and stating it is the
// point of this sentence: nothing about Diff's shape filters or redacts
// what these maps carry -- changesJSON (module.go) marshals them verbatim
// into the changes column, and the caller is the only redaction layer
// that content will ever pass. The repository's "do not write plaintext
// PII, secrets or tokens into logs, traces or API responses" security
// rule therefore applies to every Diff value with its full force: a Diff
// may record that a field changed, and how (identifiers, structural
// values), but must never carry the sensitive content itself -- no
// plaintext PII (an email address, a phone number, a person's name), no
// keys or tokens, no full prompt or document bodies. Requiring a caller
// to know exactly what changed does not license recording the sensitive
// parts of what changed; when the changed value itself is sensitive, the
// truthful diff is that it changed, not what it now says.
//
// The landing makes that limit non-negotiable in a way no other column's
// content is: the changes column is written to the one table in this
// system with no published path to delete from. audit.Repository offers
// no Update or Delete method at all; dbkit.Repository[T]'s HardDelete --
// the compliance-erasure path -- cannot even be instantiated against
// AuditEvent, which deliberately does not implement dbkit.TenantScoped
// (model.go's own doc comment on why that absence is load-bearing); the
// append-only trigger set refuses UPDATE and DELETE at the database
// itself; and compliance only ever reads this table. Anything written
// here is effectively permanent, so a caller must write nothing into a
// Diff that it could not leave in the audit trail forever.
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
	// IP, UserAgent and TraceID are the request-context values read from
	// ctx via dbkit.RequestMetadataFromContext, each the empty string when
	// ctx carried no RequestMetadata — a background job with no request
	// behind it. They travel on the event for the same reason Actor and
	// TenantID do: the persister that turns this event into an audit_events
	// row runs on the subscriber side of a bus that may have crossed a real
	// network hop, where the subscriber's ctx is not the publisher's ctx.
	IP        string
	UserAgent string
	TraceID   string
	// Action, Resource, Result and Changes are copied verbatim from Input.
	Action   string
	Resource Resource
	Result   Result
	Changes  *Diff
	// OccurredAt is when Emit was called.
	OccurredAt time.Time
}

// Emit is the declarative-secondary collection mechanism: a business module
// calls it directly, at the point it already knows a qualified action name
// ("org.member.remove") and, optionally, a rich before/after diff -- the
// two things the automatic GORM write-capture plugin (dbkit's
// audit_capture.go) cannot generically infer.
//
// Emit reads the acting identity from ctx exactly the way the automatic
// capture plugin does -- pkgcore.ActorFromContext, pkgcore.
// OnBehalfOfFromContext, pkgcore.TenantFromContext -- so the dual-identity
// shape (Actor plus the OnBehalfOf administrator behind an impersonation)
// is populated identically regardless of which of the two mechanisms
// produced a given AuditEvent.
//
// in.Action is validated against actions.Actions() before anything is
// published: an action string no module ever registered through
// AuditActionRegistrar.Add is a caller bug, and Emit rejects it with
// ErrActionNotRegistered rather than silently recording an event under an
// undeclared vocabulary -- mirroring how pkgcore.RegisterSystemPurpose
// gates pkgcore.SystemReason.Purpose. The registered enumeration is the
// mapping contract the trail's query surfaces filter on, so every
// recorded action is one somebody declared.
//
// A publish failure is returned to the caller, never swallowed: that is
// Emit's half of the never-drop rule -- an audit-write failure must alert,
// never be silently dropped. The other half is the caller's: a caller
// that discards Emit's returned error has done the very silent drop the
// rule exists to prevent, and nothing inside this package can detect it
// after the fact -- the obligation is written into the contract because
// the contract is the only place this package can hold it. The reference
// app and go/authn model the recommended caller half -- a failure is
// logged as a structured error (authn/handler.go's recordAudit logs
// "authn audit event emit failed" with the action and error), never
// dropped without a trace.
//
// This is deliberately NOT the automatic capture plugin's contract: the
// plugin runs after the write it describes has already durably committed
// (audit_capture.go's own doc comment says exactly that, and why), so it
// has nothing left to roll back or fail closed through and reports a
// structured alert instead -- alert-and-continue is the plugin's whole
// fulfilment of the shared rule, the drop half being inherent to its
// post-commit position. Emit's position is different: the persister's
// write has not happened yet, so Emit CAN fail closed, by returning the
// error -- and the caller half then belongs to the caller. The division
// of fulfilment is the two mechanisms' shared contract.
//
// The nil-seat decision is the caller half's other obligation, settled
// BEFORE Emit is ever called: what a caller whose audit wiring is missing
// -- a nil EventBus or a nil registrar, the shape a host that wired the
// module by hand, outside the assembly, leaves behind -- does
// with the operation it was about to record. (The guard itself is
// mandatory either way: Emit dereferences both seats, so a nil seat means
// Emit cannot be called at all; the question is only what the caller does
// with the operation instead.) Callers across this repository answer that
// question two opposite ways, and the answer is not a matter of taste: it
// follows the same before-and-after axis this comment just drew for the
// plugin. A caller that records a POST-HOC FACT about an operation that
// has already durably committed and been answered -- a login, a
// configuration write, a revocation, a credit mutation, each with that
// rationale written at its own site -- is recording metadata, not a gate:
// the record's loss changes nothing about the operation's legitimacy, and
// there is nothing left to refuse (the commit already happened and the
// caller was already answered), so the honest posture is to skip the
// attempt WITH an alert -- the per-call "log, never drop" obligation
// above, and a once-per-lifetime announcement for a seat that will never
// record anything -- rather than to turn the operation's already-given
// answer into a failure. A caller that records the PRECONDITION OF AN
// OPERATION'S LEGITIMACY must fail closed instead: the audited
// system-context escape-hatch grant (an unrecorded grant IS the gap that
// operation exists to close), the sensitive share whose creation the
// disclosure itself is the audit event of -- for these, an operation that
// proceeds unrecorded is the very failure the audit exists to prevent, and
// the operation has not been answered yet, so there is still something to
// refuse: a missing seat must refuse the operation, never proceed
// unrecorded. The discriminator is timing, not taste: a post-commit record
// can no longer refuse anything, so skip-with-alert is its only honest
// posture; a pre-answer gate still can refuse, and must, whenever the
// record is the gate. The rule of thumb a new call site can apply before
// reading either pole's own written reason: could the operation still be
// undone, or its answer still withheld, at the moment the audit write
// fails? If not -- the write committed and the caller was answered -- it
// is post-hoc metadata and may skip with an alert; if yes -- the audit is
// what legitimizes what is about to be answered -- it must refuse.
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
	if md, ok := dbkit.RequestMetadataFromContext(ctx); ok {
		evt.IP = md.IP
		evt.UserAgent = md.UserAgent
		evt.TraceID = md.TraceID
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
