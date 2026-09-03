package dbkit

import (
	"context"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit/internal/testutil"
)

// newSoftDeleteScopedTestDB opens a fresh in-memory SQLite database with the
// Widget and soft_deletable_widgets tables and softDeleteScopePlugin
// installed — but not tenantScopePlugin, mirroring tenant_scope_test.go's
// newScopedTestDB: each plugin's own test file exercises it in isolation,
// so a failure here can never be blamed on the other plugin's interaction.
func newSoftDeleteScopedTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := testutil.NewTestSQLite(t)
	if err := db.Exec(testutil.SoftDeletableWidgetTableSQL).Error; err != nil {
		t.Fatalf("create soft_deletable_widgets table: %v", err)
	}
	if err := db.Use(newSoftDeleteScopePlugin()); err != nil {
		t.Fatalf("install softDeleteScopePlugin: %v", err)
	}
	return db
}

// mustCreateSoftDeletableWidget creates w and fails the test on error.
func mustCreateSoftDeletableWidget(t *testing.T, db *gorm.DB, w *testutil.SoftDeletableWidget) {
	t.Helper()
	if err := db.WithContext(context.Background()).Create(w).Error; err != nil {
		t.Fatalf("seed soft-deletable widget %+v: %v", w, err)
	}
}

// TestSoftDeleteScopePlugin_Query_AppendsDeletedAtIsNull proves the plugin's
// core behavior: a row whose deleted_at column is set is hidden from an
// ordinary Find against a SoftDeletable model, while a row with deleted_at
// still NULL remains visible.
func TestSoftDeleteScopePlugin_Query_AppendsDeletedAtIsNull(t *testing.T) {
	db := newSoftDeleteScopedTestDB(t)

	live := &testutil.SoftDeletableWidget{ID: "live-1", TenantID: "tenant-a", Name: "live"}
	mustCreateSoftDeletableWidget(t, db, live)

	deletedAt := "2026-01-01 00:00:00"
	deleted := &testutil.SoftDeletableWidget{ID: "deleted-1", TenantID: "tenant-a", Name: "deleted"}
	mustCreateSoftDeletableWidget(t, db, deleted)
	if err := db.Exec(
		`UPDATE soft_deletable_widgets SET deleted_at = ?, deleted_by = ? WHERE id = ?`,
		deletedAt, "user-1", "deleted-1",
	).Error; err != nil {
		t.Fatalf("mark deleted-1 deleted via raw SQL: %v", err)
	}

	var got []testutil.SoftDeletableWidget
	if err := db.Find(&got).Error; err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "live-1" {
		t.Fatalf("Find() = %+v, want exactly [live-1] (the soft-deleted row must be hidden)", got)
	}
}

// TestSoftDeleteScopePlugin_UnscopedQuery_SeesSoftDeletedRows proves
// db.Unscoped() — GORM's own general query-scope bypass — is the sanctioned
// route past the auto-appended "deleted_at IS NULL" filter, per
// soft_delete.go's own doc comment.
func TestSoftDeleteScopePlugin_UnscopedQuery_SeesSoftDeletedRows(t *testing.T) {
	db := newSoftDeleteScopedTestDB(t)

	w := &testutil.SoftDeletableWidget{ID: "w1", TenantID: "tenant-a", Name: "gadget"}
	mustCreateSoftDeletableWidget(t, db, w)
	if err := db.Exec(
		`UPDATE soft_deletable_widgets SET deleted_at = ?, deleted_by = ? WHERE id = ?`,
		"2026-01-01 00:00:00", "user-1", "w1",
	).Error; err != nil {
		t.Fatalf("mark w1 deleted via raw SQL: %v", err)
	}

	var scoped []testutil.SoftDeletableWidget
	if err := db.Find(&scoped).Error; err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if len(scoped) != 0 {
		t.Fatalf("scoped Find() = %+v, want empty", scoped)
	}

	var unscoped []testutil.SoftDeletableWidget
	if err := db.Unscoped().Find(&unscoped).Error; err != nil {
		t.Fatalf("Unscoped().Find() error = %v", err)
	}
	if len(unscoped) != 1 || unscoped[0].ID != "w1" {
		t.Fatalf("Unscoped().Find() = %+v, want exactly [w1]", unscoped)
	}
}

// TestSoftDeleteScopePlugin_NonSoftDeletableModel_Unaffected mirrors
// TestTenantScopePlugin_NonTenantScopedModel_Unaffected: a model with no
// GetDeletedAt method (Widget, which has no deleted_at column at all) must
// behave identically whether softDeleteScopePlugin is installed or not — a
// mistakenly injected "deleted_at IS NULL" filter would fail loudly as a
// SQL error on the plugin-installed database, since widgets has no such
// column, which this test would catch immediately.
func TestSoftDeleteScopePlugin_NonSoftDeletableModel_Unaffected(t *testing.T) {
	withPlugin := newSoftDeleteScopedTestDB(t)
	without := testutil.NewTestSQLite(t)

	for _, tc := range []struct {
		name string
		db   *gorm.DB
	}{
		{name: "with plugin installed", db: withPlugin},
		{name: "without plugin installed", db: without},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := tc.db
			w := &testutil.Widget{ID: "w1", TenantID: "tenant-a", Name: "gadget"}
			if err := db.Create(w).Error; err != nil {
				t.Fatalf("Create() error = %v (a non-SoftDeletable model must never need a deleted_at column)", err)
			}

			var got []testutil.Widget
			if err := db.Find(&got).Error; err != nil {
				t.Fatalf("Find() error = %v", err)
			}
			if len(got) != 1 || got[0].ID != "w1" {
				t.Fatalf("Find() = %+v, want exactly [w1]", got)
			}
		})
	}
}
