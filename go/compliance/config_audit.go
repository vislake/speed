package compliance

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
)

// configAuditEventIDNamespace is a fixed, arbitrary UUID used as the
// namespace argument to uuid.NewSHA1 in configAuditEventID, mirroring
// go/dbkit/audit's own auditEventIDNamespace convention -- fixed so the
// same input always yields the same output, arbitrary because nothing
// outside this file ever needs to recognize it as meaningful, and never
// reused as a namespace for anything else (a distinct value from
// go/dbkit/audit's own, generated independently, so the two packages'
// deterministic ids can never collide even on identical content). Generated
// once with a fresh v4 UUID; never reused.
var configAuditEventIDNamespace = uuid.MustParse("875a41cd-229c-4648-8633-a100d3485831")

// onConfigItemChanged normalizes a config.EventConfigItemChanged event into
// an audit.AuditEvent under config.AuditActionConfigSet, closing the gap
// docs/internal/11-cross-cutting.md's own "change audit" bullet records as
// deferred to this module's own round: a dedicated audit record and its
// compliance consumer land with compliance's own round, subscribing to
// config.item.changed; that module takes no dependency on the audit side
// for it. config.AuditActionConfigSet
// is already declared on reg.AuditActions by config's own Register (see
// go/dbkit/audit/emit.go's own doc comment: "closing the loop go/config's
// own AuditActionConfigSet declaration left open") -- no new audit action
// needs declaring here, only the subscriber that finally persists a row
// under it.
//
// Mirrors go/dbkit/audit's own onWriteCaptured/onSystemContextEntered
// subscribers exactly: a payload this handler cannot decode is dropped
// rather than failing the whole handler chain (an event neither of
// configItemChangedFromWire's two accepted shapes is not this event, and
// must not wedge every other subscriber of the same event type), and the
// insert is idempotent (configAuditEventID derives the same row id for the
// same underlying change on every replica) so the distributed bus's
// at-least-once, once-per-replica delivery never double-records one
// change.
//
// config.ItemChangedEvent.Actor is a bare string (config.Actor carries no
// ActorType of its own -- see that type's own doc comment in
// go/config/values.go), so the recorded pkgcore.Actor is always typed
// ActorTypeUser: every config.Set call site this codebase wires today is
// an authenticated operator acting through an admin surface, never a
// system-context caller (the system-context path is tenancy's own
// EventSystemContextEntered, already covered by go/dbkit/audit's own
// subscriber). A future config.Set caller that is genuinely a system
// actor would need config's own event to carry an ActorType before this
// subscriber could record it correctly -- a known limitation, not a
// silent guess this file hides.
func (m *Module) onConfigItemChanged(ctx context.Context, evt pkgcore.Event) error {
	payload, ok := configItemChangedFromWire(evt.Payload)
	if !ok {
		return nil
	}

	row := &audit.AuditEvent{
		ID:         configAuditEventID(payload),
		Action:     config.AuditActionConfigSet,
		TenantID:   payload.TenantID,
		OccurredAt: payload.ChangedAt,
	}
	row.SetActor(pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: payload.Actor})
	row.SetResource(audit.Resource{Type: "config.item", ID: payload.Key})
	row.SetResult(audit.Result{Success: true})
	row.Changes = configChangesJSON(payload)
	return m.auditRepo.InsertIdempotent(ctx, row)
}

// configAuditEventID derives an AuditEvent.ID deterministically from
// payload, using uuid.NewSHA1 the identical way
// go/dbkit/audit's own auditDeterministicEventID does (unexported there,
// so duplicated here at the same fidelity rather than exported across the
// module boundary for this one call site). ChangedAt is formatted as UTC
// RFC3339Nano before hashing for the identical reason that function's own
// doc comment gives: a time.Time decoded from JSON by a remote replica's
// copy of the SAME event is not == or reflect.DeepEqual to the original
// in-process value, so both must be normalized to the same string first.
func configAuditEventID(payload config.ItemChangedEvent) string {
	data := payload.Key + "\x1f" + string(payload.Scope) + "\x1f" + payload.TenantID + "\x1f" +
		payload.Actor + "\x1f" + payload.ChangedAt.UTC().Format(time.RFC3339Nano)
	return uuid.NewSHA1(configAuditEventIDNamespace, []byte(data)).String()
}

// configChangesJSON marshals payload's old and new canonical values (already
// redacted by config's own redactIf for a Sensitive item -- the plaintext
// never reaches this subscriber, since it never reaches the bus) into an
// audit.Diff-shaped datatypes.JSON value, mirroring go/dbkit/audit's own
// changesJSON convention (also unexported there).
func configChangesJSON(payload config.ItemChangedEvent) []byte {
	b, err := json.Marshal(audit.Diff{
		Before: map[string]any{"value": payload.OldValue},
		After:  map[string]any{"value": payload.NewValue},
	})
	if err != nil {
		// json.Marshal only fails on a value it cannot represent at all,
		// not a realistic shape for the two plain strings built above.
		// Dropping Changes rather than failing the insert keeps a
		// pathological encoding failure from turning an otherwise valid
		// audit record into a lost one.
		return nil
	}
	return b
}

// configItemChangedFromWire decodes payload into a config.ItemChangedEvent,
// accepting the concrete struct (the standalone in-memory bus's own
// delivery shape) or the map[string]any shape encoding/json produces once
// it has crossed the distributed bus's Redis Streams transport --
// mirroring config's own itemChangedFromWire (unexported to package
// config, so this subscriber cannot call it directly and duplicates its
// exact two-shape contract here instead). ok is false, and the event
// dropped by onConfigItemChanged, for any other shape or a payload missing
// its mandatory Key field.
func configItemChangedFromWire(payload any) (config.ItemChangedEvent, bool) {
	switch p := payload.(type) {
	case config.ItemChangedEvent:
		return p, true
	case map[string]any:
		key, ok := p["Key"].(string)
		if !ok {
			return config.ItemChangedEvent{}, false
		}
		evt := config.ItemChangedEvent{
			Key:       key,
			Scope:     config.Scope(configStringFromWire(p["Scope"])),
			TenantID:  configStringFromWire(p["TenantID"]),
			Actor:     configStringFromWire(p["Actor"]),
			OldValue:  configStringFromWire(p["OldValue"]),
			NewValue:  configStringFromWire(p["NewValue"]),
			Sensitive: p["Sensitive"] == true,
		}
		if raw, ok := p["ChangedAt"].(string); ok {
			evt.ChangedAt, _ = time.Parse(time.RFC3339Nano, raw)
		}
		return evt, true
	default:
		return config.ItemChangedEvent{}, false
	}
}

// configStringFromWire returns v as a string when it is one, "" otherwise,
// mirroring go/config's and go/dbkit/audit's own identically-named helpers
// (each package keeps its own copy rather than exporting one across a
// module boundary for a single-field read).
func configStringFromWire(v any) string {
	s, _ := v.(string)
	return s
}
