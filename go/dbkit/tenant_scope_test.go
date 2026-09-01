package dbkit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// tenantA and tenantB are the two tenants used throughout this file's
// isolation tests.
const (
	tenantA = pkgcore.TenantID("tenant-a")
	tenantB = pkgcore.TenantID("tenant-b")
)

// ctxFor returns a context carrying tid as the current tenant.
func ctxFor(tid pkgcore.TenantID) context.Context {
	return pkgcore.WithTenant(context.Background(), tid)
}

// platformFlag is a tiny fixture standing in for platform-domain data (see
// docs/internal/04-data-and-tenancy.md's data-domain table): global, not
// owned by any tenant, and read/written the same way regardless of who is
// asking. It carries no GetTenantID method, so tenantScopePlugin must leave
// it completely alone — this is the "AssertNotTenantScoped" side of the
// isolation contract, exercised below by
// TestTenantScopePlugin_NonTenantScopedModel_Unaffected. It is also reused
// later in this file, in the detection-mechanism section's
// isTenantScopedValue table, as the "not tenant-scoped" shape.
type platformFlag struct {
	Key     string `gorm:"column:key;primaryKey;size:64"`
	Enabled bool   `gorm:"column:enabled;not null"`
}

// TableName pins platformFlag's table name explicitly so the raw CREATE
// TABLE in createPlatformFlagsTable matches it exactly, independent of
// GORM's pluralization rules.
func (platformFlag) TableName() string { return "platform_flags" }

// createPlatformFlagsTable adds platformFlag's table to db via a plain Exec
// — not AutoMigrate, matching the project-wide "no AutoMigrate" rule even
// for a throwaway in-memory test fixture.
func createPlatformFlagsTable(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.Exec(`CREATE TABLE platform_flags (key VARCHAR(64) PRIMARY KEY, enabled BOOLEAN NOT NULL)`).Error; err != nil {
		t.Fatalf("create platform_flags table: %v", err)
	}
}

// newScopedTestDB opens a fresh in-memory SQLite database with the Widget
// and platformFlag tables and tenantScopePlugin installed.
func newScopedTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := testutil.NewTestSQLite(t)
	createPlatformFlagsTable(t, db)
	if err := db.Use(newTenantScopePlugin()); err != nil {
		t.Fatalf("install tenantScopePlugin: %v", err)
	}
	return db
}

// mustCreateWidget creates w under tid's tenant context and fails the test
// on error. It is the seeding helper for every isolation test below.
func mustCreateWidget(t *testing.T, db *gorm.DB, tid pkgcore.TenantID, w *testutil.Widget) {
	t.Helper()
	if err := db.WithContext(ctxFor(tid)).Create(w).Error; err != nil {
		t.Fatalf("seed widget %+v under tenant %q: %v", w, tid, err)
	}
}

// rawTenantID reads a widget's tenant_id column directly through db.Raw,
// bypassing every GORM callback — tenantScopePlugin included. This is the
// "system/no-plugin raw check" the isolation tests use to verify what
// actually landed in the database, rather than trusting the plugin's own
// account of what it did.
func rawTenantID(t *testing.T, db *gorm.DB, widgetID string) string {
	t.Helper()
	var tid string
	if err := db.Raw(`SELECT tenant_id FROM widgets WHERE id = ?`, widgetID).Scan(&tid).Error; err != nil {
		t.Fatalf("raw tenant_id lookup for widget %q: %v", widgetID, err)
	}
	return tid
}

// rawCount reads, via db.Raw, how many widget rows exist with the given id.
func rawCount(t *testing.T, db *gorm.DB, widgetID string) int64 {
	t.Helper()
	var n int64
	if err := db.Raw(`SELECT COUNT(*) FROM widgets WHERE id = ?`, widgetID).Scan(&n).Error; err != nil {
		t.Fatalf("raw count lookup for widget %q: %v", widgetID, err)
	}
	return n
}

// rawValue reads, via db.Raw, a widget row's value column.
func rawValue(t *testing.T, db *gorm.DB, widgetID string) int {
	t.Helper()
	var v int
	if err := db.Raw(`SELECT value FROM widgets WHERE id = ?`, widgetID).Scan(&v).Error; err != nil {
		t.Fatalf("raw value lookup for widget %q: %v", widgetID, err)
	}
	return v
}

// ---------------------------------------------------------------------------
// Core contract tests: fail-closed behavior on every operation, per-tenant
// isolation across Create/Query/Update/Delete, the Create-time tenant
// override, and concurrency safety under -race.
// ---------------------------------------------------------------------------

// TestTenantScopeBeforeQuery_NoTenantInContext_FailsClosed covers the
// single most basic contract: a read against a tenant-scoped model with no
// tenant in context must fail with an error, and must never populate its
// destination with data — neither leaking unfiltered rows nor silently
// looking like "no data" via a nil error.
func TestTenantScopeBeforeQuery_NoTenantInContext_FailsClosed(t *testing.T) {
	db := newScopedTestDB(t)
	mustCreateWidget(t, db, tenantA, &testutil.Widget{ID: "seed", TenantID: string(tenantA), Name: "seed", Value: 7})
	noTenant := context.Background()

	t.Run("Find", func(t *testing.T) {
		var widgets []testutil.Widget
		err := db.WithContext(noTenant).Find(&widgets).Error
		if err == nil {
			t.Fatalf("Find() error = nil, want an error (fail closed)")
		}
		if !errors.Is(err, pkgcore.ErrNoTenant) {
			t.Errorf("Find() error = %v, want it to wrap pkgcore.ErrNoTenant", err)
		}
		if len(widgets) != 0 {
			t.Errorf("Find() populated %d widgets despite the error; no rows must leak", len(widgets))
		}
	})

	t.Run("First", func(t *testing.T) {
		var w testutil.Widget
		err := db.WithContext(noTenant).First(&w).Error
		if err == nil {
			t.Fatalf("First() error = nil, want an error (fail closed)")
		}
		if !errors.Is(err, pkgcore.ErrNoTenant) {
			t.Errorf("First() error = %v, want it to wrap pkgcore.ErrNoTenant", err)
		}
		if w != (testutil.Widget{}) {
			t.Errorf("First() populated %+v despite the error; no row must leak", w)
		}
	})

	t.Run("Count", func(t *testing.T) {
		var n int64
		err := db.WithContext(noTenant).Model(&testutil.Widget{}).Count(&n).Error
		if err == nil {
			t.Fatalf("Count() error = nil, want an error (fail closed)")
		}
		if !errors.Is(err, pkgcore.ErrNoTenant) {
			t.Errorf("Count() error = %v, want it to wrap pkgcore.ErrNoTenant", err)
		}
		if n != 0 {
			t.Errorf("Count() = %d, want 0; no rows must leak through a failed, fail-closed count", n)
		}
	})
}

// TestTenantScopePlugin_WriteWithoutTenantInContext_FailsClosed covers the
// write side of the same contract: Create, Update and Delete against a
// tenant-scoped model must all fail closed when the context carries no
// tenant, and none of them may mutate the database.
func TestTenantScopePlugin_WriteWithoutTenantInContext_FailsClosed(t *testing.T) {
	noTenant := context.Background()

	tests := []struct {
		name string
		run  func(db *gorm.DB) error
	}{
		{
			name: "Create",
			run: func(db *gorm.DB) error {
				w := testutil.Widget{ID: "orphan-create", TenantID: string(tenantA), Name: "orphan"}
				return db.WithContext(noTenant).Create(&w).Error
			},
		},
		{
			name: "Update",
			run: func(db *gorm.DB) error {
				return db.WithContext(noTenant).Model(&testutil.Widget{}).Where("id = ?", "seed").Update("value", 2).Error
			},
		},
		{
			name: "Delete",
			run: func(db *gorm.DB) error {
				return db.WithContext(noTenant).Where("id = ?", "seed").Delete(&testutil.Widget{}).Error
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newScopedTestDB(t)
			mustCreateWidget(t, db, tenantA, &testutil.Widget{ID: "seed", TenantID: string(tenantA), Name: "seed", Value: 1})

			err := tt.run(db)
			if err == nil {
				t.Fatalf("%s() error = nil, want an error (fail closed)", tt.name)
			}
			if !errors.Is(err, pkgcore.ErrNoTenant) {
				t.Errorf("%s() error = %v, want it to wrap pkgcore.ErrNoTenant", tt.name, err)
			}

			if n := rawCount(t, db, "seed"); n != 1 {
				t.Errorf("row count for the seed widget = %d, want unchanged 1", n)
			}
			if v := rawValue(t, db, "seed"); v != 1 {
				t.Errorf("value for the seed widget = %d, want unchanged 1; a failed, fail-closed %s must not mutate it", v, tt.name)
			}
			if n := rawCount(t, db, "orphan-create"); n != 0 {
				t.Errorf("row count for %q = %d, want 0; a failed, fail-closed create must not insert anything", "orphan-create", n)
			}
		})
	}
}

// TestTenantScopeBeforeQuery_TwoTenants_OnlySeesOwnTenant seeds rows for two
// tenants in the same table and asserts that querying as tenant A, via
// Find, First and Count, only ever surfaces tenant A's own rows.
func TestTenantScopeBeforeQuery_TwoTenants_OnlySeesOwnTenant(t *testing.T) {
	db := newScopedTestDB(t)
	mustCreateWidget(t, db, tenantA, &testutil.Widget{ID: "w-a1", TenantID: string(tenantA), Name: "a-one", Value: 1})
	mustCreateWidget(t, db, tenantA, &testutil.Widget{ID: "w-a2", TenantID: string(tenantA), Name: "a-two", Value: 2})
	mustCreateWidget(t, db, tenantB, &testutil.Widget{ID: "w-b1", TenantID: string(tenantB), Name: "b-one", Value: 9})

	asA := ctxFor(tenantA)

	t.Run("Find", func(t *testing.T) {
		var widgets []testutil.Widget
		if err := db.WithContext(asA).Find(&widgets).Error; err != nil {
			t.Fatalf("Find() error = %v", err)
		}
		if len(widgets) != 2 {
			t.Fatalf("Find() returned %d widgets, want 2 (tenant A's own rows only)", len(widgets))
		}
		for _, w := range widgets {
			if w.TenantID != string(tenantA) {
				t.Errorf("Find() returned widget %+v belonging to a different tenant", w)
			}
		}
	})

	t.Run("First", func(t *testing.T) {
		var w testutil.Widget
		err := db.WithContext(asA).First(&w, "id = ?", "w-b1").Error
		if err == nil {
			t.Fatalf("First() for tenant B's widget id under tenant A context succeeded (got %+v), want gorm.ErrRecordNotFound", w)
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Errorf("First() error = %v, want gorm.ErrRecordNotFound", err)
		}

		if err := db.WithContext(asA).First(&w, "id = ?", "w-a1").Error; err != nil {
			t.Fatalf("First() for tenant A's own widget id error = %v", err)
		}
		if w.TenantID != string(tenantA) {
			t.Errorf("First() returned widget under tenant %q, want %q", w.TenantID, tenantA)
		}
	})

	t.Run("Count", func(t *testing.T) {
		var n int64
		if err := db.WithContext(asA).Model(&testutil.Widget{}).Count(&n).Error; err != nil {
			t.Fatalf("Count() error = %v", err)
		}
		if n != 2 {
			t.Errorf("Count() = %d, want 2 (tenant A's own rows only)", n)
		}
	})
}

// TestTenantScopeBeforeCreate_ForcesContextTenant_OverridesStructField
// proves the override in Create is real: even when the caller populates the
// struct's TenantID field with a different tenant before calling Create,
// the row lands under the context tenant. The database is checked with a
// raw query that bypasses tenantScopePlugin entirely, so this does not just
// trust the plugin's own bookkeeping.
func TestTenantScopeBeforeCreate_ForcesContextTenant_OverridesStructField(t *testing.T) {
	db := newScopedTestDB(t)

	w := testutil.Widget{ID: "w-spoofed", TenantID: string(tenantB), Name: "spoofed", Value: 42}
	if err := db.WithContext(ctxFor(tenantA)).Create(&w).Error; err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if w.TenantID != string(tenantA) {
		t.Errorf("after Create(), in-memory TenantID = %q, want %q (SetColumn writes through to the struct too)", w.TenantID, tenantA)
	}
	if got := rawTenantID(t, db, "w-spoofed"); got != string(tenantA) {
		t.Errorf("raw tenant_id in database = %q, want %q; Create() must not let a caller insert under a different tenant", got, tenantA)
	}
}

// TestTenantScopeBeforeCreate_BatchCreate_ForcesTenantOnEveryRow guards a
// subtle correctness detail: when Create is called on a slice (a batch
// insert), the tenant override must land on every row, not only the first.
// Before this callback's per-row create loop starts, forcing a single index
// via plain SetColumn would silently miss every row after the first; this
// is exactly what the plugin's SetColumn(..., fromCallbacks=true) call
// exists to prevent.
func TestTenantScopeBeforeCreate_BatchCreate_ForcesTenantOnEveryRow(t *testing.T) {
	db := newScopedTestDB(t)
	widgets := []testutil.Widget{
		{ID: "batch-1", TenantID: string(tenantB), Name: "one"},
		{ID: "batch-2", TenantID: string(tenantB), Name: "two"},
		{ID: "batch-3", TenantID: string(tenantB), Name: "three"},
	}

	if err := db.WithContext(ctxFor(tenantA)).Create(&widgets).Error; err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	for _, id := range []string{"batch-1", "batch-2", "batch-3"} {
		if got := rawTenantID(t, db, id); got != string(tenantA) {
			t.Errorf("raw tenant_id for %q = %q, want %q (every row of a batch create must be forced, not only the first)", id, got, tenantA)
		}
	}
}

// TestTenantScopeBeforeUpdate_CannotChangeTenantID covers both payload
// shapes GORM's Update/Updates accept — a map and a struct — and asserts
// that trying to also change TenantID to a different tenant in the same
// update payload is rejected outright (this package's documented choice:
// reject with ErrTenantIDImmutable, rather than silently drop the field and
// apply the rest of the update), and that nothing about the row changes as
// a result, not even the unrelated column in the same payload.
func TestTenantScopeBeforeUpdate_CannotChangeTenantID(t *testing.T) {
	tests := []struct {
		name    string
		payload interface{}
	}{
		{
			name:    "map payload keyed by column name",
			payload: map[string]interface{}{"tenant_id": string(tenantB), "value": 999},
		},
		{
			name:    "struct payload",
			payload: testutil.Widget{TenantID: string(tenantB), Value: 999},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newScopedTestDB(t)
			mustCreateWidget(t, db, tenantA, &testutil.Widget{ID: "w-guard", TenantID: string(tenantA), Name: "guarded", Value: 1})

			err := db.WithContext(ctxFor(tenantA)).
				Model(&testutil.Widget{}).
				Where("id = ?", "w-guard").
				Updates(tt.payload).Error
			if err == nil {
				t.Fatalf("Updates() error = nil, want ErrTenantIDImmutable; changing tenant_id via update must never succeed")
			}
			appErr, ok := apperr.As(err)
			if !ok {
				t.Fatalf("Updates() error = %v (%T), want an *apperr.Error", err, err)
			}
			if appErr.Code != ErrTenantIDImmutable.Code {
				t.Errorf("Updates() error code = %q, want %q", appErr.Code, ErrTenantIDImmutable.Code)
			}

			if got := rawTenantID(t, db, "w-guard"); got != string(tenantA) {
				t.Errorf("raw tenant_id after rejected update = %q, want unchanged %q", got, tenantA)
			}
			if v := rawValue(t, db, "w-guard"); v != 1 {
				t.Errorf("raw value after rejected update = %d, want unchanged 1 (the whole statement must be rejected, not just the tenant_id column)", v)
			}
		})
	}
}

// TestTenantScopeBeforeUpdate_HarmlessNoopTenantID asserts the flip side of
// the immutability guard: an update payload that redundantly restates the
// row's own, already-correct tenant_id is not an attempt to move the row
// anywhere and must not be rejected.
func TestTenantScopeBeforeUpdate_HarmlessNoopTenantID(t *testing.T) {
	db := newScopedTestDB(t)
	mustCreateWidget(t, db, tenantA, &testutil.Widget{ID: "w-noop", TenantID: string(tenantA), Name: "noop", Value: 1})

	err := db.WithContext(ctxFor(tenantA)).
		Model(&testutil.Widget{}).
		Where("id = ?", "w-noop").
		Updates(map[string]interface{}{"tenant_id": string(tenantA), "value": 2}).Error
	if err != nil {
		t.Fatalf("Updates() error = %v, want nil (restating the row's own tenant is a harmless no-op)", err)
	}
	if v := rawValue(t, db, "w-noop"); v != 2 {
		t.Errorf("raw value after update = %d, want 2", v)
	}
	if got := rawTenantID(t, db, "w-noop"); got != string(tenantA) {
		t.Errorf("raw tenant_id after update = %q, want unchanged %q", got, tenantA)
	}
}

// TestTenantScopeBeforeUpdate_SameNameAcrossTenants_OnlyAffectsOwnTenant
// seeds a same-named widget under both tenants and updates by that
// non-unique Name column as tenant A, proving the injected tenant_id filter
// — not the caller's own WHERE clause — is what keeps the update from ever
// reaching tenant B's row.
func TestTenantScopeBeforeUpdate_SameNameAcrossTenants_OnlyAffectsOwnTenant(t *testing.T) {
	db := newScopedTestDB(t)
	mustCreateWidget(t, db, tenantA, &testutil.Widget{ID: "shared-a", TenantID: string(tenantA), Name: "shared-name", Value: 1})
	mustCreateWidget(t, db, tenantB, &testutil.Widget{ID: "shared-b", TenantID: string(tenantB), Name: "shared-name", Value: 1})

	res := db.WithContext(ctxFor(tenantA)).
		Model(&testutil.Widget{}).
		Where("name = ?", "shared-name").
		Update("value", 100)
	if res.Error != nil {
		t.Fatalf("Update() error = %v", res.Error)
	}
	if res.RowsAffected != 1 {
		t.Fatalf("Update() RowsAffected = %d, want 1 (only tenant A's row, despite both tenants sharing the same name)", res.RowsAffected)
	}

	if v := rawValue(t, db, "shared-a"); v != 100 {
		t.Errorf("tenant A's row value = %d, want 100", v)
	}
	if v := rawValue(t, db, "shared-b"); v != 1 {
		t.Errorf("tenant B's row value = %d, want unchanged 1; an update filtered by a non-unique column must never reach another tenant's row", v)
	}
}

// TestTenantScopeBeforeDelete_SameNameAcrossTenants_OnlyAffectsOwnTenant is
// the Delete counterpart of the Update test above.
func TestTenantScopeBeforeDelete_SameNameAcrossTenants_OnlyAffectsOwnTenant(t *testing.T) {
	db := newScopedTestDB(t)
	mustCreateWidget(t, db, tenantA, &testutil.Widget{ID: "shared-a", TenantID: string(tenantA), Name: "shared-name"})
	mustCreateWidget(t, db, tenantB, &testutil.Widget{ID: "shared-b", TenantID: string(tenantB), Name: "shared-name"})

	res := db.WithContext(ctxFor(tenantA)).Where("name = ?", "shared-name").Delete(&testutil.Widget{})
	if res.Error != nil {
		t.Fatalf("Delete() error = %v", res.Error)
	}
	if res.RowsAffected != 1 {
		t.Fatalf("Delete() RowsAffected = %d, want 1 (only tenant A's row)", res.RowsAffected)
	}

	if n := rawCount(t, db, "shared-a"); n != 0 {
		t.Errorf("tenant A's row count after delete = %d, want 0", n)
	}
	if n := rawCount(t, db, "shared-b"); n != 1 {
		t.Errorf("tenant B's row count after delete = %d, want 1 (unaffected; a delete filtered by a non-unique column must never reach another tenant's row)", n)
	}
}

// TestTenantScopePlugin_NonTenantScopedModel_Unaffected runs the same
// sequence of operations against platformFlag — a model with no
// GetTenantID method — on two databases, one with tenantScopePlugin
// installed and one without, using no tenant context at all throughout.
// Both must behave identically: no error, and (because platform_flags has
// no tenant_id column at all) any accidentally injected WHERE tenant_id = ?
// would fail loudly as a SQL error on the plugin-installed database, which
// this test would catch immediately.
func TestTenantScopePlugin_NonTenantScopedModel_Unaffected(t *testing.T) {
	noTenant := context.Background()

	withPlugin := newScopedTestDB(t)
	without := testutil.NewTestSQLite(t)
	createPlatformFlagsTable(t, without)

	for _, tc := range []struct {
		name string
		db   *gorm.DB
	}{
		{name: "with plugin installed", db: withPlugin},
		{name: "without plugin installed", db: without},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := tc.db

			if err := db.WithContext(noTenant).Create(&platformFlag{Key: "feature.x", Enabled: true}).Error; err != nil {
				t.Fatalf("Create() error = %v (a non-tenant-scoped model must never need a tenant context)", err)
			}

			var flags []platformFlag
			if err := db.WithContext(noTenant).Find(&flags).Error; err != nil {
				t.Fatalf("Find() error = %v", err)
			}
			if len(flags) != 1 || flags[0].Key != "feature.x" || !flags[0].Enabled {
				t.Fatalf("Find() = %+v, want a single {feature.x, true} row", flags)
			}

			if err := db.WithContext(noTenant).Model(&platformFlag{}).Where("key = ?", "feature.x").Update("enabled", false).Error; err != nil {
				t.Fatalf("Update() error = %v", err)
			}

			if err := db.WithContext(noTenant).Where("key = ?", "feature.x").Delete(&platformFlag{}).Error; err != nil {
				t.Fatalf("Delete() error = %v", err)
			}

			var n int64
			if err := db.Model(&platformFlag{}).Count(&n).Error; err != nil {
				t.Fatalf("Count() error = %v", err)
			}
			if n != 0 {
				t.Errorf("Count() after delete = %d, want 0", n)
			}
		})
	}
}

// TestTenantScopeBeforeQuery_ConcurrentDifferentTenants_EachSeesOwnTenant is
// the single most important test in this file. It runs many goroutines
// concurrently against one shared *gorm.DB (one shared tenantScopePlugin
// instance), each goroutine using its own tenant in its own context and
// repeatedly querying, and asserts every goroutine only ever observes its
// own tenant's rows.
//
// Run with -race: a plugin implemented with any shared or global state
// (for example, a field on tenantScopePlugin set by one callback invocation
// and read by another) instead of reading the tenant fresh from
// db.Statement.Context on every call would either be flagged directly by
// the race detector, or — worse — pass the race detector but leak one
// tenant's rows into another's results under concurrent load, which the
// functional assertions below would catch.
func TestTenantScopeBeforeQuery_ConcurrentDifferentTenants_EachSeesOwnTenant(t *testing.T) {
	db := newScopedTestDB(t)

	const (
		numTenants        = 8
		rowsPerTenant     = 5
		readsPerGoroutine = 25
	)

	tenants := make([]pkgcore.TenantID, numTenants)
	for i := range tenants {
		tid := pkgcore.TenantID(fmt.Sprintf("concurrent-tenant-%d", i))
		tenants[i] = tid
		for r := 0; r < rowsPerTenant; r++ {
			mustCreateWidget(t, db, tid, &testutil.Widget{
				ID:       fmt.Sprintf("w-%d-%d", i, r),
				TenantID: string(tid),
				Name:     "concurrent-widget",
				Value:    i, // encodes which tenant this row should belong to
			})
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, numTenants*readsPerGoroutine*2)

	for i, tid := range tenants {
		wg.Add(1)
		go func(i int, tid pkgcore.TenantID) {
			defer wg.Done()
			ctx := ctxFor(tid)
			for r := 0; r < readsPerGoroutine; r++ {
				var widgets []testutil.Widget
				if err := db.WithContext(ctx).Where("name = ?", "concurrent-widget").Find(&widgets).Error; err != nil {
					errs <- fmt.Errorf("tenant %q: Find() error = %w", tid, err)
					continue
				}
				if len(widgets) != rowsPerTenant {
					errs <- fmt.Errorf("tenant %q: Find() returned %d widgets, want %d", tid, len(widgets), rowsPerTenant)
				}
				for _, w := range widgets {
					if w.TenantID != string(tid) {
						errs <- fmt.Errorf("tenant %q: Find() leaked a row belonging to tenant %q", tid, w.TenantID)
					}
					if w.Value != i {
						errs <- fmt.Errorf("tenant %q: Find() returned a row with Value %d, want %d (this tenant's own seed marker)", tid, w.Value, i)
					}
				}

				var n int64
				if err := db.WithContext(ctx).Model(&testutil.Widget{}).Where("name = ?", "concurrent-widget").Count(&n).Error; err != nil {
					errs <- fmt.Errorf("tenant %q: Count() error = %w", tid, err)
					continue
				}
				if n != rowsPerTenant {
					errs <- fmt.Errorf("tenant %q: Count() = %d, want %d", tid, n, rowsPerTenant)
				}
			}
		}(i, tid)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestTenantScopeBeforeCreate_ConcurrentDifferentTenants_EachRowLandsUnderItsOwnTenant
// is the Create counterpart of the concurrency test above: many goroutines,
// each with a different tenant in context, insert rows concurrently against
// one shared *gorm.DB, and every row must land under the tenant its own
// goroutine used — never under whichever tenant happened to be "current" in
// some other goroutine, which is exactly the failure shape shared/global
// state in the plugin would produce under concurrent creates.
func TestTenantScopeBeforeCreate_ConcurrentDifferentTenants_EachRowLandsUnderItsOwnTenant(t *testing.T) {
	db := newScopedTestDB(t)

	const (
		numTenants    = 8
		rowsPerTenant = 10
	)

	var wg sync.WaitGroup
	errs := make(chan error, numTenants*rowsPerTenant)

	for i := 0; i < numTenants; i++ {
		tid := pkgcore.TenantID(fmt.Sprintf("concurrent-create-tenant-%d", i))
		wg.Add(1)
		go func(i int, tid pkgcore.TenantID) {
			defer wg.Done()
			ctx := ctxFor(tid)
			for r := 0; r < rowsPerTenant; r++ {
				w := testutil.Widget{
					ID:       fmt.Sprintf("cc-%d-%d", i, r),
					TenantID: string(tid),
					Name:     "concurrent-create",
				}
				if err := db.WithContext(ctx).Create(&w).Error; err != nil {
					errs <- fmt.Errorf("tenant %q: Create() error = %w", tid, err)
				}
			}
		}(i, tid)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	for i := 0; i < numTenants; i++ {
		want := fmt.Sprintf("concurrent-create-tenant-%d", i)
		for r := 0; r < rowsPerTenant; r++ {
			id := fmt.Sprintf("cc-%d-%d", i, r)
			if got := rawTenantID(t, db, id); got != want {
				t.Errorf("widget %q landed under tenant %q, want %q", id, got, want)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Detection-mechanism tests: isTenantScopedValue, isTenantScopedStatement and
// asTenantIDString — the reflection-based plumbing the callbacks above rely
// on to decide whether a statement is even in tenant scope.
// ---------------------------------------------------------------------------

// TestIsTenantScopedValue exercises isTenantScopedValue against every shape
// GORM is documented to hand a callback as Statement.Model or
// Statement.Dest: a pointer to a struct, a pointer to a slice of structs,
// and a slice of pointers to structs, plus the pointer-to-slice-of-pointers
// and bare-slice variants, nil and typed-nil edge cases, and the
// non-tenant-scoped fixture (platformFlag, defined above in the
// core-contract section) in the same shapes to prove the negative side of
// the contract too.
func TestIsTenantScopedValue(t *testing.T) {
	widget := testutil.Widget{ID: "w1", TenantID: "tenant-a"}
	widgets := []testutil.Widget{widget}
	widgetPtrs := []*testutil.Widget{&widget}

	tests := []struct {
		name string
		v    interface{}
		want bool
	}{
		{name: "nil", v: nil, want: false},
		{name: "struct value", v: widget, want: true},
		{name: "pointer to struct", v: &widget, want: true},
		{name: "pointer to slice of structs", v: &widgets, want: true},
		{name: "slice of pointers to structs", v: widgetPtrs, want: true},
		{name: "pointer to slice of pointers to structs", v: &widgetPtrs, want: true},
		{name: "bare slice of structs with no outer pointer", v: widgets, want: true},
		{name: "empty slice of structs", v: []testutil.Widget{}, want: true},
		{name: "typed nil pointer to struct", v: (*testutil.Widget)(nil), want: true},
		{name: "typed nil slice of structs", v: ([]testutil.Widget)(nil), want: true},
		{name: "non-scoped struct value", v: platformFlag{Key: "x"}, want: false},
		{name: "non-scoped pointer to struct", v: &platformFlag{Key: "x"}, want: false},
		{name: "non-scoped pointer to slice of structs", v: &[]platformFlag{{Key: "x"}}, want: false},
		{name: "non-scoped slice of pointers to structs", v: []*platformFlag{{Key: "x"}}, want: false},
		{name: "map payload (an Updates call with no Model set)", v: map[string]interface{}{"tenant_id": "tenant-a"}, want: false},
		{name: "pointer to an unrelated type", v: new(int), want: false},
		{name: "unrelated slice type", v: []string{"a", "b"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTenantScopedValue(tt.v); got != tt.want {
				t.Errorf("isTenantScopedValue(%#v) = %v, want %v", tt.v, got, tt.want)
			}
		})
	}
}

// TestIsTenantScopedStatement_PrefersModelOverDest pins down a detail that
// matters specifically because of how GORM's own Count finisher works: it
// seeds Statement.Model from Statement.Dest when Model is unset, but then
// overwrites Dest with the *int64 result pointer before the query
// callbacks run. For a call shaped like db.Model(&Widget{}).Count(&n), that
// means Model still holds *Widget while Dest holds *int64 by the time
// isTenantScopedStatement is asked — so it must check Model first and only
// fall back to Dest when Model is nil. Checking Dest first, or only Dest,
// would silently stop detecting Count on a tenant-scoped model.
func TestIsTenantScopedStatement_PrefersModelOverDest(t *testing.T) {
	tests := []struct {
		name  string
		model interface{}
		dest  interface{}
		want  bool
	}{
		{
			name:  "Model tenant-scoped, Dest is *int64 (the Model(...).Count(...) shape)",
			model: &testutil.Widget{},
			dest:  new(int64),
			want:  true,
		},
		{
			name:  "Model nil, Dest tenant-scoped (the Find(...) shape)",
			model: nil,
			dest:  &[]testutil.Widget{},
			want:  true,
		},
		{
			name:  "Model not tenant-scoped, Dest not tenant-scoped",
			model: &platformFlag{},
			dest:  new(int64),
			want:  false,
		},
		{
			name:  "both nil",
			model: nil,
			dest:  nil,
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt := &gorm.Statement{Model: tt.model, Dest: tt.dest}
			if got := isTenantScopedStatement(stmt); got != tt.want {
				t.Errorf("isTenantScopedStatement(Model=%#v, Dest=%#v) = %v, want %v", tt.model, tt.dest, got, tt.want)
			}
		})
	}
}

// TestAsTenantIDString covers the value shapes an Update/Updates map payload
// may hold under the tenant_id key: a plain string (TenantModel's declared
// field type), pkgcore.TenantID (what a caller most often holds a tenant id
// as, having just read it from context), and an unrecognized type, which
// must resolve to the empty string so that a genuine (never-empty)
// context tenant can never accidentally match it.
func TestAsTenantIDString(t *testing.T) {
	tests := []struct {
		name string
		v    interface{}
		want string
	}{
		{name: "string", v: "tenant-a", want: "tenant-a"},
		{name: "pkgcore.TenantID", v: pkgcore.TenantID("tenant-a"), want: "tenant-a"},
		{name: "empty string", v: "", want: ""},
		{name: "nil", v: nil, want: ""},
		{name: "int is unrecognized", v: 42, want: ""},
		{name: "bool is unrecognized", v: true, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := asTenantIDString(tt.v); got != tt.want {
				t.Errorf("asTenantIDString(%#v) = %q, want %q", tt.v, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Or()-composition regression tests.
// ---------------------------------------------------------------------------

// This section covers a gap none of the core-contract tests above exercise:
// what tenantScopeBeforeQuery/Update/Delete actually produce when the
// CALLER's own query already carries an Or(...) branch, rather than the
// plain, single-condition (or no-condition) shapes every other test in this
// package builds.
//
// The short version, confirmed empirically below: tenantScopePlugin appends
// its tenant filter as a brand new, separate `.Where(tenant_id = ?)` clause,
// merged onto the end of whatever WHERE expressions the caller already
// built (see clause.Where.MergeClause in gorm.io/gorm/clause/where.go). SQL
// gives AND strictly higher precedence than OR, and gorm's own
// clause-building only auto-parenthesizes a *raw string* condition whose
// SQL text itself visibly contains "AND "/"OR " (clause.buildExprs' Expr
// case) — it does NOT parenthesize a structured chain built via the
// separate .Or(...) builder method before merging in a later, unrelated
// .Where(...) call. So:
//
//	db.Where("name = ?", "x").Or("name = ?", "y")     // caller code
//	// ... tenantScopeBeforeQuery later appends:
//	db.Statement.Where("tenant_id = ?", tid)
//
// renders as `name = ? OR name = ? AND tenant_id = ?`, which — by normal
// SQL operator precedence — parses as `name = ? OR (name = ? AND tenant_id
// = ?)`, not the intended `(name = ? OR name = ?) AND tenant_id = ?`. The
// first branch of the OR carries no tenant filter at all, so it matches
// that row under ANY tenant, not just the caller's.
//
// This is exactly the class of caller shape the coding standard's own
// carve-out anticipates ("a reporting query genuinely needs raw SQL... pass
// the tenant explicitly" — backend coding standard section 3.2) and that
// dbkit.Repository[T]'s minimal API (no exposed filter/Or parameter) does
// not itself construct — so it is not reachable through Repository today —
// but it is very much reachable by anything holding a *gorm.DB from
// dbkit.Open and building a multi-condition query directly against a
// TenantScoped model, which is exactly what the plugin is documented to
// exist as a defense-in-depth backstop for. A defense-in-depth layer that
// silently stops defending the moment a caller's query shape includes an
// Or() is a real gap, not a hypothetical one.
func TestTenantScopeBeforeQuery_CallerOrCondition_TenantFilterAppliesToEveryBranch(t *testing.T) {
	db := newScopedTestDB(t)
	mustCreateWidget(t, db, tenantA, &testutil.Widget{ID: "a-x", TenantID: string(tenantA), Name: "x", Value: 1})
	mustCreateWidget(t, db, tenantA, &testutil.Widget{ID: "a-y", TenantID: string(tenantA), Name: "y", Value: 1})
	// Same "x" name as tenant A's row above, owned by a different tenant.
	mustCreateWidget(t, db, tenantB, &testutil.Widget{ID: "b-x", TenantID: string(tenantB), Name: "x", Value: 1})

	var got []testutil.Widget
	err := db.WithContext(ctxFor(tenantA)).
		Where("name = ?", "x").
		Or("name = ?", "y").
		Find(&got).Error
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}

	for _, w := range got {
		if w.TenantID != string(tenantA) {
			t.Errorf("tenant A's Where(name=x).Or(name=y) query returned tenant %q's row %+v; "+
				"the appended tenant_id filter must bind to every OR branch, not just the last one "+
				"(SQL operator precedence makes 'a OR b AND tenant_id=?' mean 'a OR (b AND tenant_id=?)', "+
				"leaving the first branch completely unfiltered by tenant)", w.TenantID, w)
		}
	}
	if len(got) != 2 {
		t.Errorf("tenant A's Where(name=x).Or(name=y) query returned %d rows %+v, want exactly 2 (its own x and y widgets, and nothing from tenant B)",
			len(got), got)
	}
}

// TestTenantScopeBeforeQuery_CallerRawOrExpression_TenantFilterAppliesToEveryBranch
// is the contrasting, currently-safe shape: the same logical OR, expressed
// as a single raw SQL string instead of the chained .Or(...) builder. gorm
// heuristically parenthesizes a raw condition string whenever it visibly
// contains " AND "/" OR " and more than one WHERE expression is present
// (clause.buildExprs' Expr case), so this shape composes correctly with the
// plugin's appended clause even though the .Or(...) shape above does not.
// It is kept here specifically so a future fix to the case above has a
// passing witness of the shape it must not regress.
func TestTenantScopeBeforeQuery_CallerRawOrExpression_TenantFilterAppliesToEveryBranch(t *testing.T) {
	db := newScopedTestDB(t)
	mustCreateWidget(t, db, tenantA, &testutil.Widget{ID: "a-x", TenantID: string(tenantA), Name: "x", Value: 1})
	mustCreateWidget(t, db, tenantA, &testutil.Widget{ID: "a-y", TenantID: string(tenantA), Name: "y", Value: 1})
	mustCreateWidget(t, db, tenantB, &testutil.Widget{ID: "b-x", TenantID: string(tenantB), Name: "x", Value: 1})

	var got []testutil.Widget
	err := db.WithContext(ctxFor(tenantA)).
		Where("name = ? OR name = ?", "x", "y").
		Find(&got).Error
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}

	for _, w := range got {
		if w.TenantID != string(tenantA) {
			t.Errorf("tenant A's raw 'name = ? OR name = ?' query returned tenant %q's row %+v, want only its own rows", w.TenantID, w)
		}
	}
	if len(got) != 2 {
		t.Errorf("tenant A's raw 'name = ? OR name = ?' query returned %d rows %+v, want exactly 2", len(got), got)
	}
}

// TestTenantScopeBeforeUpdate_CallerOrCondition_TenantFilterAppliesToEveryBranch
// is the write-side counterpart of the read-side leak above, and the more
// severe of the two: the same unparenthesized-OR shape lets tenant A issue
// a bulk Updates() call that also mutates a row it does not own, not merely
// read it.
func TestTenantScopeBeforeUpdate_CallerOrCondition_TenantFilterAppliesToEveryBranch(t *testing.T) {
	db := newScopedTestDB(t)
	mustCreateWidget(t, db, tenantA, &testutil.Widget{ID: "a-x", TenantID: string(tenantA), Name: "x", Value: 1})
	mustCreateWidget(t, db, tenantB, &testutil.Widget{ID: "b-x", TenantID: string(tenantB), Name: "x", Value: 1})

	res := db.WithContext(ctxFor(tenantA)).
		Model(&testutil.Widget{}).
		Where("name = ?", "x").
		Or("value = ?", 999). // never matches anything; present only to force the Or(...) shape
		Updates(map[string]interface{}{"value": 42})
	if res.Error != nil {
		t.Fatalf("Updates() error = %v", res.Error)
	}

	if got := rawValue(t, db, "b-x"); got != 1 {
		t.Errorf("tenant B's row after tenant A's Where(name=x).Or(value=999) Updates() = value %d, want unchanged 1 "+
			"(tenant A's bulk update must never modify a row it does not own, regardless of the shape of its WHERE clause)", got)
	}
	if got := rawValue(t, db, "a-x"); got != 42 {
		t.Errorf("tenant A's own row after its own Updates() = value %d, want 42 (the update must still apply to the caller's own matching row)", got)
	}
	if res.RowsAffected != 1 {
		t.Errorf("Updates() RowsAffected = %d, want exactly 1 (only tenant A's own row)", res.RowsAffected)
	}
}

// TestTenantScopeBeforeDelete_CallerOrCondition_TenantFilterAppliesToEveryBranch
// is the delete-side counterpart of the two leaks above, and the most severe
// of the three: the same unparenthesized-OR shape lets tenant A issue a bulk
// Delete() call that permanently destroys a row it does not own.
func TestTenantScopeBeforeDelete_CallerOrCondition_TenantFilterAppliesToEveryBranch(t *testing.T) {
	db := newScopedTestDB(t)
	mustCreateWidget(t, db, tenantA, &testutil.Widget{ID: "a-x", TenantID: string(tenantA), Name: "x", Value: 1})
	mustCreateWidget(t, db, tenantB, &testutil.Widget{ID: "b-x", TenantID: string(tenantB), Name: "x", Value: 1})

	res := db.WithContext(ctxFor(tenantA)).
		Where("name = ?", "x").
		Or("value = ?", 999). // never matches anything; present only to force the Or(...) shape
		Delete(&testutil.Widget{})
	if res.Error != nil {
		t.Fatalf("Delete() error = %v", res.Error)
	}

	if got := rawCount(t, db, "b-x"); got != 1 {
		t.Errorf("tenant B's row after tenant A's Where(name=x).Or(value=999) Delete() = %d rows remaining, want 1 (unchanged) "+
			"(tenant A's bulk delete must never remove a row it does not own, regardless of the shape of its WHERE clause)", got)
	}
	if got := rawCount(t, db, "a-x"); got != 0 {
		t.Errorf("tenant A's own row after its own Delete() = %d rows remaining, want 0 (the delete must still apply to the caller's own matching row)", got)
	}
	if res.RowsAffected != 1 {
		t.Errorf("Delete() RowsAffected = %d, want exactly 1 (only tenant A's own row)", res.RowsAffected)
	}
}

// ---------------------------------------------------------------------------
// Transaction tests: multi-statement, multi-tenant, nested-savepoint and
// bulk-write scenarios executed inside db.Transaction.
// ---------------------------------------------------------------------------

// note is a second, independent tenant-scoped fixture model, distinct from
// testutil.Widget. It exists solely so the tests in this file can prove
// that a single database transaction touching two DIFFERENT tenant-scoped
// model types keeps each type's tenant filtering correct and independent —
// nothing in tenant_scope.go keys any state off "the model last seen in
// this transaction", but that is exactly the kind of regression a shared
// fixture could hide, so a second, unrelated model type is a real fixture,
// not an oversight of reusing Widget for everything.
type note struct {
	ID       string `gorm:"column:id;primaryKey;size:26"`
	TenantID string `gorm:"column:tenant_id;primaryKey;size:26;not null"`
	Body     string `gorm:"column:body;size:255;not null"`
}

// GetTenantID satisfies TenantScoped.
func (n note) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(n.TenantID) }

// TableName pins note's table name explicitly, matching the raw CREATE
// TABLE in createNotesTable independent of GORM's pluralization rules.
func (note) TableName() string { return "notes" }

var _ TenantScoped = note{}

// createNotesTable adds note's table to db via a plain Exec, mirroring
// createPlatformFlagsTable's approach for the same "no AutoMigrate even in
// a throwaway test fixture" reason.
func createNotesTable(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.Exec(`CREATE TABLE notes (id VARCHAR(26) NOT NULL, tenant_id VARCHAR(26) NOT NULL, body VARCHAR(255) NOT NULL, PRIMARY KEY (tenant_id, id))`).Error; err != nil {
		t.Fatalf("create notes table: %v", err)
	}
}

// TestTenantScopePlugin_Transaction_MultipleTenantsInOneTransaction_EachWriteLandsUnderItsOwnTenant
// covers a shape none of the core-contract tests above exercise: a
// single database transaction (db.Transaction) in which statements for TWO
// DIFFERENT tenants are issued back to back, via tx.WithContext(...) calls
// for each tenant inside the same callback. This is a real risk area: if
// tenant resolution were ever cached on the transaction's *gorm.DB session
// instead of re-read from each statement's own context, the second
// tenant's write could silently be forced onto the first tenant.
func TestTenantScopePlugin_Transaction_MultipleTenantsInOneTransaction_EachWriteLandsUnderItsOwnTenant(t *testing.T) {
	db := newScopedTestDB(t)

	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.WithContext(ctxFor(tenantA)).Create(&testutil.Widget{ID: "tx-a-1", Name: "a-in-tx"}).Error; err != nil {
			return err
		}
		if err := tx.WithContext(ctxFor(tenantB)).Create(&testutil.Widget{ID: "tx-b-1", Name: "b-in-tx"}).Error; err != nil {
			return err
		}
		// A second statement under the same tenant A used earlier in this
		// transaction, to confirm switching to B and back doesn't leave
		// any residue on A's own subsequent statement either.
		if err := tx.WithContext(ctxFor(tenantA)).Create(&testutil.Widget{ID: "tx-a-2", Name: "a-in-tx-2"}).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Transaction() error = %v", err)
	}

	if got := rawTenantID(t, db, "tx-a-1"); got != string(tenantA) {
		t.Errorf("tx-a-1 tenant_id = %q, want %q", got, tenantA)
	}
	if got := rawTenantID(t, db, "tx-a-2"); got != string(tenantA) {
		t.Errorf("tx-a-2 tenant_id = %q, want %q", got, tenantA)
	}
	if got := rawTenantID(t, db, "tx-b-1"); got != string(tenantB) {
		t.Errorf("tx-b-1 tenant_id = %q, want %q (must not have been forced onto tenant A by a preceding statement in the same transaction)", got, tenantB)
	}
}

// TestTenantScopePlugin_Transaction_SpansTwoTenantScopedModelTypes_EachScopedIndependently
// covers the "transaction spanning multiple tenant-scoped models" shape
// explicitly: one transaction writes both a Widget and a note, for two
// different tenants, and every combination must come out scoped correctly.
func TestTenantScopePlugin_Transaction_SpansTwoTenantScopedModelTypes_EachScopedIndependently(t *testing.T) {
	db := newScopedTestDB(t)
	createNotesTable(t, db)

	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.WithContext(ctxFor(tenantA)).Create(&testutil.Widget{ID: "w-a", Name: "widget-a"}).Error; err != nil {
			return err
		}
		if err := tx.WithContext(ctxFor(tenantA)).Create(&note{ID: "n-a", Body: "note-a"}).Error; err != nil {
			return err
		}
		if err := tx.WithContext(ctxFor(tenantB)).Create(&testutil.Widget{ID: "w-b", Name: "widget-b"}).Error; err != nil {
			return err
		}
		if err := tx.WithContext(ctxFor(tenantB)).Create(&note{ID: "n-b", Body: "note-b"}).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Transaction() error = %v", err)
	}

	var widgetsA []testutil.Widget
	if err := db.WithContext(ctxFor(tenantA)).Find(&widgetsA).Error; err != nil {
		t.Fatalf("Find(widgets, tenant A) error = %v", err)
	}
	if want := []string{"w-a"}; !idSliceEqual(idsOf(widgetsA), want) {
		t.Errorf("tenant A widgets = %v, want %v", idsOf(widgetsA), want)
	}

	var notesA []note
	if err := db.WithContext(ctxFor(tenantA)).Find(&notesA).Error; err != nil {
		t.Fatalf("Find(notes, tenant A) error = %v", err)
	}
	if len(notesA) != 1 || notesA[0].ID != "n-a" {
		t.Errorf("tenant A notes = %+v, want exactly [n-a]", notesA)
	}

	var widgetsB []testutil.Widget
	if err := db.WithContext(ctxFor(tenantB)).Find(&widgetsB).Error; err != nil {
		t.Fatalf("Find(widgets, tenant B) error = %v", err)
	}
	if want := []string{"w-b"}; !idSliceEqual(idsOf(widgetsB), want) {
		t.Errorf("tenant B widgets = %v, want %v", idsOf(widgetsB), want)
	}

	var notesB []note
	if err := db.WithContext(ctxFor(tenantB)).Find(&notesB).Error; err != nil {
		t.Fatalf("Find(notes, tenant B) error = %v", err)
	}
	if len(notesB) != 1 || notesB[0].ID != "n-b" {
		t.Errorf("tenant B notes = %+v, want exactly [n-b]", notesB)
	}
}

// idSliceEqual is a tiny, local, order-independent-after-sort comparison
// helper (idsOf already sorts its input) so this file does not need to
// import "slices" just for one comparison already-sorted inputs make
// trivial.
func idSliceEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestTenantScopePlugin_Transaction_ErrorFromCrossTenantUpdate_RollsBackEntireTransaction
// proves that when code inside a transaction checks .Error the ordinary way
// and returns it, tenantScopeBeforeUpdate's fail path (an update whose
// WHERE-scoped RowsAffected is zero because it targets another tenant's
// row) correctly drives a full rollback — including undoing an earlier,
// perfectly valid write made earlier in the same transaction. This is the
// "transaction atomicity actually composes with tenant checks" property:
// a plugin that only ever affects SELECT/INSERT/UPDATE/DELETE statements
// individually is not useful if a caller cannot rely on the surrounding
// transaction still rolling back correctly when one of those statements
// signals a tenant violation.
func TestTenantScopePlugin_Transaction_ErrorFromCrossTenantUpdate_RollsBackEntireTransaction(t *testing.T) {
	db := newScopedTestDB(t)

	txErr := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.WithContext(ctxFor(tenantA)).Create(&testutil.Widget{ID: "w-1", Name: "original", Value: 1}).Error; err != nil {
			return err
		}
		// Cross-tenant: tenant B attempts to update tenant A's row by id,
		// inside the same transaction as the valid create above.
		res := tx.WithContext(ctxFor(tenantB)).Model(&testutil.Widget{}).Where("id = ?", "w-1").Update("value", 999)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errors.New("cross-tenant update matched no rows (expected — this is what must abort the transaction)")
		}
		return nil
	})
	if txErr == nil {
		t.Fatal("Transaction() error = nil, want an error (the cross-tenant update must not silently succeed with zero effect)")
	}

	// The whole transaction — including the earlier, valid tenant-A create
	// — must have been rolled back, not partially applied.
	if n := rawCount(t, db, "w-1"); n != 0 {
		t.Errorf("row count for w-1 after rolled-back transaction = %d, want 0 (the valid create earlier in the same transaction must also be undone)", n)
	}
}

// TestTenantScopePlugin_NestedTransaction_SavepointRollback_OnlyRevertsInnerWrites
// covers nested transactions (GORM's db.Transaction called again from
// inside another db.Transaction's callback, which — on a connection already
// inside a transaction — uses SAVEPOINT/ROLLBACK TO SAVEPOINT instead of a
// real BEGIN/ROLLBACK). None of the existing suite exercises this at all.
//
// The scenario: an outer transaction writes a tenant-A widget, then runs a
// nested transaction that writes a tenant-B widget and then deliberately
// fails, and finally (after the nested transaction has rolled back) writes
// a second tenant-A widget and commits. A correct implementation leaves:
//   - the first tenant-A widget present (written before the nested failure)
//   - the tenant-B widget ABSENT (undone by the savepoint rollback)
//   - the second tenant-A widget present (the outer transaction must not be
//     poisoned by the inner savepoint's failure and must still be able to
//     commit its own later work)
func TestTenantScopePlugin_NestedTransaction_SavepointRollback_OnlyRevertsInnerWrites(t *testing.T) {
	db := newScopedTestDB(t)

	outerErr := db.Transaction(func(outerTx *gorm.DB) error {
		if err := outerTx.WithContext(ctxFor(tenantA)).Create(&testutil.Widget{ID: "outer-1", Name: "before-nested"}).Error; err != nil {
			return err
		}

		innerErr := outerTx.Transaction(func(innerTx *gorm.DB) error {
			if err := innerTx.WithContext(ctxFor(tenantB)).Create(&testutil.Widget{ID: "inner-b", Name: "should-not-survive"}).Error; err != nil {
				return err
			}
			// Force the nested transaction to fail after its write, so
			// GORM issues ROLLBACK TO SAVEPOINT for exactly this inner
			// transaction's work.
			return errors.New("deliberate failure to force an inner savepoint rollback")
		})
		if innerErr == nil {
			t.Fatal("nested Transaction() error = nil, want the deliberate error to propagate out")
		}

		// The outer transaction continues after the inner one rolled back,
		// and performs another valid write under tenant A.
		if err := outerTx.WithContext(ctxFor(tenantA)).Create(&testutil.Widget{ID: "outer-2", Name: "after-nested"}).Error; err != nil {
			return err
		}
		return nil
	})
	if outerErr != nil {
		t.Fatalf("outer Transaction() error = %v, want nil (the inner savepoint's rollback must not poison the outer transaction)", outerErr)
	}

	if n := rawCount(t, db, "outer-1"); n != 1 {
		t.Errorf("row count for outer-1 = %d, want 1 (written before the nested transaction, must survive)", n)
	}
	if n := rawCount(t, db, "outer-2"); n != 1 {
		t.Errorf("row count for outer-2 = %d, want 1 (written after the nested transaction's rollback, the outer transaction must still be usable)", n)
	}
	if n := rawCount(t, db, "inner-b"); n != 0 {
		t.Errorf("row count for inner-b = %d, want 0 (the nested transaction's own write must have been undone by its savepoint rollback)", n)
	}
}

// TestTenantScopePlugin_BulkUpdate_MultipleMatchingRows_OnlyAffectsCallingTenantRows
// covers a genuinely bulk write: several rows for tenant A and one row for
// tenant B all share the same non-key column value, and a single Updates()
// call with no primary key in its WHERE clause at all must update every one
// of tenant A's matching rows and none of tenant B's — proving the tenant
// filter composes correctly across however many rows a single statement
// touches, not just the single-row case every other write test in this
// package covers.
func TestTenantScopePlugin_BulkUpdate_MultipleMatchingRows_OnlyAffectsCallingTenantRows(t *testing.T) {
	db := newScopedTestDB(t)
	for _, id := range []string{"a-1", "a-2", "a-3"} {
		mustCreateWidget(t, db, tenantA, &testutil.Widget{ID: id, TenantID: string(tenantA), Name: "stale", Value: 1})
	}
	mustCreateWidget(t, db, tenantB, &testutil.Widget{ID: "b-1", TenantID: string(tenantB), Name: "stale", Value: 1})

	res := db.WithContext(ctxFor(tenantA)).Model(&testutil.Widget{}).Where("name = ?", "stale").Update("value", 42)
	if res.Error != nil {
		t.Fatalf("bulk Update() error = %v", res.Error)
	}
	if res.RowsAffected != 3 {
		t.Errorf("bulk Update() RowsAffected = %d, want 3 (every tenant-A row matching the filter, and only those)", res.RowsAffected)
	}

	for _, id := range []string{"a-1", "a-2", "a-3"} {
		if got := rawValue(t, db, id); got != 42 {
			t.Errorf("tenant A widget %q value after bulk update = %d, want 42", id, got)
		}
	}
	if got := rawValue(t, db, "b-1"); got != 1 {
		t.Errorf("tenant B widget value after tenant A's bulk update = %d, want unchanged 1", got)
	}
}

// TestTenantScopePlugin_BulkDelete_MultipleMatchingRows_OnlyDeletesCallingTenantRows
// is the bulk-write counterpart for Delete: a single Delete() call whose
// WHERE clause matches several of tenant A's rows by a shared, non-key
// column, alongside a same-valued tenant B row that must survive.
func TestTenantScopePlugin_BulkDelete_MultipleMatchingRows_OnlyDeletesCallingTenantRows(t *testing.T) {
	db := newScopedTestDB(t)
	for _, id := range []string{"a-1", "a-2", "a-3"} {
		mustCreateWidget(t, db, tenantA, &testutil.Widget{ID: id, TenantID: string(tenantA), Name: "obsolete", Value: 1})
	}
	mustCreateWidget(t, db, tenantB, &testutil.Widget{ID: "b-1", TenantID: string(tenantB), Name: "obsolete", Value: 1})

	res := db.WithContext(ctxFor(tenantA)).Where("name = ?", "obsolete").Delete(&testutil.Widget{})
	if res.Error != nil {
		t.Fatalf("bulk Delete() error = %v", res.Error)
	}
	if res.RowsAffected != 3 {
		t.Errorf("bulk Delete() RowsAffected = %d, want 3 (every tenant-A row matching the filter, and only those)", res.RowsAffected)
	}

	for _, id := range []string{"a-1", "a-2", "a-3"} {
		if n := rawCount(t, db, id); n != 0 {
			t.Errorf("tenant A widget %q row count after bulk delete = %d, want 0", id, n)
		}
	}
	if n := rawCount(t, db, "b-1"); n != 1 {
		t.Errorf("tenant B widget row count after tenant A's bulk delete = %d, want unchanged 1 (survived)", n)
	}
}
