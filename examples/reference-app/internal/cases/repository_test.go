package cases

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/testkit"
	"github.com/vislake/speed/go/tenancy/tenancytest"

	"github.com/vislake/speed/examples/reference-app/internal/cases/migrations"

	// Blank-imported for its init side effect: registers dbkit.DialectSQLite
	// with dbkit's dialect registry so the dbkit.Open calls in the
	// reopen-durability test below can build a gorm.Dialector from a plain
	// file path. dbtest.NewSQLite does this import for its own calls, but
	// that helper cannot reopen a file, which is exactly what that test
	// needs -- so this file carries the driver import itself, the same
	// convention dbtest's own sqlite.go documents for test-only code.
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
)

// newRepository returns a Repository backed by a fresh, per-test SQLite
// database whose two tables and indexes were created by the domain's real
// migration set -- the same files the assembly's Verify stage applies at
// boot, never a hand-written schema shortcut -- so a test failure here can
// never be explained away as "the test fixture's schema diverged from the
// real DDL".
func newRepository(t *testing.T) *Repository {
	t.Helper()
	db := dbtest.NewSQLite(t)
	applyMigrations(t, db)
	return NewRepository(db)
}

// applyMigrations applies the domain's migration set to db through the real
// dbkit.MigrationRegistry (dbtest.Migrate) -- the same machinery the
// assembly runs, so the schema a test database carries is the one a booted
// process carries.
func applyMigrations(t *testing.T, db *gorm.DB) {
	t.Helper()
	dbtest.Migrate(t, db, dbkit.DialectSQLite, dbtest.Migration{Module: "cases", FS: migrations.FS})
}

// tenantCtx returns a context carrying tenant and a dummy actor, the shape
// every write and read below runs under.
func tenantCtx(tenant pkgcore.TenantID) context.Context {
	return pkgcore.WithActor(testkit.TenantCtx(tenant), pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "cases-test-user"})
}

// TestRepository_Case_AssertIsolated runs the mandatory tenant-isolation
// suite (the multi-tenant isolation discipline) against
// the cases repository's real dbkit.Repository[caseRecord] usage. caseRecord
// is tenant data, not identity or platform data, so AssertIsolated is the
// correct half of the AssertIsolated/AssertNotTenantScoped pair, never
// both.
func TestRepository_Case_AssertIsolated(t *testing.T) {
	repo := newRepository(t)

	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *caseRecord {
		return &caseRecord{
			ID:            uuid.NewString(),
			TenantModel:   dbkit.TenantModel{TenantID: string(tenant)},
			PatientName:   "isolation test patient",
			CreatorUserID: "cases-test-user",
		}
	})
}

// TestRepository_CasePhoto_AssertIsolated runs the same mandatory
// tenant-isolation suite against the case_photos repository (repo.photos --
// the child table's own dbkit.Repository[casePhotoRecord]). Every factory
// call gets a distinct row id AND a distinct object id: the
// uq_case_photos_tenant_object index forbids two rows of ONE tenant
// referencing the same object, so a factory reusing one object id would
// trip the suite's own multi-row-per-tenant creates for the wrong reason
// (the isolation property under test is tenant scoping, not attachment
// uniqueness -- that is service_test.go's TestService_Create_PhotoAlreadyAttached's
// job).
func TestRepository_CasePhoto_AssertIsolated(t *testing.T) {
	repo := newRepository(t)

	tenancytest.AssertIsolated(t, repo.photos, func(tenant pkgcore.TenantID) *casePhotoRecord {
		return &casePhotoRecord{
			ID:          uuid.NewString(),
			TenantModel: dbkit.TenantModel{TenantID: string(tenant)},
			CaseID:      "case-under-isolation",
			ObjectID:    uuid.NewString(),
			Position:    0,
		}
	})
}

// TestRepository_ReappliedMigrationsLeaveTheSchemaUsable pins the ledger
// contract a process restart runs over an existing database file: applying
// the domain's migration set again must succeed as a pure ledger skip (the
// files are already recorded for module "cases") and leave a usable
// repository behind.
func TestRepository_ReappliedMigrationsLeaveTheSchemaUsable(t *testing.T) {
	repo := newRepository(t)
	applyMigrations(t, repo.db)

	ctx := tenantCtx("tenant-a")
	if err := repo.Create(ctx, &caseRecord{ID: uuid.NewString(), PatientName: "still works"}); err != nil {
		t.Fatalf("Create() after the second migration apply error = %v", err)
	}
}

// TestRepository_CaseRows_SurviveReopen proves the record store's restart
// durability with a real restart: rows written through the repository,
// then the database connection closed and the same file reopened with a
// fresh dbkit.Open (exactly what a process restart does), must still be
// there -- case row and photo rows alike -- after the reopened connection's
// migration set has been re-applied over the file (a ledger skip).
func TestRepository_CaseRows_SurviveReopen(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "cases-durability.sqlite")

	open := func() *Repository {
		t.Helper()
		db, err := dbkit.Open(context.Background(), dbkit.Options{
			Dialect: dbkit.DialectSQLite,
			DSN:     dsn,
		})
		if err != nil {
			t.Fatalf("dbkit.Open(%q): %v", dsn, err)
		}
		t.Cleanup(func() {
			sqlDB, dbErr := db.DB()
			if dbErr != nil {
				return
			}
			_ = sqlDB.Close()
		})
		applyMigrations(t, db)
		return NewRepository(db)
	}

	first := open()
	ctx := tenantCtx("tenant-a")
	record := &caseRecord{ID: uuid.NewString(), PatientName: "durable patient", CreatorUserID: "user-1"}
	if err := first.createCaseWithPhotos(ctx, record, []casePhotoRecord{
		{ID: uuid.NewString(), CaseID: record.ID, ObjectID: "photo-1", Position: 0},
		{ID: uuid.NewString(), CaseID: record.ID, ObjectID: "photo-2", Position: 1},
	}); err != nil {
		t.Fatalf("createCaseWithPhotos() error = %v", err)
	}

	second := open()
	got, err := second.FindByID(ctx, record.ID)
	if err != nil {
		t.Fatalf("FindByID() after reopen error = %v", err)
	}
	if got.PatientName != record.PatientName || got.CreatorUserID != "user-1" {
		t.Fatalf("FindByID() after reopen = %+v, want the pre-restart row intact", got)
	}
	photos, err := second.listPhotosOf(ctx, record.ID)
	if err != nil {
		t.Fatalf("listPhotosOf() after reopen error = %v", err)
	}
	if len(photos) != 2 || photos[0].ObjectID != "photo-1" || photos[1].ObjectID != "photo-2" {
		t.Fatalf("listPhotosOf() after reopen = %+v, want both photo rows in order", photos)
	}
}

// TestRepository_ListByTenant_ScopesAndOrders pins the clinic-wide list
// query on its two axes: it returns EVERY case of the ctx tenant --
// whatever creator each row carries, so a colleague's case is as visible
// as one's own, the clinic-wide property the product's acceptance chain
// demands -- newest first, and a case of another tenant stays invisible.
func TestRepository_ListByTenant_ScopesAndOrders(t *testing.T) {
	repo := newRepository(t)
	ctxA := tenantCtx("tenant-a")
	ctxB := tenantCtx("tenant-b")

	now := time.Now()
	// CreatedAt is set explicitly (gorm's autoCreateTime only fills a zero
	// field), so the newest-first order below is asserted deterministically
	// rather than depending on wall-clock separation between two inserts.
	first := &caseRecord{ID: uuid.NewString(), PatientName: "first", CreatorUserID: "user-1", CreatedAt: now.Add(-time.Hour)}
	second := &caseRecord{ID: uuid.NewString(), PatientName: "second", CreatorUserID: "user-1", CreatedAt: now}
	otherCreator := &caseRecord{ID: uuid.NewString(), PatientName: "other creator", CreatorUserID: "user-2", CreatedAt: now.Add(-2 * time.Hour)}
	otherTenant := &caseRecord{ID: uuid.NewString(), PatientName: "other tenant", CreatorUserID: "user-1", CreatedAt: now}
	for _, record := range []*caseRecord{first, second, otherCreator} {
		if err := repo.Create(ctxA, record); err != nil {
			t.Fatalf("Create(%q) error = %v", record.PatientName, err)
		}
	}
	if err := repo.Create(ctxB, otherTenant); err != nil {
		t.Fatalf("Create(other tenant) error = %v", err)
	}

	rows, err := repo.listByTenant(ctxA)
	if err != nil {
		t.Fatalf("listByTenant() error = %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("listByTenant(tenant-a) = %d rows, want 3 (the other creator's row must be as visible as user-1's own)", len(rows))
	}
	if rows[0].PatientName != "second" || rows[1].PatientName != "first" || rows[2].PatientName != "other creator" {
		t.Fatalf("listByTenant(tenant-a) order = [%q %q %q], want [second first other creator] (newest first)",
			rows[0].PatientName, rows[1].PatientName, rows[2].PatientName)
	}

	rows, err = repo.listByTenant(ctxB)
	if err != nil {
		t.Fatalf("listByTenant(tenant-b) error = %v", err)
	}
	if len(rows) != 1 || rows[0].PatientName != "other tenant" {
		t.Fatalf("listByTenant(tenant-b) = %+v, want only tenant-b's own row", rows)
	}
}

// TestRepository_ListPhotosOf_OrdersByPositionAndScopesByTenant pins the
// detail read's photo ordering (attachment position, ascending -- even
// when rows were inserted out of order) and its tenant scoping (listing
// another tenant's case id answers an empty list, never an error and never
// that tenant's rows).
func TestRepository_ListPhotosOf_OrdersByPositionAndScopesByTenant(t *testing.T) {
	repo := newRepository(t)
	ctxA := tenantCtx("tenant-a")
	ctxB := tenantCtx("tenant-b")

	caseID := uuid.NewString()
	if err := repo.Create(ctxA, &caseRecord{ID: caseID, PatientName: "photo case"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	// Deliberately inserted out of order: position, not insertion order,
	// must win.
	photoRows := []casePhotoRecord{
		{ID: uuid.NewString(), CaseID: caseID, ObjectID: "photo-pos-2", Position: 2},
		{ID: uuid.NewString(), CaseID: caseID, ObjectID: "photo-pos-0", Position: 0},
		{ID: uuid.NewString(), CaseID: caseID, ObjectID: "photo-pos-1", Position: 1},
	}
	for i := range photoRows {
		if err := repo.photos.Create(ctxA, &photoRows[i]); err != nil {
			t.Fatalf("photos.Create(%q) error = %v", photoRows[i].ObjectID, err)
		}
	}

	rows, err := repo.listPhotosOf(ctxA, caseID)
	if err != nil {
		t.Fatalf("listPhotosOf() error = %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("listPhotosOf() = %d rows, want 3", len(rows))
	}
	for i, want := range []string{"photo-pos-0", "photo-pos-1", "photo-pos-2"} {
		if rows[i].ObjectID != want {
			t.Fatalf("listPhotosOf() order = [%s %s %s], want [photo-pos-0 photo-pos-1 photo-pos-2]",
				rows[0].ObjectID, rows[1].ObjectID, rows[2].ObjectID)
		}
	}

	rows, err = repo.listPhotosOf(ctxB, caseID)
	if err != nil {
		t.Fatalf("listPhotosOf(tenant-b) error = %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("listPhotosOf(tenant-b) = %d rows, want 0 (another tenant's case must list nothing)", len(rows))
	}
}
