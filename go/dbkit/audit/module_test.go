package audit

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// newRegisteredModule opens a fresh test DB (see repository_test.go's
// openAuditTestDB), builds a Module over it, registers it on a fresh
// in-memory pkgcore.ComponentRegistry, and returns both -- the shape every test in
// this file starts from.
func newRegisteredModule(t *testing.T, declares ...func(*pkgcore.ComponentRegistry) error) (*pkgcore.ComponentRegistry, *Module) {
	t.Helper()
	db := openAuditTestDB(t)
	m := New(db)
	reg := componenttest.NewRegistry()
	// The module's Register and every extra declaration step the caller
	// passes share the registry's one Init window: the seats accept writes
	// only during Init, and a registry runs Init once.
	if err := componenttest.DeclareAll(reg, append([]func(*pkgcore.ComponentRegistry) error{m.Register}, declares...)...); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	return reg, m
}

func TestModule_Name_ReturnsAudit(t *testing.T) {
	if got, want := New(nil).Name(), "audit"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
}

func TestModule_DependsOn_ReturnsNil(t *testing.T) {
	if got := New(nil).DependsOn(); got != nil {
		t.Errorf("DependsOn() = %v, want nil", got)
	}
}

func TestModule_Locales_ReturnsEmptyFS(t *testing.T) {
	if entries, err := New(nil).Locales().ReadDir("."); err == nil && len(entries) != 0 {
		t.Errorf("Locales() contains entries %v, want an empty embed.FS", entries)
	}
}

func TestModule_OpenAPISpec_ReturnsNil(t *testing.T) {
	if got := New(nil).OpenAPISpec(); got != nil {
		t.Errorf("OpenAPISpec() = %v, want nil", got)
	}
}

func TestModule_Register_DeclaresEventsAndTheSystemContextAuditAction(t *testing.T) {
	reg, _ := newRegisteredModule(t)

	types := make(map[string]bool)
	for _, decl := range reg.Events.Published() {
		types[decl.Type] = true
	}
	if !types[dbkit.EventWriteCaptured] {
		t.Errorf("Published() = %v, want it to include %q", reg.Events.Published(), dbkit.EventWriteCaptured)
	}
	if !types[EventRecorded] {
		t.Errorf("Published() = %v, want it to include %q", reg.Events.Published(), EventRecorded)
	}

	found := false
	for _, action := range reg.AuditActions.Actions() {
		if action == AuditActionSystemContextEntered {
			found = true
		}
	}
	if !found {
		t.Errorf("AuditActions.Actions() = %v, want it to include %q", reg.AuditActions.Actions(), AuditActionSystemContextEntered)
	}
}

func TestModule_OnWriteCaptured_PersistsAuditEvent(t *testing.T) {
	// A captured write's derived action ("<resource_type>.<operation>") is
	// only persisted when the owning module declared it on the registrar --
	// onWriteCaptured's gate. This test declares the vocabulary its payload
	// derives ("note.create" from note + create) the way a host wiring the
	// capture plugin would, inside the registry's one Init window.
	reg, m := newRegisteredModule(t, func(r *pkgcore.ComponentRegistry) error {
		return r.AuditActions.Add("note.create")
	})

	admin := pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: "admin-1", DisplayName: "Grace"}
	payload := dbkit.WriteCapturedEvent{
		Actor:        pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1", DisplayName: "Ada"},
		OnBehalfOf:   &admin,
		TenantID:     "tenant-a",
		Table:        "notes",
		ResourceType: "note",
		ResourceID:   "note-1",
		Operation:    "create",
		After:        map[string]any{"title": "Meeting notes"},
		IP:           "203.0.113.7",
		UserAgent:    "speed-client/1.0",
		TraceID:      "trace-abc",
		OccurredAt:   time.Now(),
	}
	err := reg.Events.Bus().Publish(context.Background(), pkgcore.Event{
		Type:     dbkit.EventWriteCaptured,
		TenantID: pkgcore.TenantID(payload.TenantID),
		Payload:  payload,
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	rows, err := m.repo.ListByTenant(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("ListByTenant() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListByTenant() returned %d rows, want 1", len(rows))
	}
	got := rows[0]
	if got.Action != "note.create" {
		t.Errorf("Action = %q, want %q", got.Action, "note.create")
	}
	if got.Actor().ID != "user-1" {
		t.Errorf("Actor().ID = %q, want %q", got.Actor().ID, "user-1")
	}
	onBehalfOf, ok := got.OnBehalfOf()
	if !ok || onBehalfOf.ID != "admin-1" {
		t.Errorf("OnBehalfOf() = (%+v, %v), want (admin-1, true)", onBehalfOf, ok)
	}
	if got.Resource().ID != "note-1" {
		t.Errorf("Resource().ID = %q, want %q", got.Resource().ID, "note-1")
	}
	if !got.Result().Success {
		t.Errorf("Result().Success = false, want true (a captured write is always a success -- capture only fires after the write actually succeeded)")
	}
	if len(got.Changes) == 0 {
		t.Errorf("Changes is empty, want the After snapshot to have been recorded")
	}
	// The request-context trio, carried on the event from the capture
	// plugin's read of the write's context, must land on the row.
	if got.IP != "203.0.113.7" {
		t.Errorf("IP = %q, want the payload's request metadata IP", got.IP)
	}
	if got.UserAgent != "speed-client/1.0" {
		t.Errorf("UserAgent = %q, want the payload's request metadata UserAgent", got.UserAgent)
	}
	if got.TraceID != "trace-abc" {
		t.Errorf("TraceID = %q, want the payload's request metadata TraceID", got.TraceID)
	}
}

func TestModule_OnWriteCaptured_JSONMapPayload_PersistsAuditEvent(t *testing.T) {
	// Round-trips a WriteCapturedEvent through encoding/json the way the
	// distributed EventBus's Redis Streams transport actually would
	// (pkgcore/redis_eventbus.go: json.Marshal on publish, json.Unmarshal
	// into interface{} on delivery), proving writeCapturedFromWire's
	// map[string]any branch against real JSON semantics rather than a
	// hand-built map.
	// Declare the derived action ("note.update") this payload's capture
	// would produce, the way a host wiring the plugin must, inside the
	// registry's one Init window.
	_, m := newRegisteredModule(t, func(r *pkgcore.ComponentRegistry) error {
		return r.AuditActions.Add("note.update")
	})

	original := dbkit.WriteCapturedEvent{
		Actor:        pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1"},
		TenantID:     "tenant-a",
		ResourceType: "note",
		ResourceID:   "note-1",
		Operation:    "update",
		IP:           "203.0.113.7",
		UserAgent:    "speed-client/1.0",
		TraceID:      "trace-abc",
		OccurredAt:   time.Now(),
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var asMap any
	if unmarshalErr := json.Unmarshal(raw, &asMap); unmarshalErr != nil {
		t.Fatalf("json.Unmarshal() error = %v", unmarshalErr)
	}

	if handleErr := m.onWriteCaptured(context.Background(), pkgcore.Event{Type: dbkit.EventWriteCaptured, Payload: asMap}); handleErr != nil {
		t.Fatalf("onWriteCaptured() error = %v", handleErr)
	}

	rows, err := m.repo.ListByTenant(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("ListByTenant() error = %v", err)
	}
	if len(rows) != 1 || rows[0].Action != "note.update" || rows[0].Actor().ID != "user-1" {
		t.Fatalf("ListByTenant() = %+v, want one row Action=note.update Actor.ID=user-1", rows)
	}
	// The request-context trio must survive the JSON round trip -- this is
	// the distributed-bus shape, where the payload crosses the wire as a
	// map and the trio rides it like Actor and TenantID do.
	if rows[0].IP != "203.0.113.7" || rows[0].UserAgent != "speed-client/1.0" || rows[0].TraceID != "trace-abc" {
		t.Errorf("persisted row request-context fields = (%q, %q, %q), want the payload's (203.0.113.7, speed-client/1.0, trace-abc)",
			rows[0].IP, rows[0].UserAgent, rows[0].TraceID)
	}
}

func TestModule_OnWriteCaptured_UnrecognizedPayload_DropsWithoutError(t *testing.T) {
	_, m := newRegisteredModule(t)

	if err := m.onWriteCaptured(context.Background(), pkgcore.Event{Type: dbkit.EventWriteCaptured, Payload: "not a write-captured event"}); err != nil {
		t.Fatalf("onWriteCaptured() error = %v, want nil (an undecodable payload must be dropped, not fail the handler chain)", err)
	}
	rows, err := m.repo.ListByTenant(context.Background(), "")
	if err != nil {
		t.Fatalf("ListByTenant() error = %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("ListByTenant() = %+v, want no rows persisted for an undecodable payload", rows)
	}
}

// TestModule_OnWriteCaptured_UndeclaredDerivedAction_RefusedWithAlert
// pins the capture-vocabulary gate: a captured write's derived action
// ("<resource_type>.<operation>", e.g. "note.create") is persisted only
// when some module declared it on the AuditActionRegistrar -- the same
// declared enumeration Emit validates its own Input.Action against
// (ErrActionNotRegistered). The ungated behavior silently wrote the row
// under the derived action, landing an action no module ever declared on
// the one table a compliance query filters by the declared vocabulary:
// rows under an undeclared action are as invisible to that query as rows
// that never existed.
func TestModule_OnWriteCaptured_UndeclaredDerivedAction_RefusedWithAlert(t *testing.T) {
	reg, m := newRegisteredModule(t)
	// Deliberately declare NOTHING for the "note" resource: the registrar
	// carries only what Module.Register itself declared
	// (AuditActionSystemContextEntered), so "note.create" is undeclared.

	err := reg.Events.Bus().Publish(context.Background(), pkgcore.Event{
		Type:     dbkit.EventWriteCaptured,
		TenantID: "tenant-a",
		Payload: dbkit.WriteCapturedEvent{
			Actor:        pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1"},
			TenantID:     "tenant-a",
			ResourceType: "note",
			ResourceID:   "note-1",
			Operation:    "create",
			After:        map[string]any{"title": "Meeting notes"},
			OccurredAt:   time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	rows, err := m.repo.ListByTenant(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("ListByTenant() error = %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("ListByTenant() = %+v, want no rows -- a captured write whose derived action no module declared must be refused, never silently persisted", rows)
	}
}

func TestModule_OnRecorded_PersistsAuditEvent(t *testing.T) {
	const action = "notes.note.create"
	reg, m := newRegisteredModule(t, func(r *pkgcore.ComponentRegistry) error {
		return r.AuditActions.Add(action)
	})

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1"})
	ctx = dbkit.WithRequestMetadata(ctx, dbkit.RequestMetadata{
		IP:        "203.0.113.7",
		UserAgent: "speed-client/1.0",
		TraceID:   "trace-abc",
	})
	in := Input{
		Action:   action,
		Resource: Resource{Type: "note", ID: "note-1"},
		Result:   Result{Success: true},
		Changes:  &Diff{Before: map[string]any{"title": "old"}, After: map[string]any{"title": "new"}},
	}
	if err := Emit(ctx, reg.Events.Bus(), reg.AuditActions, in); err != nil {
		t.Fatalf("Emit() error = %v", err)
	}

	rows, err := m.repo.ListByTenant(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("ListByTenant() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListByTenant() returned %d rows, want 1", len(rows))
	}
	if got := rows[0]; got.Action != action || got.Actor().ID != "user-1" || len(got.Changes) == 0 {
		t.Errorf("ListByTenant()[0] = %+v, want Action=%q Actor.ID=user-1 with Changes populated", got, action)
	}
	// The request-context trio, read by Emit from the caller's context and
	// carried on the event, must land on the row end to end.
	if got := rows[0]; got.IP != "203.0.113.7" || got.UserAgent != "speed-client/1.0" || got.TraceID != "trace-abc" {
		t.Errorf("persisted row request-context fields = (%q, %q, %q), want (203.0.113.7, speed-client/1.0, trace-abc)",
			got.IP, got.UserAgent, got.TraceID)
	}
}

func TestModule_OnSystemContextEntered_PersistsAuditEvent(t *testing.T) {
	_, m := newRegisteredModule(t)

	payload := fakeSystemContextEnteredEvent{
		Actor:     "platform-admin-1",
		Purpose:   "admin.tenant_search",
		Ticket:    "SUP-1234",
		EnteredAt: time.Now(),
	}
	err := m.onSystemContextEntered(context.Background(), pkgcore.Event{
		Type:     tenancySystemContextEnteredEventType,
		TenantID: "tenant-a",
		Payload:  payload,
	})
	if err != nil {
		t.Fatalf("onSystemContextEntered() error = %v", err)
	}

	rows, err := m.repo.ListByTenant(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("ListByTenant() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListByTenant() returned %d rows, want 1", len(rows))
	}
	got := rows[0]
	if got.Action != tenancySystemContextEnteredEventType {
		t.Errorf("Action = %q, want %q", got.Action, tenancySystemContextEnteredEventType)
	}
	if got.Actor().Type != pkgcore.ActorTypeSystem || got.Actor().ID != "platform-admin-1" {
		t.Errorf("Actor() = %+v, want Type=system ID=platform-admin-1", got.Actor())
	}
	if got.Resource().ID != "admin.tenant_search" {
		t.Errorf("Resource().ID = %q, want %q", got.Resource().ID, "admin.tenant_search")
	}
	if len(got.Changes) == 0 {
		t.Errorf("Changes is empty, want the ticket to have been recorded")
	}
}

func TestModule_OnSystemContextEntered_JSONMapPayload_PersistsAuditEvent(t *testing.T) {
	_, m := newRegisteredModule(t)

	original := fakeSystemContextEnteredEvent{Actor: "platform-admin-1", Purpose: "admin.tenant_search", EnteredAt: time.Now()}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var asMap any
	if unmarshalErr := json.Unmarshal(raw, &asMap); unmarshalErr != nil {
		t.Fatalf("json.Unmarshal() error = %v", unmarshalErr)
	}

	err = m.onSystemContextEntered(context.Background(), pkgcore.Event{Type: tenancySystemContextEnteredEventType, Payload: asMap})
	if err != nil {
		t.Fatalf("onSystemContextEntered() error = %v", err)
	}
	rows, err := m.repo.ListByTenant(context.Background(), "")
	if err != nil {
		t.Fatalf("ListByTenant() error = %v", err)
	}
	if len(rows) != 1 || rows[0].Actor().ID != "platform-admin-1" {
		t.Fatalf("ListByTenant() = %+v, want one row with Actor.ID=platform-admin-1", rows)
	}
}

func TestModule_OnSystemContextEntered_EmptyActor_DropsWithoutError(t *testing.T) {
	_, m := newRegisteredModule(t)

	payload := fakeSystemContextEnteredEvent{Purpose: "admin.tenant_search"}
	if err := m.onSystemContextEntered(context.Background(), pkgcore.Event{Type: tenancySystemContextEnteredEventType, Payload: payload}); err != nil {
		t.Fatalf("onSystemContextEntered() error = %v, want nil", err)
	}
	rows, err := m.repo.ListByTenant(context.Background(), "")
	if err != nil {
		t.Fatalf("ListByTenant() error = %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("ListByTenant() = %+v, want no rows persisted for an Actor-less payload", rows)
	}
}

// fakeSystemContextEnteredEvent reproduces
// tenancy.SystemContextEnteredEvent's exact field names and types (Actor
// string, Purpose pkgcore.SystemPurpose, Ticket string, EnteredAt
// time.Time) WITHOUT importing package tenancy -- this package cannot
// (see tenancySystemContextEnteredEventType's doc comment) -- so
// decodeSystemContextEntered's reflection-based struct branch can be
// exercised against the identical shape the real event actually has.
type fakeSystemContextEnteredEvent struct {
	Actor     string
	Purpose   pkgcore.SystemPurpose
	Ticket    string
	EnteredAt time.Time
}

func TestChangesJSON_BothEmpty_ReturnsNil(t *testing.T) {
	if got := changesJSON(nil, nil); got != nil {
		t.Errorf("changesJSON(nil, nil) = %v, want nil", got)
	}
	if got := changesJSON(map[string]any{}, map[string]any{}); got != nil {
		t.Errorf("changesJSON({}, {}) = %v, want nil", got)
	}
}

func TestChangesJSON_Populated_MarshalsBoth(t *testing.T) {
	got := changesJSON(map[string]any{"a": 1.0}, map[string]any{"a": 2.0})
	if got == nil {
		t.Fatal("changesJSON() = nil, want a populated JSON value")
	}
	var decoded Diff
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if decoded.Before["a"] != 1.0 || decoded.After["a"] != 2.0 {
		t.Errorf("decoded = %+v, want Before.a=1 After.a=2", decoded)
	}
}

// TestModule_OnWriteCaptured_DeliveredToMultipleReplicas_PersistsExactlyOnce
// proves Module's deduplication end to end: in distributed deployment
// mode, a broker-backed bus delivers a single published event to every
// replica once each (the Redis implementation's own doc comment: "each
// event is delivered to every replica exactly once" -- once per replica,
// not once system-wide), and every replica independently runs
// onWriteCaptured against the SAME shared database. Without the
// deterministic auditDeterministicEventID plus Repository.InsertIdempotent
// pairing, each replica's independent Insert would generate its own
// random UUID and a single real write would leave one audit_events row per
// replica instead of one row total.
//
// This test simulates exactly that: onWriteCaptured is invoked twice for
// the SAME logical write -- once as the publishing replica would see it
// (the concrete dbkit.WriteCapturedEvent struct, delivered synchronously
// in-process) and once as every OTHER replica would see it (the identical
// event decoded from JSON, the shape a broker-backed transport
// reconstructs it as -- see the sibling
// TestModule_OnWriteCaptured_JSONMapPayload_PersistsAuditEvent for that
// same round trip in isolation) -- against the one shared Module/db pair a
// real deployment's replicas would also share, and asserts exactly one row
// results, not two.
func TestModule_OnWriteCaptured_DeliveredToMultipleReplicas_PersistsExactlyOnce(t *testing.T) {
	// Declare the derived action ("note.create") this payload's capture
	// would produce, the way a host wiring the plugin must, inside the
	// registry's one Init window.
	_, m := newRegisteredModule(t, func(r *pkgcore.ComponentRegistry) error {
		return r.AuditActions.Add("note.create")
	})

	original := dbkit.WriteCapturedEvent{
		Actor:        pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1"},
		TenantID:     "tenant-a",
		ResourceType: "note",
		ResourceID:   "note-1",
		Operation:    "create",
		After:        map[string]any{"title": "Meeting notes"},
		OccurredAt:   time.Now(),
	}
	raw, marshalErr := json.Marshal(original)
	if marshalErr != nil {
		t.Fatalf("json.Marshal() error = %v", marshalErr)
	}
	var asMap any
	if unmarshalErr := json.Unmarshal(raw, &asMap); unmarshalErr != nil {
		t.Fatalf("json.Unmarshal() error = %v", unmarshalErr)
	}

	ctx := context.Background()
	// Replica A: the publishing replica's own synchronous, in-process
	// delivery -- the concrete struct.
	if err := m.onWriteCaptured(ctx, pkgcore.Event{Type: dbkit.EventWriteCaptured, TenantID: "tenant-a", Payload: original}); err != nil {
		t.Fatalf("onWriteCaptured() [replica A] error = %v", err)
	}
	// Replica B: a remote replica's reader goroutine delivering the SAME
	// event after its own JSON round trip through Redis Streams.
	if err := m.onWriteCaptured(ctx, pkgcore.Event{Type: dbkit.EventWriteCaptured, TenantID: "tenant-a", Payload: asMap}); err != nil {
		t.Fatalf("onWriteCaptured() [replica B] error = %v", err)
	}

	rows, err := m.repo.ListByTenant(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("ListByTenant() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListByTenant() returned %d rows, want exactly 1 (one real write delivered to 2 replicas must persist once, not once per replica)", len(rows))
	}
}

// TestModule_OnRecorded_DeliveredToMultipleReplicas_PersistsExactlyOnce is
// onRecorded's counterpart of
// TestModule_OnWriteCaptured_DeliveredToMultipleReplicas_PersistsExactlyOnce
// -- see that test's doc comment for the scenario being reproduced.
func TestModule_OnRecorded_DeliveredToMultipleReplicas_PersistsExactlyOnce(t *testing.T) {
	_, m := newRegisteredModule(t)

	original := RecordedEvent{
		Actor:      pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1"},
		TenantID:   "tenant-a",
		Action:     "notes.note.create",
		Resource:   Resource{Type: "note", ID: "note-1"},
		Result:     Result{Success: true},
		OccurredAt: time.Now(),
	}
	raw, marshalErr := json.Marshal(original)
	if marshalErr != nil {
		t.Fatalf("json.Marshal() error = %v", marshalErr)
	}
	var asMap any
	if unmarshalErr := json.Unmarshal(raw, &asMap); unmarshalErr != nil {
		t.Fatalf("json.Unmarshal() error = %v", unmarshalErr)
	}

	ctx := context.Background()
	if err := m.onRecorded(ctx, pkgcore.Event{Type: EventRecorded, TenantID: "tenant-a", Payload: original}); err != nil {
		t.Fatalf("onRecorded() [replica A] error = %v", err)
	}
	if err := m.onRecorded(ctx, pkgcore.Event{Type: EventRecorded, TenantID: "tenant-a", Payload: asMap}); err != nil {
		t.Fatalf("onRecorded() [replica B] error = %v", err)
	}

	rows, err := m.repo.ListByTenant(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("ListByTenant() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListByTenant() returned %d rows, want exactly 1 (one real Emit delivered to 2 replicas must persist once, not once per replica)", len(rows))
	}
}

// TestModule_OnSystemContextEntered_DeliveredToMultipleReplicas_PersistsExactlyOnce
// is onSystemContextEntered's counterpart of
// TestModule_OnWriteCaptured_DeliveredToMultipleReplicas_PersistsExactlyOnce
// -- see that test's doc comment for the scenario being reproduced.
func TestModule_OnSystemContextEntered_DeliveredToMultipleReplicas_PersistsExactlyOnce(t *testing.T) {
	_, m := newRegisteredModule(t)

	original := fakeSystemContextEnteredEvent{
		Actor:     "platform-admin-1",
		Purpose:   "admin.tenant_search",
		Ticket:    "SUP-1234",
		EnteredAt: time.Now(),
	}
	raw, marshalErr := json.Marshal(original)
	if marshalErr != nil {
		t.Fatalf("json.Marshal() error = %v", marshalErr)
	}
	var asMap any
	if unmarshalErr := json.Unmarshal(raw, &asMap); unmarshalErr != nil {
		t.Fatalf("json.Unmarshal() error = %v", unmarshalErr)
	}

	ctx := context.Background()
	if err := m.onSystemContextEntered(ctx, pkgcore.Event{Type: tenancySystemContextEnteredEventType, TenantID: "tenant-a", Payload: original}); err != nil {
		t.Fatalf("onSystemContextEntered() [replica A] error = %v", err)
	}
	if err := m.onSystemContextEntered(ctx, pkgcore.Event{Type: tenancySystemContextEnteredEventType, TenantID: "tenant-a", Payload: asMap}); err != nil {
		t.Fatalf("onSystemContextEntered() [replica B] error = %v", err)
	}

	rows, err := m.repo.ListByTenant(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("ListByTenant() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListByTenant() returned %d rows, want exactly 1 (one real system-context grant delivered to 2 replicas must persist once, not once per replica)", len(rows))
	}
}
