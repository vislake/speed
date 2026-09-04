package integration

import (
	"testing"

	"gorm.io/datatypes"

	"github.com/vislake/speed/go/dbkit"
)

func TestWebhookSubscription_ImplementsTenantScoped(t *testing.T) {
	s := WebhookSubscription{TenantModel: dbkit.TenantModel{TenantID: "tenant-1"}}
	if got := s.GetTenantID(); string(got) != "tenant-1" {
		t.Errorf("GetTenantID() = %q, want %q", got, "tenant-1")
	}
}

func TestWebhookDelivery_ImplementsTenantScoped(t *testing.T) {
	d := WebhookDelivery{TenantModel: dbkit.TenantModel{TenantID: "tenant-1"}}
	if got := d.GetTenantID(); string(got) != "tenant-1" {
		t.Errorf("GetTenantID() = %q, want %q", got, "tenant-1")
	}
}

func TestEventTypesJSON_RoundTrip(t *testing.T) {
	types := []string{"org.member.joined", "org.member.removed"}
	stored := eventTypesJSON(types)

	got, err := parseEventTypes(stored)
	if err != nil {
		t.Fatalf("parseEventTypes: %v", err)
	}
	if len(got) != len(types) || got[0] != types[0] || got[1] != types[1] {
		t.Errorf("round-trip = %v, want %v", got, types)
	}
}

// TestEventTypesJSON_Nil_StoresEmptyArray_NeverNull mirrors
// TestScopesJSON_Nil_StoresEmptyArray_NeverNull's identical contract for
// this file's own JSON column.
func TestEventTypesJSON_Nil_StoresEmptyArray_NeverNull(t *testing.T) {
	stored := eventTypesJSON(nil)
	if string(stored) != "[]" {
		t.Errorf("eventTypesJSON(nil) = %q, want %q", stored, "[]")
	}
	got, err := parseEventTypes(stored)
	if err != nil {
		t.Fatalf("parseEventTypes: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("parseEventTypes(eventTypesJSON(nil)) = %v, want an empty slice", got)
	}
}

func TestParseEventTypes_CorruptValue_ReturnsError(t *testing.T) {
	invalid := datatypes.JSON([]byte(`not json`))
	if _, err := parseEventTypes(invalid); err == nil {
		t.Error("parseEventTypes(corrupt) error = nil, want a decode error")
	}
}
