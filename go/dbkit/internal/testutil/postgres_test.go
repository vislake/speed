package testutil

import (
	"strings"
	"testing"
)

// TestWidgetPostgresMigrationSQL_ReturnsCreateTableWidgets is a hermetic,
// no-database unit test for WidgetPostgresMigrationSQL's own embed-and-read
// logic: it needs no running PostgreSQL server (that is what the
// integration_test tier's postgres_tenant_isolation_test.go and
// postgres_rls_test.go exercise, applying this exact SQL against a real
// server), but it does confirm the embedded file path is still correct and
// the fixture still defines the table those integration tests, and every
// dual-dialect assertion resting on it, assume exists.
func TestWidgetPostgresMigrationSQL_ReturnsCreateTableWidgets(t *testing.T) {
	sql := WidgetPostgresMigrationSQL(t)

	if !strings.Contains(sql, "CREATE TABLE widgets") {
		t.Errorf("WidgetPostgresMigrationSQL() = %q, want it to contain \"CREATE TABLE widgets\"", sql)
	}
	// The multi-tenant isolation standard's (tenant_id, id) primary-key
	// convention, tenant_id leftmost — the same property
	// TestNewTestSQLite_SameIDAcrossTenants_BothRowsPersist locks in for the
	// SQLite copy of this fixture.
	if !strings.Contains(sql, "PRIMARY KEY (tenant_id, id)") {
		t.Errorf("WidgetPostgresMigrationSQL() = %q, want a PRIMARY KEY (tenant_id, id) clause", sql)
	}
}
