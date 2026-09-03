package audit

import (
	"context"
	"embed"
	"encoding/json"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit/migrations"
	"github.com/vislake/speed/go/pkgcore"
)

// moduleName is Module's pkgcore.Module.Name().
const moduleName = "audit"

// tenancySystemContextEnteredEventType mirrors go/tenancy's
// EventSystemContextEntered constant ("tenancy.system_context.entered")
// verbatim, WITHOUT importing package tenancy: go/tenancy's go.mod
// requires go/dbkit (tenancy sits above dbkit in the module dependency
// graph -- pkgcore -> dbkit -> tenancy -> config/jobs -> ...), and this
// package is a subpackage of dbkit itself, so importing tenancy from here
// would make dbkit depend on tenancy -- inverting the direction root
// CLAUDE.md's "Dependencies flow strictly bottom-up" rule requires and
// reintroducing exactly the module cycle that rule exists to prevent. This
// is the same conflict model_test.go already documents for
// tenancytest.AssertNotTenantScoped; this round's scope-freeze report left
// the exact subscription wiring open for the implementing round (§3's
// adjudication on whether tenancy gains its own Register()), and this is
// that round's resolution: subscribe by the bare string, and decode the
// payload structurally (decodeSystemContextEntered, below) instead of by
// importing tenancy.SystemContextEnteredEvent's concrete type.
//
// A future round giving tenancy its own pkgcore.Module (still open per the
// scope-freeze report) can replace this constant with a genuine import
// once dbkit is no longer a dependency tenancy sits above -- or, more
// likely, never needs to, since the event's wire shape is unlikely to
// change independently of this constant.
const tenancySystemContextEnteredEventType = "tenancy.system_context.entered"

// AuditActionSystemContextEntered is registered on the host's
// AuditActionRegistrar by Register, on tenancy's behalf, since go/tenancy
// has no pkgcore.Module of its own (see AGENTS.md's "Module home" and its
// own doc comment) to declare its audit-action vocabulary. It names the
// same string tenancySystemContextEnteredEventType does; the two are kept
// as separate constants because they answer different questions (an audit
// action vs. an event type) that happen to share one string value here,
// matching go/config's own AuditActionConfigSet vs. EventConfigItemChanged
// distinction.
const AuditActionSystemContextEntered = tenancySystemContextEnteredEventType

// Module implements pkgcore.Module: the persister that subscribes to both
// collection mechanisms' published events (dbkit.EventWriteCaptured,
// EventRecorded) plus tenancy's already-shipped
// EventSystemContextEntered, normalizes each into an AuditEvent, and
// stores it through Repository.Insert.
//
// Unlike a business module, Module owns no HTTP surface and declares no
// configuration schema or feature flags -- its whole contribution is the
// three subscriptions Register installs plus the one event type
// (EventRecorded) and the one audit action
// (AuditActionSystemContextEntered) it declares.
type Module struct {
	repo *Repository
}

// New returns a Module persisting into db. db is expected to come from
// dbkit.Open, with audit's migrations.FS already applied through a
// dbkit.MigrationRegistry -- identical contract to go/config's NewModule.
// Constructing a Module performs no I/O.
func New(db *gorm.DB) *Module {
	return &Module{repo: NewRepository(db)}
}

// Name implements pkgcore.Module.
func (m *Module) Name() string { return moduleName }

// DependsOn implements pkgcore.Module. Module depends on nothing: it
// subscribes to events other modules publish, but a pkgcore.Module's
// DependsOn only orders migrations and registration, and Module's own
// subscriptions are valid to install regardless of registration order (a
// subscription installed before its publisher registers, or after, both
// work identically -- see pkgcore's EventRegistrar doc comment).
func (m *Module) DependsOn() []string { return nil }

// Migrations implements pkgcore.Module.
func (m *Module) Migrations() embed.FS { return migrations.FS }

// Locales implements pkgcore.Module: Module ships no user-facing messages
// in this milestone (there is no query/report API yet -- that is M4,
// go/compliance), so it contributes an empty file set.
func (m *Module) Locales() embed.FS { return embed.FS{} }

// OpenAPISpec implements pkgcore.Module: nil. Module mounts no HTTP route
// in this milestone.
func (m *Module) OpenAPISpec() []byte { return nil }

// Register implements pkgcore.Module. Per the interface's contract ("It
// must not perform I/O; it only declares"), it declares Module's one
// published event type, the audit action it records on tenancy's behalf,
// and installs its three subscriptions.
//
// It also declares dbkit.EventWriteCaptured on reg.Events, even though
// that event's Type constant is defined in dbkit's root package: dbkit
// itself is not a pkgcore.Module (it has no Register to call this from),
// and Module is that event's one real subscriber and therefore its
// closest thing to an owning module for cataloging purposes.
func (m *Module) Register(reg *pkgcore.Registry) error {
	if err := reg.Events.Publishes(
		pkgcore.EventDecl{
			Type:        dbkit.EventWriteCaptured,
			PayloadType: "dbkit.WriteCapturedEvent",
			Description: "Published by dbkit's automatic GORM write-capture plugin after a successful Create, Update or Delete against a model implementing dbkit.Auditable. Declared here, on dbkit's behalf, since dbkit itself is not a pkgcore.Module.",
		},
		pkgcore.EventDecl{
			Type:        EventRecorded,
			PayloadType: eventRecordedPayloadType,
			Description: "Published by audit.Emit after a business module explicitly records an audited action, carrying the actor, resource, result and an optional before/after diff.",
		},
	); err != nil {
		return err
	}
	if err := reg.AuditActions.Add(AuditActionSystemContextEntered); err != nil {
		return err
	}

	reg.Events.Subscribe(dbkit.EventWriteCaptured, m.onWriteCaptured)
	reg.Events.Subscribe(EventRecorded, m.onRecorded)
	reg.Events.Subscribe(tenancySystemContextEnteredEventType, m.onSystemContextEntered)
	return nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)

// onWriteCaptured normalizes a dbkit.WriteCapturedEvent into an AuditEvent
// and persists it. Action defaults to "<resource_type>.<operation>" (e.g.
// "note.create") -- deliberately honest about what a generic diff-capture
// can know, per this round's scope-freeze report; a business module
// wanting a qualified action name uses Emit instead.
//
// A payload that does not decode as a WriteCapturedEvent (neither the
// concrete struct the standalone in-memory bus delivers, nor the JSON map
// shape the distributed bus's Redis Streams transport reconstructs it as)
// is dropped without failing the handler chain, mirroring go/config's
// itemChangedFromWire convention: an event this subscriber cannot make
// sense of must not wedge every other subscriber of the same event.
func (m *Module) onWriteCaptured(ctx context.Context, evt pkgcore.Event) error {
	payload, ok := writeCapturedFromWire(evt.Payload)
	if !ok {
		return nil
	}

	row := &AuditEvent{
		ID:         auditDeterministicEventID(dbkit.EventWriteCaptured, payload.OccurredAt, payload.TenantID, payload.ResourceType, payload.ResourceID, payload.Operation, string(payload.Actor.Type), payload.Actor.ID),
		Action:     payload.ResourceType + "." + payload.Operation,
		TenantID:   payload.TenantID,
		OccurredAt: payload.OccurredAt,
	}
	row.SetActor(payload.Actor)
	row.SetOnBehalfOf(payload.OnBehalfOf)
	row.SetResource(Resource{Type: payload.ResourceType, ID: payload.ResourceID})
	row.SetResult(Result{Success: true})
	row.Changes = changesJSON(payload.Before, payload.After)
	return m.repo.InsertIdempotent(ctx, row)
}

// onRecorded normalizes a RecordedEvent (Emit's own event type) into an
// AuditEvent and persists it. See onWriteCaptured's doc comment for why an
// undecodable payload is dropped rather than failing the handler chain.
func (m *Module) onRecorded(ctx context.Context, evt pkgcore.Event) error {
	payload, ok := recordedFromWire(evt.Payload)
	if !ok {
		return nil
	}

	row := &AuditEvent{
		ID:         auditDeterministicEventID(EventRecorded, payload.OccurredAt, payload.TenantID, payload.Action, payload.Resource.Type, payload.Resource.ID, string(payload.Actor.Type), payload.Actor.ID),
		Action:     payload.Action,
		TenantID:   payload.TenantID,
		OccurredAt: payload.OccurredAt,
	}
	row.SetActor(payload.Actor)
	row.SetOnBehalfOf(payload.OnBehalfOf)
	row.SetResource(payload.Resource)
	row.SetResult(payload.Result)
	if payload.Changes != nil {
		row.Changes = changesJSON(payload.Changes.Before, payload.Changes.After)
	}
	return m.repo.InsertIdempotent(ctx, row)
}

// onSystemContextEntered normalizes tenancy's already-shipped
// EventSystemContextEntered into an AuditEvent and persists it -- closing
// docs/internal/10-compliance-and-audit.md's requirement that every use of
// the system context is itself an audit event, for a mechanism that
// already exists in production, at the cost of only this one subscriber.
// See decodeSystemContextEntered's doc comment for why the payload is
// decoded structurally rather than by importing tenancy's concrete event
// type.
func (m *Module) onSystemContextEntered(ctx context.Context, evt pkgcore.Event) error {
	actor, purpose, ticket, enteredAt, ok := decodeSystemContextEntered(evt.Payload)
	if !ok {
		return nil
	}

	row := &AuditEvent{
		ID:         auditDeterministicEventID(tenancySystemContextEnteredEventType, enteredAt, string(evt.TenantID), actor, purpose, ticket),
		Action:     tenancySystemContextEnteredEventType,
		TenantID:   string(evt.TenantID),
		OccurredAt: enteredAt,
	}
	row.SetActor(pkgcore.Actor{Type: pkgcore.ActorTypeSystem, ID: actor})
	row.SetResource(Resource{Type: "tenancy.system_context", ID: purpose})
	row.SetResult(Result{Success: true})
	if ticket != "" {
		row.Changes = changesJSON(nil, map[string]any{"ticket": ticket})
	}
	return m.repo.InsertIdempotent(ctx, row)
}

// auditEventIDNamespace is a fixed, arbitrary UUID used as the namespace
// argument to uuid.NewSHA1 in auditDeterministicEventID, exactly the way
// any UUIDv5 namespace constant is meant to be used: fixed so the same
// input always yields the same output, arbitrary because nothing outside
// this package ever needs to recognize it as meaningful. Generated once
// with `uuidgen`; never reused as a namespace for anything else.
var auditEventIDNamespace = uuid.MustParse("6f2a9b3e-6c9c-4e3f-8f1a-9d9b7e2b7b41")

// auditDeterministicEventID derives an AuditEvent.ID deterministically
// from eventType, occurredAt and parts, using uuid.NewSHA1 (a UUIDv5-style,
// namespace-and-content hash) rather than the random UUIDs Repository.Insert
// generates by default.
//
// It exists so that Module's three event subscribers (onWriteCaptured,
// onRecorded, onSystemContextEntered) can call Repository.InsertIdempotent
// with an ID that is the SAME across every replica that independently
// receives the SAME underlying event -- see InsertIdempotent's doc comment
// for why that is necessary in distributed deployment mode -- while still
// being (for all practical purposes) distinct for any two genuinely
// different events. occurredAt is taken at nanosecond precision
// (time.RFC3339Nano) specifically so that two distinct real actions on the
// same resource by the same actor essentially never collide: they would
// have to share the same tenant, resource, actor, operation AND the same
// nanosecond timestamp, which does not happen outside of the exact
// redelivery this function exists to detect. occurredAt is formatted as
// UTC RFC3339Nano rather than hashed as a time.Time value directly because
// a time.Time decoded from JSON (the shape a remote replica's copy of the
// SAME event takes -- see writeCapturedFromWire/recordedFromWire) is not
// == or reflect.DeepEqual to the original in-process value (no monotonic
// reading, and Location may differ); formatting first normalizes both to
// an identical string.
func auditDeterministicEventID(eventType string, occurredAt time.Time, parts ...string) string {
	all := make([]string, 0, len(parts)+2)
	all = append(all, eventType, occurredAt.UTC().Format(time.RFC3339Nano))
	all = append(all, parts...)
	// \x1f (the ASCII "unit separator") joins the parts so that, e.g.,
	// parts {"ab", "c"} and {"a", "bc"} never hash identically -- a plain
	// "" or "|" separator could collide across a field boundary a real
	// tenant ID, resource ID or actor ID might one day contain.
	data := strings.Join(all, "\x1f")
	return uuid.NewSHA1(auditEventIDNamespace, []byte(data)).String()
}

// changesJSON marshals before and after into a Diff-shaped
// datatypes.JSON value for AuditEvent.Changes, or returns nil when both are
// empty -- there is nothing to store, and a nil column is more honest than
// an empty-but-present JSON object.
func changesJSON(before, after map[string]any) []byte {
	if len(before) == 0 && len(after) == 0 {
		return nil
	}
	b, err := json.Marshal(Diff{Before: before, After: after})
	if err != nil {
		// json.Marshal only fails on a value it cannot represent at all
		// (a channel, a function, a cyclic map) -- not a realistic shape
		// for the plain map[string]any this package ever builds Diff
		// from. Dropping Changes rather than failing Insert keeps a
		// pathological caller-supplied map from turning an otherwise
		// valid audit record into a lost one.
		return nil
	}
	return b
}

// stringFromWire returns v as a string when it is one, "" otherwise. It
// keeps every *FromWire helper below one line per optional field, matching
// go/config/events.go's identical stringOf convention.
func stringFromWire(v any) string {
	s, _ := v.(string)
	return s
}

// timeFromWire parses v as an RFC3339Nano timestamp when it is a string
// (the shape a time.Time takes once JSON round-tripped), returning the
// zero time.Time otherwise -- mirroring go/config's itemChangedFromJSONMap
// ChangedAt handling.
func timeFromWire(v any) time.Time {
	s, ok := v.(string)
	if !ok {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// actorFromWire decodes v into a pkgcore.Actor, accepting either the
// concrete struct (the standalone in-memory bus's own delivery shape) or
// the map[string]any shape encoding/json produces for it once
// JSON-round-tripped by the distributed bus.
func actorFromWire(v any) pkgcore.Actor {
	switch a := v.(type) {
	case pkgcore.Actor:
		return a
	case map[string]any:
		return pkgcore.Actor{
			Type:        pkgcore.ActorType(stringFromWire(a["Type"])),
			ID:          stringFromWire(a["ID"]),
			DisplayName: stringFromWire(a["DisplayName"]),
		}
	default:
		return pkgcore.Actor{}
	}
}

// onBehalfOfFromWire decodes v into a *pkgcore.Actor, accepting a
// *pkgcore.Actor, a nil interface (no impersonation), or the
// map[string]any shape a non-nil *pkgcore.Actor round-trips to as JSON.
func onBehalfOfFromWire(v any) *pkgcore.Actor {
	switch a := v.(type) {
	case *pkgcore.Actor:
		return a
	case map[string]any:
		copyOf := actorFromWire(a)
		return &copyOf
	default:
		return nil
	}
}

// resourceFromWire decodes v into a Resource, accepting the concrete
// struct or its map[string]any JSON shape.
func resourceFromWire(v any) Resource {
	switch r := v.(type) {
	case Resource:
		return r
	case map[string]any:
		return Resource{
			Type:        stringFromWire(r["Type"]),
			ID:          stringFromWire(r["ID"]),
			DisplayName: stringFromWire(r["DisplayName"]),
		}
	default:
		return Resource{}
	}
}

// resultFromWire decodes v into a Result, accepting the concrete struct or
// its map[string]any JSON shape.
func resultFromWire(v any) Result {
	switch r := v.(type) {
	case Result:
		return r
	case map[string]any:
		success, _ := r["Success"].(bool)
		return Result{Success: success, FailureReason: stringFromWire(r["FailureReason"])}
	default:
		return Result{}
	}
}

// stringMapFromWire decodes v into a map[string]any, accepting the
// concrete map (the standalone bus's own delivery shape, and
// encoding/json's own decode-to-any result for a JSON object) or nil.
func stringMapFromWire(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// diffFromWire decodes v into a *Diff, accepting a *Diff, a nil interface
// (no diff), or the map[string]any shape a non-nil *Diff round-trips to.
func diffFromWire(v any) *Diff {
	switch d := v.(type) {
	case *Diff:
		return d
	case map[string]any:
		return &Diff{Before: stringMapFromWire(d["Before"]), After: stringMapFromWire(d["After"])}
	default:
		return nil
	}
}

// writeCapturedFromWire decodes payload into a dbkit.WriteCapturedEvent,
// accepting the concrete struct (standalone in-memory bus) or the
// map[string]any shape encoding/json produces once it has crossed the
// distributed bus's Redis Streams transport. ok is false for any other
// shape, in which case the caller drops the event -- see onWriteCaptured's
// doc comment.
func writeCapturedFromWire(payload any) (dbkit.WriteCapturedEvent, bool) {
	switch p := payload.(type) {
	case dbkit.WriteCapturedEvent:
		return p, true
	case map[string]any:
		resourceType, hasType := p["ResourceType"].(string)
		if !hasType {
			return dbkit.WriteCapturedEvent{}, false
		}
		return dbkit.WriteCapturedEvent{
			Actor:        actorFromWire(p["Actor"]),
			OnBehalfOf:   onBehalfOfFromWire(p["OnBehalfOf"]),
			TenantID:     stringFromWire(p["TenantID"]),
			Table:        stringFromWire(p["Table"]),
			ResourceType: resourceType,
			ResourceID:   stringFromWire(p["ResourceID"]),
			Operation:    stringFromWire(p["Operation"]),
			Before:       stringMapFromWire(p["Before"]),
			After:        stringMapFromWire(p["After"]),
			OccurredAt:   timeFromWire(p["OccurredAt"]),
		}, true
	default:
		return dbkit.WriteCapturedEvent{}, false
	}
}

// recordedFromWire decodes payload into a RecordedEvent, accepting the
// concrete struct or its map[string]any JSON shape. ok is false for any
// other shape.
func recordedFromWire(payload any) (RecordedEvent, bool) {
	switch p := payload.(type) {
	case RecordedEvent:
		return p, true
	case map[string]any:
		action, hasAction := p["Action"].(string)
		if !hasAction {
			return RecordedEvent{}, false
		}
		return RecordedEvent{
			Actor:      actorFromWire(p["Actor"]),
			OnBehalfOf: onBehalfOfFromWire(p["OnBehalfOf"]),
			TenantID:   stringFromWire(p["TenantID"]),
			Action:     action,
			Resource:   resourceFromWire(p["Resource"]),
			Result:     resultFromWire(p["Result"]),
			Changes:    diffFromWire(p["Changes"]),
			OccurredAt: timeFromWire(p["OccurredAt"]),
		}, true
	default:
		return RecordedEvent{}, false
	}
}

// decodeSystemContextEntered reads the Actor, Purpose, Ticket and
// EnteredAt fields off payload without importing tenancy's concrete
// SystemContextEnteredEvent type -- see tenancySystemContextEnteredEventType's
// doc comment for why that import is unavailable to this package.
//
// It accepts two shapes: the map[string]any a JSON round trip (or a
// deliberately payload-agnostic caller) produces, and -- via reflection --
// any struct exposing string fields named Actor, Purpose and Ticket plus a
// time.Time field named EnteredAt, which is exactly
// tenancy.SystemContextEnteredEvent's own shape as the standalone
// in-memory bus delivers it (a concrete struct value, never a map). ok is
// false, and every other result the zero value, when payload matches
// neither shape or its Actor is empty (mirroring
// pkgcore.WithSystemContext's own "Actor is mandatory" rule -- a system
// context grant with no actor could never have been recorded in the first
// place).
func decodeSystemContextEntered(payload any) (actor, purpose, ticket string, enteredAt time.Time, ok bool) {
	if m, isMap := payload.(map[string]any); isMap {
		actor = stringFromWire(m["Actor"])
		purpose = stringFromWire(m["Purpose"])
		ticket = stringFromWire(m["Ticket"])
		enteredAt = timeFromWire(m["EnteredAt"])
		return actor, purpose, ticket, enteredAt, actor != ""
	}

	v := reflect.ValueOf(payload)
	if v.Kind() != reflect.Struct {
		return "", "", "", time.Time{}, false
	}
	actorField := v.FieldByName("Actor")
	if !actorField.IsValid() || actorField.Kind() != reflect.String {
		return "", "", "", time.Time{}, false
	}
	actor = actorField.String()
	if actor == "" {
		return "", "", "", time.Time{}, false
	}
	if purposeField := v.FieldByName("Purpose"); purposeField.IsValid() && purposeField.Kind() == reflect.String {
		purpose = purposeField.String()
	}
	if ticketField := v.FieldByName("Ticket"); ticketField.IsValid() && ticketField.Kind() == reflect.String {
		ticket = ticketField.String()
	}
	if enteredAtField := v.FieldByName("EnteredAt"); enteredAtField.IsValid() {
		if t, isTime := enteredAtField.Interface().(time.Time); isTime {
			enteredAt = t
		}
	}
	return actor, purpose, ticket, enteredAt, true
}
