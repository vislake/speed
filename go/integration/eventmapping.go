package integration

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/vislake/speed/go/pkgcore"
)

// EventTransform maps one internal domain event onto the raw JSON body of
// its versioned public schema -- the "only the deliberately chosen fields
// are exposed" rule docs/internal/07-platform-services.md's outbound-
// webhook section states.
//
// A Transform never sees WebhookSubscription -- the mapping from internal
// event to public payload is a property of the EVENT TYPE alone, decided
// once by the module that owns the internal fact, not of who happens to be
// subscribed to hear about it. It runs once per matching domain event (not
// once per subscription): its result is stored in
// WebhookDelivery.Payload and fanned out to every matching subscription
// unchanged, so a Transform must be pure with respect to which
// subscriptions exist.
//
// The returned bytes become the "data" field of the public envelope (see
// buildEnvelope in webhook_delivery.go) -- they are ONLY the event's own
// fields, never re-wrapped with event.type/event.version, which
// buildEnvelope adds once for every mapping. Returning an error refuses the
// whole fan-out for this event (logged and swallowed by handleDomainEvent,
// per that function's own doc comment on why it can never propagate an
// error back to the publisher).
type EventTransform func(ctx context.Context, evt pkgcore.Event) (json.RawMessage, error)

// EventMapping is one business module's declared internal-to-public event
// schema mapping: "this internal event type maps to this public schema
// version, via this transform function"
// (docs/internal/07-platform-services.md's own phrasing).
//
// # Why a Module option, not a pkgcore.Registry field or a business-module
// # import
//
// The design doc's rule that an internal domain event must never be
// forwarded outbound as-is needs a mapping layer somewhere, and three
// shapes were available: (1) a new field on
// pkgcore.Registry every business module writes to during its own Register
// call, (2) go/integration importing every business module to read its
// events directly, or (3) a host-supplied declaration this module's own
// Module exposes.
//
// (2) is a straightforward violation of the root CLAUDE.md's module-
// boundary rule and is not considered further. (1) was the task's own
// suggested shape, and IS how a mapping conceptually belongs to the module
// that owns the internal event -- but implementing it means changing
// pkgcore.Registry, which this round is explicitly scoped to leave
// untouched (pkgcore is the dependency floor every other module sits on;
// widening its Registry contract is a change every module in the codebase
// would need to react to, not a decision one module's round should make
// unilaterally). (3) is what this file implements: WithEventMapping is a
// Module Option, wired by the HOST at composition time -- exactly the shape
// WithPermissionLister and WithMembershipChecker already established in
// round 1 (seams.go), and exactly the shape every other cross-module seam
// in this codebase not carried on pkgcore.Registry uses (org.FeatureGate,
// rbac.SubtreeResolver, notification.UserAddressResolver). The host already
// imports every business module it boots plus go/integration, so it is the
// one place in the whole program allowed to see across every module
// boundary at once; wiring the mapping there costs no import edge business
// module code does not already have reason to avoid.
//
// A business module's own Register call still populates the CATALOG half of
// this contract for free, with no go/integration-specific code of its own:
// every EventDecl a module declares through reg.Events.Publishes already
// documents the event's Type, its PayloadType and a human description
// (pkgcore.EventDecl's own doc comment: "the declarations form the catalog
// integration maps onto its versioned public event schema"). What a
// business module's Register call does NOT do -- and, per this round's own
// mandate not to touch pkgcore, cannot be made to do without a business
// module importing go/integration -- is hand a Transform function across
// the boundary; only the host, which sits above every module, can close
// that function over the business module's own concrete payload type (or,
// as this round's own proof does in webhook_delivery_test.go, read the
// payload structurally via JSON, the identical no-import technique
// org.userIDFromPayload already uses for authn.user.created).
//
// # Registered at Module construction time, not at Register time
//
// WithEventMapping runs inside NewModule, before Kernel.Bootstrap ever
// calls this module's own Register -- unlike WithPermissionLister, whose
// seam Service.Create merely reads at call time, this module's Register
// must know every declared mapping's InternalType up front, so it can
// subscribe to each one on reg.Events (reg.Events.Subscribe performs no
// I/O, so this remains legal inside Register's "must not perform I/O; it
// only declares" contract). That ordering requirement is exactly why this
// is a NewModule Option rather than, say, a method called after Attach.
type EventMapping struct {
	// InternalType is the pkgcore.Event.Type this mapping subscribes to --
	// the routing key the OWNING business module publishes under (for
	// example "org.member.joined").
	InternalType string

	// PublicType is the public event type a webhook subscriber names in
	// WebhookSubscription.EventTypes to receive this mapping's fan-out (for
	// example "org.member.joined" -- often, but not necessarily, identical
	// to InternalType; the two vocabularies are allowed to diverge, per
	// webhook_model.go's own "EventTypes names PUBLIC event types" doc
	// comment).
	PublicType string

	// PublicVersion is the schema version this mapping's Transform produces
	// right now (for example "v1"), per docs/internal/07's rule that public
	// events are versioned independently: a breaking change to what Transform
	// returns ships as a NEW EventMapping under a new PublicVersion (and,
	// per the design doc, the old version keeps being served for a
	// deprecation window rather than changing in place) -- never by
	// mutating an existing mapping's Transform in place, which would
	// silently change what an already-configured subscriber receives.
	PublicVersion string

	// Transform produces the public payload from the internal event. See
	// EventTransform's own doc comment. Required: WithEventMapping rejects
	// a mapping with a nil Transform (see validate).
	Transform EventTransform

	// Description is optional English catalog text -- kept for symmetry
	// with pkgcore.EventDecl.Description and for a later round's admin
	// console to render, never required.
	Description string
}

// validate reports the first structural problem with m: an empty
// InternalType, PublicType, PublicVersion or a nil Transform. It never
// inspects Description.
func (m EventMapping) validate() error {
	switch {
	case m.InternalType == "":
		return ErrInvalidEventMapping.WithParam("field", "internal_type")
	case m.PublicType == "":
		return ErrInvalidEventMapping.WithParam("field", "public_type")
	case m.PublicVersion == "":
		return ErrInvalidEventMapping.WithParam("field", "public_version")
	case m.Transform == nil:
		return ErrInvalidEventMapping.WithParam("field", "transform")
	default:
		return nil
	}
}

// eventMappingIndex is the two views Module.Register builds once, over
// every EventMapping a host declared through WithEventMapping: byInternal
// for subscribing (one reg.Events.Subscribe call per distinct
// InternalType) and dispatching (handleDomainEvent looks an incoming
// pkgcore.Event.Type up here), and publicTypes for validating a
// subscription's requested EventTypes at creation time
// (Service.CreateWebhookSubscription -- ErrWebhookEventTypeUnknown for a
// type no mapping declares).
type eventMappingIndex struct {
	byInternal  map[string]EventMapping
	publicTypes map[string]bool
}

// buildEventMappingIndex validates every mapping (see EventMapping.validate)
// and rejects a repeated InternalType with ErrDuplicateEventMapping --
// exactly like pkgcore.EventRegistrar.Publishes rejects a repeated Type,
// since two mappings claiming to subscribe to the same internal event type
// would mean only the second Subscribe call actually reaches
// reg.Events.Subscribe under a shared handler, silently dropping the
// first's Transform.
func buildEventMappingIndex(mappings []EventMapping) (eventMappingIndex, error) {
	idx := eventMappingIndex{
		byInternal:  make(map[string]EventMapping, len(mappings)),
		publicTypes: make(map[string]bool, len(mappings)),
	}
	for _, m := range mappings {
		if err := m.validate(); err != nil {
			return eventMappingIndex{}, err
		}
		if _, exists := idx.byInternal[m.InternalType]; exists {
			return eventMappingIndex{}, ErrDuplicateEventMapping.WithParam("internal_type", m.InternalType)
		}
		idx.byInternal[m.InternalType] = m
		idx.publicTypes[m.PublicType] = true
	}
	return idx, nil
}

// publicEventEnvelope is the versioned public wire shape every webhook
// delivery's body is, per docs/internal/07-platform-services.md's rule that
// the payload schema is identified by "event.type" + "event.version".
// buildEnvelope (webhook_delivery.go) is the only place this type is
// constructed.
type publicEventEnvelope struct {
	Event publicEventMeta `json:"event"`
	Data  json.RawMessage `json:"data"`
}

// publicEventMeta is publicEventEnvelope's "event" field.
type publicEventMeta struct {
	Type    string `json:"type"`
	Version string `json:"version"`
}

// buildEnvelope runs mapping.Transform over evt and wraps the result in the
// versioned public envelope. The returned bytes are exactly what
// WebhookDelivery.Payload stores and what a delivery attempt's HMAC
// signature covers (webhook_signature.go).
func buildEnvelope(ctx context.Context, mapping EventMapping, evt pkgcore.Event) ([]byte, error) {
	data, err := mapping.Transform(ctx, evt)
	if err != nil {
		return nil, fmt.Errorf("integration: transform %q into %q/%s: %w", evt.Type, mapping.PublicType, mapping.PublicVersion, err)
	}
	if len(data) == 0 {
		data = json.RawMessage("{}")
	}
	envelope := publicEventEnvelope{
		Event: publicEventMeta{Type: mapping.PublicType, Version: mapping.PublicVersion},
		Data:  data,
	}
	return json.Marshal(envelope)
}
