package testutil

import (
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

func TestNewTestSQLite_CreateThenFirst_RoundTripsWidget(t *testing.T) {
	db := NewTestSQLite(t)

	widget := Widget{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", TenantID: "tenant-a", Name: "gadget", Value: 7}
	if err := db.Create(&widget).Error; err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var got Widget
	if err := db.First(&got, "id = ? AND tenant_id = ?", widget.ID, widget.TenantID).Error; err != nil {
		t.Fatalf("First() error = %v", err)
	}
	if got != widget {
		t.Errorf("First() = %+v, want %+v", got, widget)
	}
	if got.GetTenantID() != pkgcore.TenantID("tenant-a") {
		t.Errorf("GetTenantID() = %q, want %q", got.GetTenantID(), "tenant-a")
	}
}

func TestNewTestSQLite_SameIDAcrossTenants_BothRowsPersist(t *testing.T) {
	// The primary key is (tenant_id, id), not id alone, so two different
	// tenants may each own a widget with the same id. This is the schema
	// property the whole multi-tenant isolation design depends on: if it
	// ever regresses to a bare "id" primary key, this test starts failing
	// with a duplicate-key error on the second Create.
	db := NewTestSQLite(t)

	const sharedID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	widgets := []Widget{
		{ID: sharedID, TenantID: "tenant-a", Name: "a-gadget"},
		{ID: sharedID, TenantID: "tenant-b", Name: "b-gadget"},
	}
	for _, w := range widgets {
		if err := db.Create(&w).Error; err != nil {
			t.Fatalf("Create(%+v) error = %v", w, err)
		}
	}

	var count int64
	if err := db.Model(&Widget{}).Where("id = ?", sharedID).Count(&count).Error; err != nil {
		t.Fatalf("Count() error = %v", err)
	}
	if count != 2 {
		t.Errorf("Count() = %d, want 2 (one row per tenant sharing the same id)", count)
	}
}

func TestNewTestSQLite_CalledTwice_ReturnsIndependentDatabases(t *testing.T) {
	first := NewTestSQLite(t)
	if err := first.Create(&Widget{ID: "id-1", TenantID: "tenant-a", Name: "gadget"}).Error; err != nil {
		t.Fatalf("Create() on first database error = %v", err)
	}

	second := NewTestSQLite(t)
	var count int64
	if err := second.Model(&Widget{}).Count(&count).Error; err != nil {
		t.Fatalf("Count() on second database error = %v", err)
	}
	if count != 0 {
		t.Errorf("Count() on second database = %d, want 0; NewTestSQLite must not share state across calls", count)
	}
}
