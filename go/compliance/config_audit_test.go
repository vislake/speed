package compliance

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
)

// testItemChangedEvent is the fixture config.ItemChangedEvent every test in
// this file publishes, mirroring one real config.Set call: a plain (never
// Sensitive) item changed by an operator, in a tenant.
func testItemChangedEvent() config.ItemChangedEvent {
	return config.ItemChangedEvent{
		Key:       "brand.support_email",
		Scope:     config.ScopeTenant,
		TenantID:  "tenant-acme",
		Actor:     "user-42",
		OldValue:  "old@example.com",
		NewValue:  "new@example.com",
		Sensitive: false,
		ChangedAt: time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC),
	}
}

// TestModule_OnConfigItemChanged_PersistsAnAuditRow proves the subscriber
// Register installs (config_audit.go's onConfigItemChanged) actually turns
// a real config.EventConfigItemChanged event into a persisted
// audit.AuditEvent row under config.AuditActionConfigSet -- the exact gap
// docs/internal/11-cross-cutting.md's "变更审计" bullet recorded as
// deferred to this module's own round.
func TestModule_OnConfigItemChanged_PersistsAnAuditRow(t *testing.T) {
	auditRepo := newTestAuditRepo(t)
	m := NewModule(auditRepo, WithQueue(&recordingQueue{}))
	reg, err := pkgcore.NewKernel().Bootstrap(context.Background(), m)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	payload := testItemChangedEvent()
	if pubErr := reg.EventBus().Publish(context.Background(), pkgcore.Event{
		Type:     config.EventConfigItemChanged,
		TenantID: pkgcore.TenantID(payload.TenantID),
		Payload:  payload,
	}); pubErr != nil {
		t.Fatalf("Publish(EventConfigItemChanged): %v", pubErr)
	}

	rows, err := auditRepo.ListByTenant(context.Background(), payload.TenantID)
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListByTenant returned %d rows, want 1: %+v", len(rows), rows)
	}
	row := rows[0]

	if row.Action != config.AuditActionConfigSet {
		t.Errorf("Action = %q, want %q", row.Action, config.AuditActionConfigSet)
	}
	if row.TenantID != payload.TenantID {
		t.Errorf("TenantID = %q, want %q", row.TenantID, payload.TenantID)
	}
	if row.ActorType != string(pkgcore.ActorTypeUser) || row.ActorID != payload.Actor {
		t.Errorf("Actor = (%q, %q), want (%q, %q)", row.ActorType, row.ActorID, pkgcore.ActorTypeUser, payload.Actor)
	}
	if row.ResourceType != "config.item" || row.ResourceID != payload.Key {
		t.Errorf("Resource = (%q, %q), want (\"config.item\", %q)", row.ResourceType, row.ResourceID, payload.Key)
	}
	if !row.Success {
		t.Error("Success = false, want true")
	}
	if !row.OccurredAt.Equal(payload.ChangedAt) {
		t.Errorf("OccurredAt = %v, want %v", row.OccurredAt, payload.ChangedAt)
	}

	var diff audit.Diff
	if err := json.Unmarshal(row.Changes, &diff); err != nil {
		t.Fatalf("unmarshal Changes: %v", err)
	}
	if diff.Before["value"] != payload.OldValue || diff.After["value"] != payload.NewValue {
		t.Errorf("Changes = %+v, want before=%q after=%q", diff, payload.OldValue, payload.NewValue)
	}
}

// TestModule_OnConfigItemChanged_IsIdempotentAcrossRedelivery proves a
// redelivered copy of the SAME underlying change -- what the distributed
// bus's at-least-once, once-per-replica transport produces -- lands exactly
// one row, not two, because configAuditEventID derives the same id both
// times.
func TestModule_OnConfigItemChanged_IsIdempotentAcrossRedelivery(t *testing.T) {
	auditRepo := newTestAuditRepo(t)
	m := NewModule(auditRepo, WithQueue(&recordingQueue{}))
	reg, err := pkgcore.NewKernel().Bootstrap(context.Background(), m)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	payload := testItemChangedEvent()
	evt := pkgcore.Event{
		Type:     config.EventConfigItemChanged,
		TenantID: pkgcore.TenantID(payload.TenantID),
		Payload:  payload,
	}
	for i := 0; i < 2; i++ {
		if pubErr := reg.EventBus().Publish(context.Background(), evt); pubErr != nil {
			t.Fatalf("Publish #%d: %v", i, pubErr)
		}
	}

	rows, err := auditRepo.ListByTenant(context.Background(), payload.TenantID)
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListByTenant returned %d rows after a redelivered event, want 1: %+v", len(rows), rows)
	}
}

// TestModule_OnConfigItemChanged_DropsAnUndecodablePayload proves a payload
// neither configItemChangedFromWire shape accepts is dropped without
// erroring the handler chain, mirroring go/dbkit/audit's own
// onWriteCaptured/onSystemContextEntered contract for the identical case.
func TestModule_OnConfigItemChanged_DropsAnUndecodablePayload(t *testing.T) {
	auditRepo := newTestAuditRepo(t)
	m := NewModule(auditRepo, WithQueue(&recordingQueue{}))

	if err := m.onConfigItemChanged(context.Background(), pkgcore.Event{
		Type:    config.EventConfigItemChanged,
		Payload: "not an ItemChangedEvent",
	}); err != nil {
		t.Fatalf("onConfigItemChanged(undecodable payload) error = %v, want nil", err)
	}

	rows, err := auditRepo.ListByTenant(context.Background(), "tenant-acme")
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("ListByTenant returned %d rows, want 0 for a dropped payload", len(rows))
	}
}

// TestConfigItemChangedFromWire_AcceptsTheJSONMapShape proves the
// distributed-bus decode path -- a config.ItemChangedEvent JSON round-
// tripped into map[string]any, the shape a Redis Streams reader hands a
// subscriber -- decodes identically to the concrete-struct shape the
// standalone in-memory bus delivers.
func TestConfigItemChangedFromWire_AcceptsTheJSONMapShape(t *testing.T) {
	want := testItemChangedEvent()
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("json.Unmarshal into map: %v", err)
	}

	got, ok := configItemChangedFromWire(asMap)
	if !ok {
		t.Fatal("configItemChangedFromWire(map shape) ok = false, want true")
	}
	if got.Key != want.Key || got.Scope != want.Scope || got.TenantID != want.TenantID ||
		got.Actor != want.Actor || got.OldValue != want.OldValue || got.NewValue != want.NewValue ||
		got.Sensitive != want.Sensitive || !got.ChangedAt.Equal(want.ChangedAt) {
		t.Errorf("configItemChangedFromWire(map shape) = %+v, want %+v", got, want)
	}
}

// TestConfigItemChangedFromWire_RefusesAnUnrecognizedShape pins ok=false
// for a payload that is neither the concrete struct nor a decodable map.
func TestConfigItemChangedFromWire_RefusesAnUnrecognizedShape(t *testing.T) {
	if _, ok := configItemChangedFromWire(42); ok {
		t.Error("configItemChangedFromWire(42) ok = true, want false")
	}
	if _, ok := configItemChangedFromWire(map[string]any{"NotKey": "x"}); ok {
		t.Error("configItemChangedFromWire(map without Key) ok = true, want false")
	}
}
