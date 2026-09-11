package main

// demo_notification_test.go unit-tests SimulationCompletedFieldsFromPayload
// in isolation, the exact way NoteCreatedFieldsFromPayload's own probe
// would be tested if it carried a dedicated file: a concrete
// smilesim.SimulationCompletedPayload value (the in-process EventBus
// shape), a map[string]any shaped like what pkgcore/eventbus/redis's
// EventBus actually hands a subscriber after its JSON round-trip (the
// shape a same-process subscriber sees whenever it is not the bus instance
// that published -- see the probe's own doc comment in internal/app/demo/demo_notification.go
// for why that is not the same as "same process, always safe"), and a
// handful of genuinely unreadable shapes that must still warn-and-drop.
//
// The regression it guards: a subscription that type-asserts the naked
// evt.Payload.(smilesim.SimulationCompletedPayload) passes case (1)
// below and fails case (2) -- the failure this test's "decoded map"
// cases catch. The Docker-backed integration test in
// examples/reference-app/integration_test proves the same end to end,
// over a real Redis EventBus and a real subprocess.
//
// The probe carries three required fields; the third, the completing
// job's image id, rides in the subscription's dispatch Params as the
// per-occurrence marker that keeps two completed simulations for the same
// recipient from collapsing into one delivery (see the probe's own doc
// comment in internal/app/demo/demo_notification.go). Every readable-payload case below
// therefore carries one, and the unreadable list names its absence.

import (
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/app/demo"
	"github.com/vislake/speed/examples/reference-app/internal/notes"
	"github.com/vislake/speed/examples/reference-app/internal/smilesim"
)

// --- NoteCreatedFieldsFromPayload -------------------------------------
//
// The note-created probe's own dedicated suite, mirroring the
// simulationCompleted suite above shape for shape: a concrete
// notes.NoteCreatedPayload value (the in-process EventBus shape), a
// map[string]any shaped like what the Redis bus's JSON round-trip hands
// a subscriber, and the unreadable shapes that must warn-and-drop. The
// probe's two required fields are the note id and the creator's user id
// -- the note-created event is undeliverable without the creator (the
// notification module routes on it), and a note id the dispatch cannot
// name renders no template.

func TestNoteCreatedFieldsFromPayload_ConcreteStruct(t *testing.T) {
	payload := notes.NoteCreatedPayload{NoteID: "note-1", TenantID: "tenant-a", CreatorUserID: "user-7"}
	noteID, creator, ok := demo.NoteCreatedFieldsFromPayload(payload)
	if !ok {
		t.Fatal("probe rejected a concrete notes.NoteCreatedPayload, want extraction")
	}
	if noteID != "note-1" || creator != "user-7" {
		t.Fatalf("probe = (note %q, creator %q), want (note-1, user-7)", noteID, creator)
	}
}

func TestNoteCreatedFieldsFromPayload_DecodedMap(t *testing.T) {
	payload := map[string]any{"note_id": "note-2", "tenant_id": "tenant-a", "creator_user_id": "user-8"}
	noteID, creator, ok := demo.NoteCreatedFieldsFromPayload(payload)
	if !ok {
		t.Fatal("probe rejected a decoded-map payload, want extraction")
	}
	if noteID != "note-2" || creator != "user-8" {
		t.Fatalf("probe = (note %q, creator %q), want (note-2, user-8)", noteID, creator)
	}
}

func TestNoteCreatedFieldsFromPayload_SurvivesTheAlternativeSpellings(t *testing.T) {
	// The struct-spelling keys are accepted from a map too: the map a
	// Redis-bus round-trip produces carries the struct field names as its
	// keys, and the probe's contract is to read whichever shape arrived.
	payload := map[string]any{"NoteID": "note-3", "CreatorUserID": "user-9"}
	noteID, creator, ok := demo.NoteCreatedFieldsFromPayload(payload)
	if !ok {
		t.Fatal("probe rejected the struct-spelling keys on a map payload")
	}
	if noteID != "note-3" || creator != "user-9" {
		t.Fatalf("probe = (note %q, creator %q), want (note-3, user-9)", noteID, creator)
	}
}

func TestNoteCreatedFieldsFromPayload_Unreadable(t *testing.T) {
	cases := []struct {
		name    string
		payload any
	}{
		{"nil payload", nil},
		{"missing creator", map[string]any{"note_id": "note-1"}},
		{"missing note id", map[string]any{"creator_user_id": "user-1"}},
		{"empty note id", map[string]any{"note_id": "", "creator_user_id": "user-1"}},
		{"empty creator", map[string]any{"note_id": "note-1", "creator_user_id": ""}},
		{"wrong-typed fields", map[string]any{"note_id": 42, "creator_user_id": true}},
		{"non-map json value", "just a string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			noteID, creator, ok := demo.NoteCreatedFieldsFromPayload(tc.payload)
			if ok {
				t.Errorf("unreadable payload must return ok=false, got (note %q, creator %q)", noteID, creator)
			}
		})
	}
}

// TestSimulationCompletedFieldsFromPayload_ConcreteStruct proves the probe
// extracts correctly from the exact shape the in-process EventBus
// delivers -- a straight pass-through of the publisher's own Go value --
// the shape the unit tests in this package compose under, where a
// type-asserting subscription reads the value directly while the probe
// must decode it like any other delivery.
func TestSimulationCompletedFieldsFromPayload_ConcreteStruct(t *testing.T) {
	payload := smilesim.SimulationCompletedPayload{
		ImageJobID:      "job-1",
		TenantID:        "tenant-acme",
		RecipientUserID: "user-smilesim-recipient-1",
		Succeeded:       true,
		OutputObjectID:  "obj-1",
	}
	recipientUserID, imageJobID, succeeded, ok := demo.SimulationCompletedFieldsFromPayload(payload)
	if !ok {
		t.Fatalf("SimulationCompletedFieldsFromPayload(%+v) ok = false, want true", payload)
	}
	if recipientUserID != "user-smilesim-recipient-1" {
		t.Errorf("recipientUserID = %q, want %q", recipientUserID, "user-smilesim-recipient-1")
	}
	if imageJobID != "job-1" {
		t.Errorf("imageJobID = %q, want %q", imageJobID, "job-1")
	}
	if !succeeded {
		t.Errorf("succeeded = false, want true")
	}
}

// TestSimulationCompletedFieldsFromPayload_ConcreteStruct_Failed proves a
// present-but-false Succeeded is read correctly -- false is a meaningful
// answer (a failed or cancelled simulation), not an absent field, and the
// caller (WireDemoNotification's subscription) relies on ok=true, succeeded
// =false to skip dispatch without logging a warning.
func TestSimulationCompletedFieldsFromPayload_ConcreteStruct_Failed(t *testing.T) {
	payload := smilesim.SimulationCompletedPayload{
		ImageJobID:      "job-2",
		RecipientUserID: "user-smilesim-recipient-1",
		Succeeded:       false,
	}
	recipientUserID, imageJobID, succeeded, ok := demo.SimulationCompletedFieldsFromPayload(payload)
	if !ok {
		t.Fatalf("ok = false, want true (a failed simulation is a readable payload)")
	}
	if succeeded {
		t.Errorf("succeeded = true, want false")
	}
	if recipientUserID != "user-smilesim-recipient-1" {
		t.Errorf("recipientUserID = %q, want %q", recipientUserID, "user-smilesim-recipient-1")
	}
	if imageJobID != "job-2" {
		t.Errorf("imageJobID = %q, want %q", imageJobID, "job-2")
	}
}

// TestSimulationCompletedFieldsFromPayload_DecodedMap is the regression
// case: a map[string]any shaped exactly like what
// pkgcore/eventbus/redis's EventBus hands a subscriber once it has
// round-tripped a real smilesim.SimulationCompletedPayload through JSON
// (json.Marshal on the struct's plain, tag-less field names, then
// json.Unmarshal into interface{} -- exactly deliverRemote's own decode
// path). A subscription that type-asserts the payload to the concrete
// struct type fails on exactly this shape,
// silently dropping the event with only a warning log -- this case is
// what a type-asserting subscriber cannot pass.
func TestSimulationCompletedFieldsFromPayload_DecodedMap(t *testing.T) {
	decoded := map[string]any{
		"ImageJobID":      "job-1",
		"TenantID":        "tenant-acme",
		"RecipientUserID": "user-smilesim-recipient-1",
		"Succeeded":       true,
		"OutputObjectID":  "obj-1",
	}
	recipientUserID, imageJobID, succeeded, ok := demo.SimulationCompletedFieldsFromPayload(decoded)
	if !ok {
		t.Fatalf("SimulationCompletedFieldsFromPayload(%+v) ok = false, want true -- this is the exact shape the Redis EventBus delivers", decoded)
	}
	if recipientUserID != "user-smilesim-recipient-1" {
		t.Errorf("recipientUserID = %q, want %q", recipientUserID, "user-smilesim-recipient-1")
	}
	if imageJobID != "job-1" {
		t.Errorf("imageJobID = %q, want %q", imageJobID, "job-1")
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
		"ImageJobID":      "job-2",
		"RecipientUserID": "user-smilesim-recipient-1",
		"Succeeded":       false,
	}
	recipientUserID, imageJobID, succeeded, ok := demo.SimulationCompletedFieldsFromPayload(decoded)
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if succeeded {
		t.Errorf("succeeded = true, want false")
	}
	if recipientUserID != "user-smilesim-recipient-1" {
		t.Errorf("recipientUserID = %q, want %q", recipientUserID, "user-smilesim-recipient-1")
	}
	if imageJobID != "job-2" {
		t.Errorf("imageJobID = %q, want %q", imageJobID, "job-2")
	}
}

// TestSimulationCompletedFieldsFromPayload_Unreadable proves the
// warn-and-drop path is preserved for every shape this subscription must
// never dispatch for: nil, a payload of some unrelated type, and maps
// missing one of the three required keys (including an empty
// RecipientUserID, which SimulationCompletedPayload's own doc comment
// declares NotifyOnCompletion never publishes, and an absent or empty
// ImageJobID, without which the dispatch could not carry its
// per-occurrence marker -- see the probe's own doc comment for why such a
// payload has nothing this subscription can truthfully dispatch).
func TestSimulationCompletedFieldsFromPayload_Unreadable(t *testing.T) {
	cases := map[string]any{
		"nil":                        nil,
		"unrelated struct":           struct{ Foo string }{Foo: "bar"},
		"missing ImageJobID":         map[string]any{"RecipientUserID": "user-smilesim-recipient-1", "Succeeded": true},
		"empty ImageJobID":           map[string]any{"ImageJobID": "", "RecipientUserID": "user-smilesim-recipient-1", "Succeeded": true},
		"ImageJobID wrong type":      map[string]any{"ImageJobID": 42, "RecipientUserID": "user-smilesim-recipient-1", "Succeeded": true},
		"missing RecipientUserID":    map[string]any{"ImageJobID": "job-1", "Succeeded": true},
		"missing Succeeded":          map[string]any{"ImageJobID": "job-1", "RecipientUserID": "user-smilesim-recipient-1"},
		"empty RecipientUserID":      map[string]any{"ImageJobID": "job-1", "RecipientUserID": "", "Succeeded": true},
		"RecipientUserID wrong type": map[string]any{"ImageJobID": "job-1", "RecipientUserID": 42, "Succeeded": true},
		"Succeeded wrong type":       map[string]any{"ImageJobID": "job-1", "RecipientUserID": "user-smilesim-recipient-1", "Succeeded": "yes"},
		"unmarshalable channel":      make(chan int),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			recipientUserID, imageJobID, succeeded, ok := demo.SimulationCompletedFieldsFromPayload(payload)
			if ok {
				t.Fatalf("ok = true for %s, want false (recipientUserID=%q imageJobID=%q succeeded=%v)", name, recipientUserID, imageJobID, succeeded)
			}
			if recipientUserID != "" || imageJobID != "" || succeeded {
				t.Errorf("unreadable payload must return zero values, got recipientUserID=%q imageJobID=%q succeeded=%v", recipientUserID, imageJobID, succeeded)
			}
		})
	}
}
