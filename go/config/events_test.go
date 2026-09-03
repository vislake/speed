package config

import "testing"

// Tests for events.go's change-event vocabulary: the event declaration's
// constants stay aligned with the registry contract they describe, and
// redactIf is the single place a Sensitive value becomes the marker instead
// of crossing the bus.

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
