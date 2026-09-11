package compliance

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
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
// audit.AuditEvent row under config.AuditActionConfigSet -- the change
// audit every successful config.Set must leave behind.
func TestModule_OnConfigItemChanged_PersistsAnAuditRow(t *testing.T) {
	auditRepo := newTestAuditRepo(t)
	m := NewModule(auditRepo, WithQueue(&recordingQueue{}))
	reg, err := componenttest.DeclareModules(m)
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
	reg, err := componenttest.DeclareModules(m)
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

// TestConfigItemChangedFromWire_RequiresScopeAndKeyStrings pins the map
// branch of configItemChangedFromWire to config's own decoder's acceptance
// contract -- go/config/events.go's itemChangedFromJSONMap doc: "Only Key
// and Scope are mandatory". Key AND Scope must both decode as strings; a
// map missing either, or carrying either in a non-string form, is not this
// event. This is the regression for the Scope-less wire delivery that used
// to decode with Scope "" -- a scope no real config.Set can produce
// (config.Scope's closed set of ScopeSystem and ScopeTenant) -- into an
// audit row for a change config's own subscriber refused.
func TestConfigItemChangedFromWire_RequiresScopeAndKeyStrings(t *testing.T) {
	refused := []map[string]any{
		{"Scope": "tenant"},                                 // Key absent.
		{"Key": 42, "Scope": "tenant"},                      // Key present but not a string.
		{"Key": ConfigDefaultRetentionWindow},               // Scope absent.
		{"Key": ConfigDefaultRetentionWindow, "Scope": 42},  // Scope not a string.
		{"Key": ConfigDefaultRetentionWindow, "Scope": nil}, // Scope JSON null.
		{"Key": ConfigDefaultRetentionWindow, "Scope": []any{"tenant"}},
	}
	for i, payload := range refused {
		if got, ok := configItemChangedFromWire(payload); ok {
			t.Errorf("refusal case %d (%v): ok = true (decoded %+v), want false", i, payload, got)
		}
	}

	// The mirror is a string-ness check, not a non-empty check: config's
	// own decoder accepts an explicitly empty string in either mandatory
	// field, so this decoder must too -- dropping those would diverge the
	// other way, refusing a payload config's own subscriber processes.
	accepted := []map[string]any{
		{"Key": ConfigDefaultRetentionWindow, "Scope": "tenant"},
		{"Key": ConfigDefaultRetentionWindow, "Scope": ""},
		{"Key": "", "Scope": "tenant"},
	}
	for i, payload := range accepted {
		if _, ok := configItemChangedFromWire(payload); !ok {
			t.Errorf("acceptance case %d (%v): ok = false, want true", i, payload)
		}
	}
}

// TestModule_OnConfigItemChanged_ScopeMissingWireMap_MatchesConfigsOwnDrop
// proves the parity config_audit.go's configItemChangedFromWire doc
// claims ("exactly mirrors config's own ... contract") on the one shape
// the two decoders disagree about: a distributed-bus delivery whose Scope
// field is missing or corrupted. config's own subscriber drops such a
// payload -- its decoder treats Key and Scope as the two mandatory fields
// (go/config/events.go's itemChangedFromJSONMap) -- and this test proves
// compliance's subscriber drops it too, by publishing the SAME payload on
// ONE bus to BOTH modules and asserting both stay silent: config's
// watcher on the changed key never fires (config's side decoded nothing)
// and no audit row lands (compliance's side decoded nothing). Were the
// Scope-less payload decoded with Scope "" and recorded as a
// config.item.set audit row, this assertion would fail -- an audit row for
// a change config itself never processed. The positive leg then republishes
// the identical payload WITH its Scope field, and both subscribers act:
// the watcher fires and exactly one audit row lands -- pinning that only
// the malformed shape is dropped, never the well-formed one.
func TestModule_OnConfigItemChanged_ScopeMissingWireMap_MatchesConfigsOwnDrop(t *testing.T) {
	db := newTestAuditDB(t)
	auditRepo := audit.NewRepository(db)
	compMod := NewModule(auditRepo, WithQueue(&recordingQueue{}))
	configMod := config.NewModule(db, config.WithPollInterval(0))
	reg := componenttest.NewRegistry()
	var configSvc *config.Service
	// The modules' Register calls and config's Attach share the registry's
	// one Init window: Attach subscribes on the Events seat, and the seats
	// accept writes only during Init.
	if err := componenttest.DeclareAll(reg, compMod.Register, configMod.Register,
		func(r *pkgcore.ComponentRegistry) error {
			attached, err := configMod.Attach(r)
			if err != nil {
				return err
			}
			configSvc = attached
			return nil
		},
	); err != nil {
		t.Fatalf("declare and attach: %v", err)
	}

	// A watcher on a compliance-declared key: config fires it exactly when
	// its own subscriber accepts an event for that key, so its silence is
	// the observable verdict of config's own decoder on the payload below.
	key := ConfigDefaultRetentionWindow
	fired := make(chan config.Value, 4)
	if err := configSvc.Watch(key, func(v config.Value) { fired <- v }); err != nil {
		t.Fatalf("config Watch(%q): %v", key, err)
	}

	// The wire shape a Redis Streams reader hands subscribers for a real
	// config.Set -- minus the Scope field, a delivery no honest publisher
	// produces but a corrupted or foreign stream record can.
	scopeMissing := map[string]any{
		"Key":       key,
		"TenantID":  "tenant-acme",
		"Actor":     "user-42",
		"OldValue":  "24h0m0s",
		"NewValue":  "48h0m0s",
		"Sensitive": false,
		"ChangedAt": "2026-09-04T10:00:00Z",
	}
	if err := reg.EventBus().Publish(context.Background(), pkgcore.Event{
		Type:     config.EventConfigItemChanged,
		TenantID: "tenant-acme",
		Payload:  scopeMissing,
	}); err != nil {
		t.Fatalf("Publish(Scope-less event): %v", err)
	}

	// config's verdict: its decoder refuses the payload, so its watcher
	// never fired.
	select {
	case v := <-fired:
		t.Fatalf("config watcher fired %+v for a Scope-less payload config's own decoder refuses", v)
	default:
	}

	// compliance's verdict must be the same: no audit row for a change
	// config itself never processed.
	rows, err := auditRepo.ListByTenant(context.Background(), "tenant-acme")
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("Scope-less payload produced %d audit rows, want 0 (config's own decoder drops the same payload)", len(rows))
	}

	// Positive leg: the same payload carrying its Scope is a genuine
	// change on both sides -- config fires the watcher, compliance records
	// exactly one row.
	scopeTenant := map[string]any{
		"Key":       key,
		"Scope":     "tenant",
		"TenantID":  "tenant-acme",
		"Actor":     "user-42",
		"OldValue":  "24h0m0s",
		"NewValue":  "48h0m0s",
		"Sensitive": false,
		"ChangedAt": "2026-09-04T10:00:00Z",
	}
	if pubErr := reg.EventBus().Publish(context.Background(), pkgcore.Event{
		Type:     config.EventConfigItemChanged,
		TenantID: "tenant-acme",
		Payload:  scopeTenant,
	}); pubErr != nil {
		t.Fatalf("Publish(Scope-bearing event): %v", err)
	}

	select {
	case v := <-fired:
		if v.Scope != config.ScopeTenant {
			t.Errorf("config watcher fired with Scope %q, want %q", v.Scope, config.ScopeTenant)
		}
	default:
		t.Fatal("config watcher never fired for a well-formed payload its own decoder accepts")
	}

	rows, err = auditRepo.ListByTenant(context.Background(), "tenant-acme")
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("well-formed payload produced %d audit rows, want exactly 1", len(rows))
	}
}
