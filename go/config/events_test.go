package config

import (
	"encoding/json"
	"testing"
	"time"
)

// Tests for events.go's change-event vocabulary: the event declaration's
// constants stay aligned with the registry contract they describe, and
// redactIf is the single place a Sensitive value becomes the marker instead
// of crossing the bus. itemChangedFromWire's two shapes are covered here
// too: a delivered payload must decode whether it crossed a replica
// boundary (the JSON map shape) or not (the concrete struct).

func TestEventDecl_MatchesItsConstants(t *testing.T) {
	// The declaration is what Register publishes to the registry's event
	// registrar; a drift between it and the constants a subscriber or the
	// host keys on would break the vocabulary silently.
	if eventDecl.Type != EventConfigItemChanged {
		t.Fatalf("eventDecl.Type = %q, want %q", eventDecl.Type, EventConfigItemChanged)
	}
	if eventDecl.PayloadType != eventConfigItemChangedPayloadType {
		t.Fatalf("eventDecl.PayloadType = %q, want %q", eventDecl.PayloadType, eventConfigItemChangedPayloadType)
	}
	if eventDecl.Type != "config.item.changed" {
		t.Fatalf("the event type constant drifted from its stable wire value: %q", eventDecl.Type)
	}
	if eventConfigItemChangedPayloadType != "config.ItemChangedEvent" {
		t.Fatalf("the payload type constant drifted from its stable wire value: %q", eventConfigItemChangedPayloadType)
	}
	if AuditActionConfigSet != "config.item.set" {
		t.Fatalf("the audit action constant drifted from its stable wire value: %q", AuditActionConfigSet)
	}
}

func TestRedactIf_MasksSensitiveValuesOnly(t *testing.T) {
	if got := redactIf(true, "secret@example.com"); got != redactedMarker {
		t.Fatalf("redactIf(true, ...) = %q, want the marker %q", got, redactedMarker)
	}
	if got := redactIf(false, "public@example.com"); got != "public@example.com" {
		t.Fatalf("redactIf(false, ...) = %q, want the value unchanged", got)
	}
	if redactedMarker != "[redacted]" {
		t.Fatalf("the redaction marker drifted from its stable wire value: %q", redactedMarker)
	}
}

func TestItemChangedFromWire_PassesTheConcreteStructThrough(t *testing.T) {
	// The standalone mode's in-memory bus delivers the struct itself; the
	// recovery must be an identity for it.
	at := time.Now().Truncate(time.Second).UTC()
	want := ItemChangedEvent{
		Key: "brand.site_name", Scope: ScopeTenant, TenantID: "tenant-a",
		Actor: "alice", OldValue: "Studio A", NewValue: "Studio A2",
		Sensitive: false, ChangedAt: at,
	}
	got, ok := itemChangedFromWire(want)
	if !ok {
		t.Fatal("itemChangedFromWire rejected the concrete struct it was built from")
	}
	if got != want {
		t.Fatalf("itemChangedFromWire(struct) = %+v, want the identical event %+v", got, want)
	}
}

func TestItemChangedFromWire_ReadsARemoteJSONMap(t *testing.T) {
	// pkgcore's distributed bus carries the payload as JSON and reconstructs
	// it into a plain map (see pkgcore's RedisEventBus reader): a struct
	// delivered through that path must decode back field for field, with
	// time.Time arriving as its RFC3339 string. This is the regression test
	// for the delivery path a cross-replica invalidation actually uses.
	at := time.Now().Truncate(time.Second).UTC()
	want := ItemChangedEvent{
		Key: "brand.site_name", Scope: ScopeSystem, TenantID: "",
		Actor: "ops-1", OldValue: "Global Co", NewValue: "Global Co 2",
		Sensitive: false, ChangedAt: at,
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var wire any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("json.Unmarshal into any: %v", err)
	}

	got, ok := itemChangedFromWire(wire)
	if !ok {
		t.Fatal("itemChangedFromWire rejected the JSON map a remote bus delivers")
	}
	if got.Key != want.Key || got.Scope != want.Scope || got.TenantID != want.TenantID || got.Actor != want.Actor {
		t.Fatalf("remote map decoded to %+v, want %+v", got, want)
	}
	if got.OldValue != "Global Co" || got.NewValue != "Global Co 2" || got.Sensitive {
		t.Fatalf("remote map values decoded wrong: %+v", got)
	}
	if !got.ChangedAt.Equal(at) {
		t.Fatalf("remote map ChangedAt = %v, want %v", got.ChangedAt, at)
	}
}

func TestItemChangedFromWire_RejectsForeignPayloads(t *testing.T) {
	// Anything that is neither the struct nor a JSON map of it is not this
	// event; the subscriber ignores it without failing the handler chain.
	if _, ok := itemChangedFromWire(nil); ok {
		t.Fatal("itemChangedFromWire accepted a nil payload")
	}
	if _, ok := itemChangedFromWire("config.item.changed"); ok {
		t.Fatal("itemChangedFromWire accepted a bare string payload")
	}
	if _, ok := itemChangedFromWire(map[string]any{"Scope": "system"}); ok {
		t.Fatal("itemChangedFromWire accepted a map without the Key field")
	}
	if _, ok := itemChangedFromWire(map[string]any{"Key": "brand.site_name"}); ok {
		t.Fatal("itemChangedFromWire accepted a map without the Scope field")
	}
}

func TestItemChangedFromWire_ToleratesOptionalFieldLoss(t *testing.T) {
	// Invalidation keys on Key and Scope alone; an event whose optional
	// fields were stripped (or whose ChangedAt did not survive as a string)
	// must still decode rather than be dropped.
	got, ok := itemChangedFromWire(map[string]any{
		"Key": "brand.site_name", "Scope": "system",
		"NewValue": "Global Co", "Sensitive": false,
		"ChangedAt": 12345, // not a time string: degrades, does not drop
	})
	if !ok {
		t.Fatal("itemChangedFromWire dropped a map that carried the mandatory fields")
	}
	if got.Key != "brand.site_name" || got.Scope != ScopeSystem || got.NewValue != "Global Co" {
		t.Fatalf("decoded event = %+v, want Key/Scope/NewValue preserved", got)
	}
	if !got.ChangedAt.IsZero() {
		t.Fatalf("ChangedAt = %v, want zero after a non-string value", got.ChangedAt)
	}
}
