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

// ---------------------------------------------------------------------------
// Or()-composition regression tests.
// ---------------------------------------------------------------------------

// seedSoftDeleteOrScenario populates db with the three rows every test in
// this section queries: a live row named "x" (id live-x), a soft-deleted row
// also named "x" (id deleted-x), and a live row named "y" (id live-y). The
// two same-named x rows are what make the shape dangerous: "the row named
// x" is ambiguous between a live row and a soft-deleted one, so a filter
// that fails to bind to the query's first OR branch is caught by exactly
// this pairing.
//
// The soft-deleted x row is created and marked deleted FIRST, then the live
// x row is inserted — the fixture table's partial unique index on
// (tenant_id, name) WHERE deleted_at IS NULL (the unique-index tests in
// this file pin that adjudicated answer) is exactly what makes that order
// legal, and it is the order a real soft-delete-then-recreate lifecycle
// produces.
func seedSoftDeleteOrScenario(t *testing.T, db *gorm.DB) {
	t.Helper()
	mustCreateSoftDeletableWidget(t, db, &testutil.SoftDeletableWidget{
		ID: "deleted-x", TenantID: "tenant-a", Name: "x",
	})
	if err := db.Exec(
		`UPDATE soft_deletable_widgets SET deleted_at = ?, deleted_by = ? WHERE id = ?`,
		"2026-01-01 00:00:00", "user-1", "deleted-x",
	).Error; err != nil {
		t.Fatalf("mark deleted-x deleted via raw SQL: %v", err)
	}
	mustCreateSoftDeletableWidget(t, db, &testutil.SoftDeletableWidget{
		ID: "live-x", TenantID: "tenant-a", Name: "x",
	})
	mustCreateSoftDeletableWidget(t, db, &testutil.SoftDeletableWidget{
		ID: "live-y", TenantID: "tenant-a", Name: "y",
	})
}

// This section covers a gap none of the core-contract tests above exercise:
// what softDeleteScopeBeforeQuery actually produces when the CALLER's own
// query already carries an Or(...) branch, rather than the plain,
// single-condition (or no-condition) shapes every other test in this file
// builds. It is the soft-delete mirror of tenant_scope_test.go's own
// Or()-composition section, and the analysis is identical to that section's:
//
// SQL gives AND strictly higher precedence than OR, and gorm's own
// clause-building only auto-parenthesizes a *raw string* condition whose
// SQL text itself visibly contains "AND "/"OR " — it does NOT parenthesize a
// structured chain built via the separate .Or(...) builder method before a
// later, unrelated condition is merged onto it. So:
//
//	db.Where("name = ?", "x").Or("name = ?", "y")     // caller code
//	// ... softDeleteScopeBeforeQuery later appends:
//	db.Statement.Where("deleted_at IS NULL")
//
// would render as `name = ? OR name = ? AND deleted_at IS NULL`, which — by
// normal SQL operator precedence — parses as
// `name = ? OR (name = ? AND deleted_at IS NULL)`, not the intended
// `(name = ? OR name = ?) AND deleted_at IS NULL`. The first branch of the
// OR carries no soft-delete filter at all, so it matches soft-deleted rows
// just like live ones — breaching the plugin's whole documented purpose,
// "soft-deleted rows are invisible to ordinary reads", the moment a caller's
// query shape includes an Or(). The fix mirrors tenant_scope.go's own:
// group the caller's existing conditions first, then append the
// soft-delete predicate as one AND'd sibling.
func TestSoftDeleteScopeBeforeQuery_CallerOrCondition_DeletedAtFilterAppliesToEveryBranch(t *testing.T) {
	db := newSoftDeleteScopedTestDB(t)
	seedSoftDeleteOrScenario(t, db)

	var got []testutil.SoftDeletableWidget
	err := db.
		Where("name = ?", "x").
		Or("name = ?", "y").
		Find(&got).Error
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}

	for _, w := range got {
		if w.ID == "deleted-x" {
			t.Errorf("Where(name=x).Or(name=y) query returned soft-deleted row %+v; "+
				"the appended deleted_at IS NULL filter must bind to every OR branch, not just the last one "+
				"(SQL operator precedence makes 'a OR b AND deleted_at IS NULL' mean 'a OR (b AND deleted_at IS NULL)', "+
				"leaving the first branch completely unfiltered by soft-delete)", w)
		}
	}
	if len(got) != 2 {
		t.Errorf("Where(name=x).Or(name=y) query returned %d rows %+v, want exactly 2 (live-x and live-y; the soft-deleted x row must stay hidden)",
			len(got), got)
	}
}

// TestSoftDeleteScopeBeforeQuery_CallerRawOrExpression_DeletedAtFilterAppliesToEveryBranch
// is the contrasting, currently-safe shape: the same logical OR, expressed
// as a single raw SQL string instead of the chained .Or(...) builder. gorm
// heuristically parenthesizes a raw condition string whenever it visibly
// contains " AND "/" OR " and more than one WHERE expression is present, so
// this shape composes correctly with the plugin's appended clause even
// though the .Or(...) shape above does not. It is kept here — exactly as its
// tenant_scope_test.go counterpart is — so a future fix to the case above
// has a passing witness of the shape it must not regress.
func TestSoftDeleteScopeBeforeQuery_CallerRawOrExpression_DeletedAtFilterAppliesToEveryBranch(t *testing.T) {
	db := newSoftDeleteScopedTestDB(t)
	seedSoftDeleteOrScenario(t, db)

	var got []testutil.SoftDeletableWidget
	err := db.
		Where("name = ? OR name = ?", "x", "y").
		Find(&got).Error
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}

	for _, w := range got {
		if w.ID == "deleted-x" {
			t.Errorf("raw 'name = ? OR name = ?' query returned soft-deleted row %+v, want it hidden", w)
		}
	}
	if len(got) != 2 {
		t.Errorf("raw 'name = ? OR name = ?' query returned %d rows %+v, want exactly 2 (live-x and live-y)", len(got), got)
	}
}

// TestSoftDeleteScopeBeforeQuery_UnscopedOrCondition_SeesSoftDeletedRows is
// the bypass-semantics witness for the shape above: the very same
// Where(name=x).Or(name=y) query under db.Unscoped() — the documented route
// past the auto-appended "deleted_at IS NULL" filter, per soft_delete.go's
// own doc comment — must return the soft-deleted x row alongside the two
// live ones. It pins what the scope is supposed to do (hide soft-deleted
// rows from ordinary reads only) so the regression test above cannot be
// "fixed" by weakening the filter instead of strengthening its binding.
func TestSoftDeleteScopeBeforeQuery_UnscopedOrCondition_SeesSoftDeletedRows(t *testing.T) {
	db := newSoftDeleteScopedTestDB(t)
	seedSoftDeleteOrScenario(t, db)

	var got []testutil.SoftDeletableWidget
	err := db.Unscoped().
		Where("name = ?", "x").
		Or("name = ?", "y").
		Find(&got).Error
	if err != nil {
		t.Fatalf("Unscoped().Where(name=x).Or(name=y).Find() error = %v", err)
	}

	ids := make(map[string]bool, len(got))
	for _, w := range got {
		ids[w.ID] = true
	}
	for _, want := range []string{"live-x", "deleted-x", "live-y"} {
		if !ids[want] {
			t.Errorf("Unscoped() query returned rows %+v, missing %q (bypassing the scope must reveal the soft-deleted row)", got, want)
		}
	}
}

// TestSoftDeleteScopePlugin_RowPath_Scan_SkipsSoftDeletedRows is regression
// (c), the soft-delete twin of tenant_scope_test.go's row-path regressions:
// db.Model(&SoftDeletableWidget{}).Scan(&rows) — the projection/query shape
// that routes through GORM's row processor, where the plugin registered no
// callback until this round — must hide soft-deleted rows exactly like
// Find. Pre-fix the scan returned the soft-deleted row with a nil error.
func TestSoftDeleteScopePlugin_RowPath_Scan_SkipsSoftDeletedRows(t *testing.T) {
	db := newSoftDeleteScopedTestDB(t)

	live := &testutil.SoftDeletableWidget{ID: "live-1", TenantID: "tenant-a", Name: "live"}
	mustCreateSoftDeletableWidget(t, db, live)

	deleted := &testutil.SoftDeletableWidget{ID: "deleted-1", TenantID: "tenant-a", Name: "deleted"}
	mustCreateSoftDeletableWidget(t, db, deleted)
	if err := db.Exec(
		`UPDATE soft_deletable_widgets SET deleted_at = ?, deleted_by = ? WHERE id = ?`,
		"2026-01-01 00:00:00", "user-1", "deleted-1",
	).Error; err != nil {
		t.Fatalf("mark deleted-1 deleted via raw SQL: %v", err)
	}

	t.Run("Scan", func(t *testing.T) {
		var got []testutil.SoftDeletableWidget
		if err := db.Model(&testutil.SoftDeletableWidget{}).Scan(&got).Error; err != nil {
			t.Fatalf("Scan() error = %v", err)
		}
		if len(got) != 1 || got[0].ID != "live-1" {
			t.Fatalf("Scan() = %+v, want exactly [live-1] (the soft-deleted row must be hidden)", got)
		}
	})

	t.Run("Unscoped Scan", func(t *testing.T) {
		var got []testutil.SoftDeletableWidget
		if err := db.Unscoped().Model(&testutil.SoftDeletableWidget{}).Scan(&got).Error; err != nil {
			t.Fatalf("Unscoped().Scan() error = %v", err)
		}
		ids := make(map[string]bool, len(got))
		for _, w := range got {
			ids[w.ID] = true
		}
		if !ids["live-1"] || !ids["deleted-1"] {
			t.Fatalf("Unscoped().Scan() = %+v, want both [live-1 deleted-1] (the scope's own bypass must keep working on the row path)", got)
		}
	})
}

// TestSoftDeleteUniqueIndex_NameReusableAfterSoftDelete_ViaPartialIndex is
// this round's proof for the unique-index interaction
// docs/internal/04-data-and-tenancy.md's delete-semantics section (§4) requires every
// soft-deletable model to decide explicitly: a soft-deleted row is still a
// real row and still occupies a plain unique constraint. This round's
// answer, for SoftDeletableWidget (testutil.SoftDeletableWidgetTableSQL),
// is a partial unique index on (tenant_id, name) WHERE deleted_at IS NULL —
// proven here to actually let a name be reused immediately after its
// holder is soft-deleted, on SQLite; the identical DDL string is exercised
// again against real PostgreSQL in
// integration_test/postgres_soft_delete_rls_test.go, which is the
// dual-dialect half of this proof. See go/dbkit/AGENTS.md's "Soft
// deletion" section for the general guidance this backs, and this
// fixture's own doc comment for why the alternative ("no reuse until
// hard-deleted") remains legitimate for a model that wants it instead.
func TestSoftDeleteUniqueIndex_NameReusableAfterSoftDelete_ViaPartialIndex(t *testing.T) {
	repo := newSoftDeletableWidgetRepo(t)
	ctx := ctxTenantActor("tenant-a", "user-1")

	original := &testutil.SoftDeletableWidget{ID: "w1", Name: "x"}
	if err := repo.Create(ctx, original); err != nil {
		t.Fatalf("Create(original) error = %v", err)
	}

	// Attempting to create a second live row with the same name, before any
	// delete, must still be rejected — proving the partial index really is
	// enforcing uniqueness among live rows, not merely absent.
	dupe := &testutil.SoftDeletableWidget{ID: "w-dupe", Name: "x"}
	if err := repo.Create(ctx, dupe); err == nil {
		t.Fatal("Create() of a second live row with the same name succeeded, want a unique-constraint violation")
	}

	if err := repo.Delete(ctx, original.ID); err != nil {
		t.Fatalf("Delete(original) error = %v", err)
	}

	// The partial index no longer covers the soft-deleted row (its
	// deleted_at is no longer NULL), so a brand new row with the same name
	// under the same tenant must now succeed.
	reborn := &testutil.SoftDeletableWidget{ID: "w2", Name: "x"}
	if err := repo.Create(ctx, reborn); err != nil {
		t.Fatalf("Create() reusing the name after soft-delete error = %v, want success (partial unique index WHERE deleted_at IS NULL)", err)
	}

	got, err := repo.FindByID(ctx, reborn.ID)
	if err != nil {
		t.Fatalf("FindByID(reborn) error = %v", err)
	}
	if got.Name != "x" {
		t.Errorf("FindByID(reborn).Name = %q, want %q", got.Name, "x")
	}
}

// TestSoftDeleteUniqueIndex_TwoTenantsCanEachHaveALiveRowWithTheSameName
// proves the partial index is still tenant-scoped via its leftmost column:
// two tenants each holding a live "x" concurrently must not collide with
// one another, exactly as an ordinary (non-partial) UNIQUE(tenant_id, name)
// index would behave.
func TestSoftDeleteUniqueIndex_TwoTenantsCanEachHaveALiveRowWithTheSameName(t *testing.T) {
	repo := newSoftDeletableWidgetRepo(t)
	ctxA := ctxTenantActor("tenant-a", "user-1")
	ctxB := ctxTenantActor("tenant-b", "user-1")

	if err := repo.Create(ctxA, &testutil.SoftDeletableWidget{ID: "a1", Name: "x"}); err != nil {
		t.Fatalf("Create(tenant-a) error = %v", err)
	}
	if err := repo.Create(ctxB, &testutil.SoftDeletableWidget{ID: "b1", Name: "x"}); err != nil {
		t.Fatalf("Create(tenant-b) error = %v, want success -- the partial index is scoped by tenant_id, its leftmost column", err)
	}
}
