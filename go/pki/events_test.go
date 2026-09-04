package pki

import (
	"testing"
	"time"
)

// TestSigningKeyLifecycleEventFromWire_PassesConcreteStructThrough proves
// the standalone deployment mode's shape -- the in-memory bus hands
// subscribers the exact struct a publisher sent, untouched.
func TestSigningKeyLifecycleEventFromWire_PassesConcreteStructThrough(t *testing.T) {
	want := SigningKeyLifecycleEvent{
		Purpose:     "authn.access_token",
		KID:         "kid-1",
		PreviousKID: "kid-0",
		OccurredAt:  time.Now(),
	}
	got, ok := signingKeyLifecycleEventFromWire(want)
	if !ok {
		t.Fatalf("signingKeyLifecycleEventFromWire(concrete struct) = not ok, want ok")
	}
	if got != want {
		t.Errorf("signingKeyLifecycleEventFromWire(concrete struct) = %+v, want %+v unchanged", got, want)
	}
}

// TestSigningKeyLifecycleEventFromWire_DecodesTheDistributedModeShape
// proves the distributed deployment mode's shape -- a JSON-decoded
// map[string]any keyed by the struct's Go field names -- decodes correctly.
// This is not a nicety: cache invalidation across replicas is the whole
// point of publishing these events, so an event that crossed a replica
// boundary must decode here or a distributed deployment silently falls
// back to the cache's TTL poll alone.
func TestSigningKeyLifecycleEventFromWire_DecodesTheDistributedModeShape(t *testing.T) {
	payload := map[string]any{
		"Purpose":     "authn.access_token",
		"KID":         "kid-1",
		"PreviousKID": "kid-0",
	}
	got, ok := signingKeyLifecycleEventFromWire(payload)
	if !ok {
		t.Fatalf("signingKeyLifecycleEventFromWire(map) = not ok, want ok")
	}
	want := SigningKeyLifecycleEvent{Purpose: "authn.access_token", KID: "kid-1", PreviousKID: "kid-0"}
	if got != want {
		t.Errorf("signingKeyLifecycleEventFromWire(map) = %+v, want %+v", got, want)
	}
}

// TestSigningKeyLifecycleEventFromWire_MapWithoutPreviousKID proves
// PreviousKID is optional in the wire map -- EventSigningKeyStaged and
// EventSigningKeyRetired never set it (events.go's own doc comment), so a
// map missing the key must still decode, not fail.
func TestSigningKeyLifecycleEventFromWire_MapWithoutPreviousKID(t *testing.T) {
	payload := map[string]any{"Purpose": "authn.access_token", "KID": "kid-1"}
	got, ok := signingKeyLifecycleEventFromWire(payload)
	if !ok {
		t.Fatalf("signingKeyLifecycleEventFromWire(map without PreviousKID) = not ok, want ok")
	}
	if got.PreviousKID != "" {
		t.Errorf("PreviousKID = %q, want empty", got.PreviousKID)
	}
}

// TestSigningKeyLifecycleEventFromWire_RejectsAMapWithNoPurpose proves the
// cache invalidation path (Service.onSigningKeyLifecycleEvent) has
// something to key on before it trusts a decoded payload -- an empty or
// missing Purpose is the one field cache invalidation strictly needs.
func TestSigningKeyLifecycleEventFromWire_RejectsAMapWithNoPurpose(t *testing.T) {
	for name, payload := range map[string]map[string]any{
		"missing Purpose key": {"KID": "kid-1"},
		"empty Purpose value": {"Purpose": "", "KID": "kid-1"},
		"wrong-typed Purpose": {"Purpose": 42, "KID": "kid-1"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := signingKeyLifecycleEventFromWire(payload); ok {
				t.Errorf("signingKeyLifecycleEventFromWire(%v) = ok, want a decode failure", payload)
			}
		})
	}
}

// TestSigningKeyLifecycleEventFromWire_RejectsAnUnrecognizedShape proves a
// payload of neither expected shape (not the concrete struct, not a map) is
// dropped rather than decoded into a zero-value event that would look like
// a valid-but-empty-Purpose one.
func TestSigningKeyLifecycleEventFromWire_RejectsAnUnrecognizedShape(t *testing.T) {
	if _, ok := signingKeyLifecycleEventFromWire(42); ok {
		t.Errorf("signingKeyLifecycleEventFromWire(int) = ok, want a decode failure")
	}
	if _, ok := signingKeyLifecycleEventFromWire(nil); ok {
		t.Errorf("signingKeyLifecycleEventFromWire(nil) = ok, want a decode failure")
	}
}

// TestEventDecls_MatchTheDeclaredConstants pins eventDecls (what Register
// declares) against the exported event-name constants, and against each
// event's expected PayloadType, so a rename of one without the other is
// caught here rather than by a mismatched catalog entry at bootstrap.
func TestEventDecls_MatchTheDeclaredConstants(t *testing.T) {
	want := map[string]string{
		EventSigningKeyStaged:    signingKeyEventPayloadType,
		EventSigningKeyActivated: signingKeyEventPayloadType,
		EventSigningKeyRetired:   signingKeyEventPayloadType,
		EventSigningKeyRevoked:   signingKeyEventPayloadType,
		EventCertificateRevoked:  certificateEventPayloadType,
	}
	if len(eventDecls) != len(want) {
		t.Fatalf("eventDecls has %d entries, want %d", len(eventDecls), len(want))
	}
	for _, decl := range eventDecls {
		wantPayload, ok := want[decl.Type]
		if !ok {
			t.Errorf("eventDecls declares unexpected type %q", decl.Type)
			continue
		}
		if decl.PayloadType != wantPayload {
			t.Errorf("eventDecls[%q].PayloadType = %q, want %q", decl.Type, decl.PayloadType, wantPayload)
		}
		if decl.Description == "" {
			t.Errorf("eventDecls[%q] has no Description", decl.Type)
		}
	}
}
