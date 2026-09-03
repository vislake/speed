package audit

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// newRegisteredModule opens a fresh test DB (see repository_test.go's
// openAuditTestDB), builds a Module over it, registers it on a fresh
// in-memory pkgcore.Registry, and returns both -- the shape every test in
// this file starts from.
func newRegisteredModule(t *testing.T) (*pkgcore.Registry, *Module) {
	t.Helper()
	db := openAuditTestDB(t)
	m := New(db)
	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := m.Register(reg); err != nil {
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
	reg, m := newRegisteredModule(t)

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
}

func TestModule_OnWriteCaptured_JSONMapPayload_PersistsAuditEvent(t *testing.T) {
	// Round-trips a WriteCapturedEvent through encoding/json the way the
	// distributed EventBus's Redis Streams transport actually would
	// (pkgcore/redis_eventbus.go: json.Marshal on publish, json.Unmarshal
	// into interface{} on delivery), proving writeCapturedFromWire's
	// map[string]any branch against real JSON semantics rather than a
	// hand-built map.
	_, m := newRegisteredModule(t)

	original := dbkit.WriteCapturedEvent{
		Actor:        pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1"},
		TenantID:     "tenant-a",
		ResourceType: "note",
		ResourceID:   "note-1",
		Operation:    "update",
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

func TestModule_OnRecorded_PersistsAuditEvent(t *testing.T) {
	reg, m := newRegisteredModule(t)
	const action = "notes.note.create"
	if err := reg.AuditActions.Add(action); err != nil {
		t.Fatalf("AuditActions.Add() error = %v", err)
	}

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1"})
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
