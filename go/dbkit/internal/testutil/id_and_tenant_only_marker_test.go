package testutil

import (
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

func TestIDAndTenantOnlyMarker_GetTenantID_ReturnsTenantIDField(t *testing.T) {
	tests := []struct {
		name string
		m    IDAndTenantOnlyMarker
		want pkgcore.TenantID
	}{
		{name: "non-empty tenant", m: IDAndTenantOnlyMarker{TenantID: "tenant-a"}, want: pkgcore.TenantID("tenant-a")},
		{name: "empty tenant", m: IDAndTenantOnlyMarker{TenantID: ""}, want: pkgcore.TenantID("")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.m.GetTenantID(); got != tt.want {
				t.Errorf("GetTenantID() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestIDAndTenantOnlyMarkerTableSQL_CreatesUsableTable is a hermetic,
// no-database-server unit test proving IDAndTenantOnlyMarkerTableSQL is
// actually valid, executable DDL — not just a string that happens to
// contain the right substrings — by running it against a real (in-memory)
// SQLite connection and inserting through it. This is the SQLite half of
// the "identical DDL on both dialects" claim in the constant's own doc
// comment; the PostgreSQL half is exercised by
// integration_test/postgres_tenant_isolation_test.go, which applies this
// exact constant against a real PostgreSQL server.
func TestIDAndTenantOnlyMarkerTableSQL_CreatesUsableTable(t *testing.T) {
	if !strings.Contains(IDAndTenantOnlyMarkerTableSQL, "CREATE TABLE id_and_tenant_only_markers") {
		t.Fatalf("IDAndTenantOnlyMarkerTableSQL = %q, want it to contain %q", IDAndTenantOnlyMarkerTableSQL, "CREATE TABLE id_and_tenant_only_markers")
	}
	if !strings.Contains(IDAndTenantOnlyMarkerTableSQL, "PRIMARY KEY (tenant_id, id)") {
		t.Errorf("IDAndTenantOnlyMarkerTableSQL = %q, want a PRIMARY KEY (tenant_id, id) clause", IDAndTenantOnlyMarkerTableSQL)
	}

	// NewTestSQLite only applies Widget's migration, so this table is
	// created by hand here too, exactly like both real call sites do.
	db := NewTestSQLite(t)
	if err := db.Exec(IDAndTenantOnlyMarkerTableSQL).Error; err != nil {
		t.Fatalf("exec IDAndTenantOnlyMarkerTableSQL: %v", err)
	}

	marker := IDAndTenantOnlyMarker{ID: "marker-1", TenantID: "tenant-a"}
	if err := db.Create(&marker).Error; err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var got IDAndTenantOnlyMarker
	if err := db.First(&got, "id = ? AND tenant_id = ?", marker.ID, marker.TenantID).Error; err != nil {
		t.Fatalf("First() error = %v", err)
	}
	if got != marker {
		t.Errorf("First() = %+v, want %+v", got, marker)
	}
}
