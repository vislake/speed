package main

// demo_notification_test.go unit-tests simulationCompletedFieldsFromPayload
// in isolation, the exact way noteCreatedFieldsFromPayload's own probe
// would be tested if it carried a dedicated file: a concrete
// smilesim.SimulationCompletedPayload value (today's in-process EventBus
// shape), a map[string]any shaped like what pkgcore/eventbus/redis's
// EventBus actually hands a subscriber after its JSON round-trip (the
// shape a same-process subscriber sees whenever it is not the bus instance
// that published -- see the probe's own doc comment in demo_notification.go
// for why that is not the same as "same process, always safe"), and a
// handful of genuinely unreadable shapes that must still warn-and-drop.
//
// This is the regression the reference-app-go.md P1-1 finding named: before
// this probe existed, the subscription's naked
// evt.Payload.(smilesim.SimulationCompletedPayload) type assertion passed
// case (1) below and failed case (2) -- exactly the failure this test's
// "decoded map" cases would have caught. The Docker-backed integration
// regression in examples/reference-app/integration_test proves the same
// thing end to end, over a real Redis EventBus and a real subprocess.

import (
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/smilesim"
)

// TestSimulationCompletedFieldsFromPayload_ConcreteStruct proves the probe
// extracts correctly from the exact shape the in-process EventBus delivers
// today -- a straight pass-through of the publisher's own Go value -- so
// switching the subscription from a naked type assertion to this probe is
// not a regression for the composition every unit test in this package
// already runs under.
func TestSimulationCompletedFieldsFromPayload_ConcreteStruct(t *testing.T) {
	payload := smilesim.SimulationCompletedPayload{
		ImageJobID:      "job-1",
		TenantID:        "tenant-acme",
		RecipientUserID: "user-smilesim-recipient-1",
		Succeeded:       true,
		OutputObjectID:  "obj-1",
	}
	recipientUserID, succeeded, ok := simulationCompletedFieldsFromPayload(payload)
	if !ok {
		t.Fatalf("simulationCompletedFieldsFromPayload(%+v) ok = false, want true", payload)
	}
	if recipientUserID != "user-smilesim-recipient-1" {
		t.Errorf("recipientUserID = %q, want %q", recipientUserID, "user-smilesim-recipient-1")
	}
	if !succeeded {
		t.Errorf("succeeded = false, want true")
	}
}

// TestSimulationCompletedFieldsFromPayload_ConcreteStruct_Failed proves a
// present-but-false Succeeded is read correctly -- false is a meaningful
// answer (a failed or cancelled simulation), not an absent field, and the
// caller (wireDemoNotification's subscription) relies on ok=true, succeeded
// =false to skip dispatch without logging a warning.
func TestSimulationCompletedFieldsFromPayload_ConcreteStruct_Failed(t *testing.T) {
	payload := smilesim.SimulationCompletedPayload{
		RecipientUserID: "user-smilesim-recipient-1",
		Succeeded:       false,
	}
	recipientUserID, succeeded, ok := simulationCompletedFieldsFromPayload(payload)
	if !ok {
		t.Fatalf("ok = false, want true (a failed simulation is a readable payload)")
	}
	if succeeded {
		t.Errorf("succeeded = true, want false")
	}
	if recipientUserID != "user-smilesim-recipient-1" {
		t.Errorf("recipientUserID = %q, want %q", recipientUserID, "user-smilesim-recipient-1")
	}
}

// TestSimulationCompletedFieldsFromPayload_DecodedMap is the regression
// case: a map[string]any shaped exactly like what
// pkgcore/eventbus/redis's EventBus hands a subscriber once it has
// round-tripped a real smilesim.SimulationCompletedPayload through JSON
// (json.Marshal on the struct's plain, tag-less field names, then
// json.Unmarshal into interface{} -- exactly deliverRemote's own decode
// path). Before this probe existed, the subscription's naked type
// assertion to the concrete struct type failed on exactly this shape,
// silently dropping the event with only a warning log -- this case is
// what the pre-fix code could not pass.
func TestSimulationCompletedFieldsFromPayload_DecodedMap(t *testing.T) {
	decoded := map[string]any{
		"ImageJobID":      "job-1",
		"TenantID":        "tenant-acme",
		"RecipientUserID": "user-smilesim-recipient-1",
		"Succeeded":       true,
		"OutputObjectID":  "obj-1",
	}
	recipientUserID, succeeded, ok := simulationCompletedFieldsFromPayload(decoded)
	if !ok {
		t.Fatalf("simulationCompletedFieldsFromPayload(%+v) ok = false, want true -- this is the exact shape the Redis EventBus delivers", decoded)
	}
	if recipientUserID != "user-smilesim-recipient-1" {
		t.Errorf("recipientUserID = %q, want %q", recipientUserID, "user-smilesim-recipient-1")
	}
	if !succeeded {
		t.Errorf("succeeded = false, want true")
	}
}

// TestSimulationCompletedFieldsFromPayload_DecodedMap_Failed is the decoded-
// map twin of TestSimulationCompletedFieldsFromPayload_ConcreteStruct_Failed:
// JSON round-tripping a false bool through a Redis-delivered map must still
// read back as false, not as absent.
func TestSimulationCompletedFieldsFromPayload_DecodedMap_Failed(t *testing.T) {
	decoded := map[string]any{
		"RecipientUserID": "user-smilesim-recipient-1",
		"Succeeded":       false,
	}
	recipientUserID, succeeded, ok := simulationCompletedFieldsFromPayload(decoded)
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if succeeded {
		t.Errorf("succeeded = true, want false")
	}
	if recipientUserID != "user-smilesim-recipient-1" {
		t.Errorf("recipientUserID = %q, want %q", recipientUserID, "user-smilesim-recipient-1")
	}
}

// TestSimulationCompletedFieldsFromPayload_Unreadable proves the
// warn-and-drop path is preserved for every shape this subscription must
// never dispatch for: nil, a payload of some unrelated type, and maps
// missing one of the two required keys (including an empty
// RecipientUserID, which SimulationCompletedPayload's own doc comment
// declares NotifyOnCompletion never publishes).
func TestSimulationCompletedFieldsFromPayload_Unreadable(t *testing.T) {
	cases := map[string]any{
		"nil":                        nil,
		"unrelated struct":           struct{ Foo string }{Foo: "bar"},
		"missing RecipientUserID":    map[string]any{"Succeeded": true},
		"missing Succeeded":          map[string]any{"RecipientUserID": "user-smilesim-recipient-1"},
		"empty RecipientUserID":      map[string]any{"RecipientUserID": "", "Succeeded": true},
		"RecipientUserID wrong type": map[string]any{"RecipientUserID": 42, "Succeeded": true},
		"Succeeded wrong type":       map[string]any{"RecipientUserID": "user-smilesim-recipient-1", "Succeeded": "yes"},
		"unmarshalable channel":      make(chan int),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			recipientUserID, succeeded, ok := simulationCompletedFieldsFromPayload(payload)
			if ok {
				t.Fatalf("ok = true for %s, want false (recipientUserID=%q succeeded=%v)", name, recipientUserID, succeeded)
			}
			if recipientUserID != "" || succeeded {
				t.Errorf("unreadable payload must return zero values, got recipientUserID=%q succeeded=%v", recipientUserID, succeeded)
			}
		})
	}
}
