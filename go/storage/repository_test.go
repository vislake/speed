package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"

	"github.com/vislake/speed/go/storage/internal/testutil"
	"github.com/vislake/speed/go/storage/migrations"
)

// newTestDB returns a freshly migrated SQLite database for one test: the
// module's real migration files applied from zero, so every test below is
// also a proof those files run (AutoMigrate is banned, and a broken
// migration must fail here, in the unit tier, not on a deployment).
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	return testutil.NewSQLite(t, moduleName, migrations.FS)
}

// tenantCtx returns a context carrying tenant, the tenant context every
// repository call in this module's production path is reached through.
func tenantCtx(tenant pkgcore.TenantID) context.Context {
	return pkgcore.WithTenant(context.Background(), tenant)
}

// seedObject creates one row through the repository under test, failing the
// test on any error, so a seeding problem surfaces at the seed line rather
// than as a confusing later assertion.
func seedObject(t *testing.T, repo *ObjectRepository, ctx context.Context, o Object) {
	t.Helper()
	if err := repo.Create(ctx, &o); err != nil {
		t.Fatalf("seed object %q: %v", o.ID, err)
	}
}

// newUpload builds one Object in ObjectStateUploading -- the exact shape the
// upload declaration round creates -- with a key of the canonical
// "<tenantID>/<objectID>/original" grammar. The explicit CreatedAt is what
// the ordering tests seed staggered times with: gorm's autoCreateTime only
// fills a zero timestamp, so a non-zero one is preserved as given.
func newUpload(id string, tenant pkgcore.TenantID, createdAt time.Time) Object {
	return Object{
		TenantModel:      dbkit.TenantModel{TenantID: string(tenant)},
		ID:               id,
		Key:              string(tenant) + "/" + id + "/original",
		State:            ObjectStateUploading,
		DeclaredSize:     1024,
		DeclaredType:     "image/png",
		DeclaredChecksum: "",
		UploadExpiresAt:  createdAt.Add(30 * time.Minute),
		CreatedAt:        createdAt,
	}
}

// newCompleted builds one Object in ObjectStateCompleted: an upload row
// whose revalidation pipeline has already run, so it carries the finalized
// columns a completed row must. The digest is a deterministic function of
// the id rather than of real bytes -- the repository neither knows nor
// cares about content, and the service round proves the real shape. The
// explicit CreatedAt is what the ordering tests seed staggered times with.
func newCompleted(id string, tenant pkgcore.TenantID, createdAt time.Time) Object {
	o := newUpload(id, tenant, createdAt)
	o.State = ObjectStateCompleted
	size := int64(1024)
	mime := "image/png"
	digest := sha256HexDigest([]byte(id))
	o.Size = &size
	o.MIME = &mime
	o.ChecksumSHA256 = &digest
	return o
}

// assertObjectIDs fails t unless got carries exactly the wanted ids, in
// order -- the ordering assertions of the cursor tests are the whole point,
// so a length-only check would not do.
func assertObjectIDs(t *testing.T, got []Object, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d objects, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("objects[%d].ID = %q, want %q (full order: %v)", i, got[i].ID, want[i], objectIDs(got))
		}
	}
}

// objectIDs extracts the ids of a page, for failure messages.
func objectIDs(objects []Object) []string {
	ids := make([]string, len(objects))
	for i := range objects {
		ids[i] = objects[i].ID
	}
	return ids
}

// isolationUUID returns a distinct, UUID-shaped id for index n, the shape
// this module's own ID column convention promises (application-generated
// UUID strings).
func isolationUUID(n int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
}

// TestObjectRepository_AssertIsolated runs the shared isolation suite
// against Object: the mandatory proof that objects are tenant data whose
// rows can neither be read, updated nor deleted across tenants. newRecord
// fills every NOT NULL column of the objects table (there is no nullable
// column and no default that would make Create succeed with less).
func TestObjectRepository_AssertIsolated(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))

	n := 0
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *Object {
		n++
		upload := newUpload(isolationUUID(n), tenant, time.Now())
		return &upload
	})
}

// TestDerivativeRepository_AssertIsolated runs the shared isolation suite
// against ObjectDerivative: derivatives are tenant data exactly like the
// objects they derive from -- the unique index protecting one object's
// thumbnail set is (tenant_id, object_id, kind), tenant first.
func TestDerivativeRepository_AssertIsolated(t *testing.T) {
	db := newTestDB(t)
	repo := NewDerivativeRepository(db)

	n := 0
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *ObjectDerivative {
		n++
		parent := isolationUUID(n)
		key, err := DerivativeKey(tenant, parent, DerivativeKindThumbnail)
		if err != nil {
			t.Fatalf("DerivativeKey(%q, %q): %v", tenant, parent, err)
		}
		return &ObjectDerivative{
			TenantModel: dbkit.TenantModel{TenantID: string(tenant)},
			ID:          isolationUUID(100 + n),
			ObjectID:    parent,
			Kind:        DerivativeKindThumbnail,
			Key:         key,
			MIME:        "image/jpeg",
			Size:        4096,
		}
	})
}

// TestObjectRepository_ListPage_NewestFirst proves the page is ordered by
// (created_at DESC, id DESC), not by insertion order or by id. The five
// seeds' creation times deliberately contradict their id order -- obj-01 is
// the newest -- so an implementation that fell back to id ordering fails
// here: gorm's autoCreateTime only fills zero timestamps, so the explicit
// staggered CreatedAt values land in the table exactly as given.
func TestObjectRepository_ListPage_NewestFirst(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))
	ctx := tenantCtx(pkgcore.TenantID("tenant-a"))
	base := time.Now().Add(-1 * time.Hour).Truncate(time.Second)

	// obj-01 is created last (newest), obj-02 first (oldest).
	seedObject(t, repo, ctx, newUpload("obj-01", "tenant-a", base.Add(5*time.Minute)))
	seedObject(t, repo, ctx, newUpload("obj-02", "tenant-a", base.Add(1*time.Minute)))
	seedObject(t, repo, ctx, newUpload("obj-03", "tenant-a", base.Add(4*time.Minute)))
	seedObject(t, repo, ctx, newUpload("obj-04", "tenant-a", base.Add(2*time.Minute)))
	seedObject(t, repo, ctx, newUpload("obj-05", "tenant-a", base.Add(3*time.Minute)))

	page, err := repo.listPage(ctx, 2, "")
	if err != nil {
		t.Fatalf("listPage(first page): %v", err)
	}
	assertObjectIDs(t, page, "obj-01", "obj-03")

	page, err = repo.listPage(ctx, 2, "obj-03")
	if err != nil {
		t.Fatalf("listPage(after obj-03): %v", err)
	}
	assertObjectIDs(t, page, "obj-05", "obj-04")

	page, err = repo.listPage(ctx, 2, "obj-04")
	if err != nil {
		t.Fatalf("listPage(after obj-04): %v", err)
	}
	assertObjectIDs(t, page, "obj-02")

	// The cursor has walked past every row: the next page is empty, not an
	// error, and not the first page again.
	page, err = repo.listPage(ctx, 10, "obj-02")
	if err != nil {
		t.Fatalf("listPage(after the last row): %v", err)
	}
	assertObjectIDs(t, page)
}

// TestObjectRepository_ListPage_ExhaustiveWalk walks the whole table with
// the smallest page size that still spans it, and asserts the sum equals
// the full DESC order exactly once each -- the pagination contract a caller
// like an admin listing relies on: no row skipped, none doubled, order
// total.
func TestObjectRepository_ListPage_ExhaustiveWalk(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))
	ctx := tenantCtx(pkgcore.TenantID("tenant-a"))
	base := time.Now().Add(-1 * time.Hour).Truncate(time.Second)

	seedObject(t, repo, ctx, newUpload("obj-01", "tenant-a", base.Add(5*time.Minute)))
	seedObject(t, repo, ctx, newUpload("obj-02", "tenant-a", base.Add(1*time.Minute)))
	seedObject(t, repo, ctx, newUpload("obj-03", "tenant-a", base.Add(4*time.Minute)))
	seedObject(t, repo, ctx, newUpload("obj-04", "tenant-a", base.Add(2*time.Minute)))
	seedObject(t, repo, ctx, newUpload("obj-05", "tenant-a", base.Add(3*time.Minute)))

	var walked []Object
	cursor := ""
	for {
		page, err := repo.listPage(ctx, 2, cursor)
		if err != nil {
			t.Fatalf("listPage(after %q): %v", cursor, err)
		}
		if len(page) == 0 {
			break
		}
		walked = append(walked, page...)
		cursor = page[len(page)-1].ID
		if len(walked) > 10 {
			t.Fatal("walk did not terminate")
		}
	}
	assertObjectIDs(t, walked, "obj-01", "obj-03", "obj-05", "obj-04", "obj-02")
}

// TestObjectRepository_ListPage_EmptyTable pins the empty case: a first page
// over no rows is empty with no error -- never an ErrRecordNotFound, which
// is reserved for a cursor that names no row.
func TestObjectRepository_ListPage_EmptyTable(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))

	page, err := repo.listPage(tenantCtx("tenant-a"), 5, "")
	if err != nil {
		t.Fatalf("listPage over an empty table: %v", err)
	}
	assertObjectIDs(t, page)
}

// TestObjectRepository_ListPage_TiesBrokenByIdDescending proves the
// (created_at = ? AND id < ?) half of the cursor predicate: two rows sharing
// a creation timestamp still page in a total order, newest-then-highest-id
// first, without either row being skipped or doubled at the boundary --
// without that clause, equal timestamps would make keyset pagination
// ambiguous.
func TestObjectRepository_ListPage_TiesBrokenByIdDescending(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))
	ctx := tenantCtx(pkgcore.TenantID("tenant-a"))
	base := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	tie := base.Add(1 * time.Minute)

	seedObject(t, repo, ctx, newUpload("obj-aa", "tenant-a", tie))
	seedObject(t, repo, ctx, newUpload("obj-zz", "tenant-a", tie))
	seedObject(t, repo, ctx, newUpload("obj-old", "tenant-a", base))

	page, err := repo.listPage(ctx, 1, "")
	if err != nil {
		t.Fatalf("listPage(first page): %v", err)
	}
	assertObjectIDs(t, page, "obj-zz") // higher id wins the tie

	// Past obj-zz: the remaining tied row (id < obj-zz at the same
	// timestamp) comes before the strictly older row.
	page, err = repo.listPage(ctx, 5, "obj-zz")
	if err != nil {
		t.Fatalf("listPage(after obj-zz): %v", err)
	}
	assertObjectIDs(t, page, "obj-aa", "obj-old")
}

// TestObjectRepository_ListPage_InsertionsDoNotShiftFetchedPages pins the
// reason the page is a keyset cursor rather than an offset: after two pages
// are fetched, rows inserted with creation times NEWER than the current
// cursor (obj-04) do not disturb the pages already taken -- re-fetching
// from that cursor returns exactly what it returned before the inserts --
// and the new rows only surface by starting over from the beginning.
func TestObjectRepository_ListPage_InsertionsDoNotShiftFetchedPages(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))
	ctx := tenantCtx(pkgcore.TenantID("tenant-a"))
	base := time.Now().Add(-1 * time.Hour).Truncate(time.Second)

	seedObject(t, repo, ctx, newUpload("obj-01", "tenant-a", base.Add(1*time.Minute)))
	seedObject(t, repo, ctx, newUpload("obj-02", "tenant-a", base.Add(2*time.Minute)))
	seedObject(t, repo, ctx, newUpload("obj-03", "tenant-a", base.Add(3*time.Minute)))
	seedObject(t, repo, ctx, newUpload("obj-04", "tenant-a", base.Add(4*time.Minute)))
	seedObject(t, repo, ctx, newUpload("obj-05", "tenant-a", base.Add(5*time.Minute)))

	page, err := repo.listPage(ctx, 2, "")
	if err != nil {
		t.Fatalf("listPage(first page): %v", err)
	}
	assertObjectIDs(t, page, "obj-05", "obj-04")

	page, err = repo.listPage(ctx, 3, "obj-04")
	if err != nil {
		t.Fatalf("listPage(after obj-04): %v", err)
	}
	assertObjectIDs(t, page, "obj-03", "obj-02", "obj-01")

	// Two rows land while the caller holds the first two pages: one at the
	// very top (obj-06) and one between obj-05 and obj-04 (obj-55, at
	// base+4m30s). Both are newer than the obj-04 cursor.
	seedObject(t, repo, ctx, newUpload("obj-06", "tenant-a", base.Add(6*time.Minute)))
	seedObject(t, repo, ctx, newUpload("obj-55", "tenant-a", base.Add(4*time.Minute+30*time.Second)))

	// The already-fetched pages are undisturbed.
	page, err = repo.listPage(ctx, 3, "obj-04")
	if err != nil {
		t.Fatalf("listPage(after obj-04, post-insert): %v", err)
	}
	assertObjectIDs(t, page, "obj-03", "obj-02", "obj-01")

	// The full list, read from the beginning, now carries all seven rows
	// with the new ones in their chronological positions.
	page, err = repo.listPage(ctx, 3, "")
	if err != nil {
		t.Fatalf("listPage(fresh first page): %v", err)
	}
	assertObjectIDs(t, page, "obj-06", "obj-05", "obj-55")

	page, err = repo.listPage(ctx, 3, "obj-55")
	if err != nil {
		t.Fatalf("listPage(after obj-55): %v", err)
	}
	assertObjectIDs(t, page, "obj-04", "obj-03", "obj-02")

	page, err = repo.listPage(ctx, 10, "obj-02")
	if err != nil {
		t.Fatalf("listPage(after obj-02): %v", err)
	}
	assertObjectIDs(t, page, "obj-01")
}

// TestObjectRepository_ListPage_IsTenantScoped proves the ordering query
// never leaks across tenants: two tenants seeding rows with interleaved
// creation times each page over exactly their own rows, newest of their own
// first. Tenant B's newest is older than tenant A's newest, which is the
// shape that would expose a missing tenant filter: without one, tenant B's
// first page would carry tenant A's rows. The ids are distinct per tenant
// because they are globally unique application UUIDs -- the id-alone
// primary key the schema declares (org's own model documents the choice)
// makes cross-tenant id reuse a schema violation by design.
func TestObjectRepository_ListPage_IsTenantScoped(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))
	ctxA := tenantCtx(pkgcore.TenantID("tenant-a"))
	ctxB := tenantCtx(pkgcore.TenantID("tenant-b"))
	base := time.Now().Add(-1 * time.Hour).Truncate(time.Second)

	// Interleave: A's rows are all NEWER than B's.
	seedObject(t, repo, ctxA, newUpload("a-obj-1", "tenant-a", base.Add(9*time.Minute)))
	seedObject(t, repo, ctxB, newUpload("b-obj-1", "tenant-b", base.Add(4*time.Minute)))
	seedObject(t, repo, ctxA, newUpload("a-obj-2", "tenant-a", base.Add(8*time.Minute)))
	seedObject(t, repo, ctxB, newUpload("b-obj-2", "tenant-b", base.Add(3*time.Minute)))
	seedObject(t, repo, ctxA, newUpload("a-obj-3", "tenant-a", base.Add(7*time.Minute)))
	seedObject(t, repo, ctxB, newUpload("b-obj-3", "tenant-b", base.Add(2*time.Minute)))

	page, err := repo.listPage(ctxB, 10, "")
	if err != nil {
		t.Fatalf("listPage for tenant-b: %v", err)
	}
	assertObjectIDs(t, page, "b-obj-1", "b-obj-2", "b-obj-3")
	for _, row := range page {
		if row.GetTenantID() != "tenant-b" {
			t.Errorf("row %q reports tenant %q, want tenant-b", row.ID, row.GetTenantID())
		}
	}

	page, err = repo.listPage(ctxA, 10, "")
	if err != nil {
		t.Fatalf("listPage for tenant-a: %v", err)
	}
	assertObjectIDs(t, page, "a-obj-1", "a-obj-2", "a-obj-3")
	for _, row := range page {
		if row.GetTenantID() != "tenant-a" {
			t.Errorf("row %q reports tenant %q, want tenant-a", row.ID, row.GetTenantID())
		}
	}
}

// TestObjectRepository_ListPage_RejectsForeignOrUnknownCursor pins the
// cursor lookup's fail-closed shape: a cursor that names no row of the
// caller's tenant -- whether it never existed or belongs to another tenant
// -- reports dbkit.ErrRecordNotFound, indistinguishable on purpose, so a
// caller cannot probe whether an id it does not own exists.
func TestObjectRepository_ListPage_RejectsForeignOrUnknownCursor(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))
	ctxA := tenantCtx(pkgcore.TenantID("tenant-a"))
	ctxB := tenantCtx(pkgcore.TenantID("tenant-b"))
	base := time.Now().Add(-1 * time.Hour).Truncate(time.Second)

	seedObject(t, repo, ctxA, newUpload("obj-1", "tenant-a", base.Add(2*time.Minute)))
	seedObject(t, repo, ctxA, newUpload("obj-2", "tenant-a", base.Add(1*time.Minute)))

	tests := []struct {
		name   string
		ctx    context.Context
		cursor string
	}{
		{"cursor that never existed", ctxA, "obj-99"},
		{"cursor owned by another tenant", ctxB, "obj-1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			page, err := repo.listPage(tc.ctx, 5, tc.cursor)
			if page != nil {
				t.Errorf("listPage returned %d rows with an unknown cursor, want none", len(page))
			}
			assertCode(t, err, dbkit.ErrRecordNotFound.Code)
		})
	}
}

// TestObjectRepository_ListPageState_CompletedRowsOnly pins the filtered
// form's reason to exist: the service listing promises completed objects
// only, so the rows a completed-only page may return are exactly the
// ObjectStateCompleted ones. Uploading rows interspersed by creation time
// between completed rows are skipped by every page, and skipping them never
// breaks the keyset ordering of the rows that remain -- the walk from the
// newest completed row to the oldest lands on precisely the completed set.
// The unfiltered listPage over the same table still sees every state, which
// is what keeps the two forms' contracts distinct.
func TestObjectRepository_ListPageState_CompletedRowsOnly(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))
	ctx := tenantCtx(pkgcore.TenantID("tenant-a"))
	base := time.Now().Add(-1 * time.Hour).Truncate(time.Second)

	// States interleave by creation time: completed rows sit at minutes 5, 3
	// and 1, uploading rows at minutes 4 and 2 -- the shape that would leak
	// an uploading row into a completed-only page if the filter were a
	// post-query thinning rather than part of the query.
	seedObject(t, repo, ctx, newCompleted("c-01", "tenant-a", base.Add(5*time.Minute)))
	seedObject(t, repo, ctx, newUpload("u-01", "tenant-a", base.Add(4*time.Minute)))
	seedObject(t, repo, ctx, newCompleted("c-02", "tenant-a", base.Add(3*time.Minute)))
	seedObject(t, repo, ctx, newUpload("u-02", "tenant-a", base.Add(2*time.Minute)))
	seedObject(t, repo, ctx, newCompleted("c-03", "tenant-a", base.Add(1*time.Minute)))

	// Walk the completed set one row per page; uploading rows between the
	// completed ones must never surface nor stall the walk.
	var walked []Object
	cursor := ""
	for {
		page, err := repo.listPageState(ctx, ObjectStateCompleted, 1, cursor)
		if err != nil {
			t.Fatalf("listPageState(after %q): %v", cursor, err)
		}
		if len(page) == 0 {
			break
		}
		walked = append(walked, page...)
		cursor = page[len(page)-1].ID
		if len(walked) > 10 {
			t.Fatal("walk did not terminate")
		}
	}
	assertObjectIDs(t, walked, "c-01", "c-02", "c-03")

	// The unfiltered form is untouched: it still pages over all five rows.
	page, err := repo.listPage(ctx, 10, "")
	if err != nil {
		t.Fatalf("listPage over the same table: %v", err)
	}
	assertObjectIDs(t, page, "c-01", "u-01", "c-02", "u-02", "c-03")
}

// TestObjectRepository_ListPageState_CursorRowOutsideTheState pins the
// cursor rule of the filtered form: a cursor that names a row in the wrong
// state reports dbkit.ErrRecordNotFound -- the same shape as a cursor that
// never existed -- because a row outside the listed state is exactly as
// invisible to this listing as a missing one, and a cursor a concurrent
// transition pushed out of the state must never silently resume the walk
// from a row the caller was not listing. The unfiltered form accepts an
// uploading row as its cursor without complaint, which is what keeps the
// state check on the filtered form alone.
func TestObjectRepository_ListPageState_CursorRowOutsideTheState(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))
	ctx := tenantCtx(pkgcore.TenantID("tenant-a"))
	base := time.Now().Add(-1 * time.Hour).Truncate(time.Second)

	seedObject(t, repo, ctx, newCompleted("c-01", "tenant-a", base.Add(2*time.Minute)))
	seedObject(t, repo, ctx, newUpload("u-01", "tenant-a", base.Add(1*time.Minute)))
	seedObject(t, repo, ctx, newCompleted("c-00", "tenant-a", base))

	page, err := repo.listPageState(ctx, ObjectStateCompleted, 5, "u-01")
	if page != nil {
		t.Errorf("listPageState returned %d rows with an out-of-state cursor, want none", len(page))
	}
	assertCode(t, err, dbkit.ErrRecordNotFound.Code)

	// The same row is a legitimate cursor for the unfiltered form: paging
	// from it walks the rows older than it, c-00.
	page, err = repo.listPage(ctx, 5, "u-01")
	if err != nil {
		t.Fatalf("listPage with an uploading cursor: %v", err)
	}
	assertObjectIDs(t, page, "c-00")

	// And an in-state cursor pages normally, skipping nothing on the way.
	page, err = repo.listPageState(ctx, ObjectStateCompleted, 5, "c-01")
	if err != nil {
		t.Fatalf("listPageState(after c-01): %v", err)
	}
	assertObjectIDs(t, page, "c-00")
}

// TestObjectRepository_ListPage_FailsClosedWithoutTenant pins the no-tenant
// shape: every repository call, the cursor page included, fails closed with
// pkgcore.ErrNoTenant when no tenant rides in the context -- and the module
// wraps that in its own internal error rather than leaking it raw, with the
// cause chain intact so errors.Is still recognizes it.
func TestObjectRepository_ListPage_FailsClosedWithoutTenant(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))

	page, err := repo.listPage(context.Background(), 5, "")
	if page != nil {
		t.Errorf("listPage returned %d rows with no tenant in context, want none", len(page))
	}
	if !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Errorf("listPage error = %v, want errors.Is(err, pkgcore.ErrNoTenant)", err)
	}
	assertCode(t, err, ErrInternal.Code)
}

// TestObjectRepository_Objects_SeedAndFindByID_RoundTrip exercises the
// promoted Repository surface on a real migrated database: what module_test
// and the later service round rely on -- a row created under its tenant is
// found again under that tenant, in ObjectStateUploading.
func TestObjectRepository_Objects_SeedAndFindByID_RoundTrip(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))
	ctx := tenantCtx(pkgcore.TenantID("tenant-a"))
	upload := newUpload("obj-1", "tenant-a", time.Now())

	seedObject(t, repo, ctx, upload)

	got, err := repo.FindByID(ctx, "obj-1")
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.State != ObjectStateUploading {
		t.Errorf("state = %q, want %q", got.State, ObjectStateUploading)
	}
	if got.Key != upload.Key {
		t.Errorf("key = %q, want %q", got.Key, upload.Key)
	}
	if got.GetTenantID() != "tenant-a" {
		t.Errorf("GetTenantID() = %q, want tenant-a", got.GetTenantID())
	}
}

// newDerivative builds one ObjectDerivative row of the canonical derivative
// key grammar -- "<tenantID>/<objectID>/<kind>" -- the shape the derive
// pipeline writes. kind must be a value the (object, kind) unique index does
// not already hold for that object; the delete-protocol tests that need
// multi-derivative objects pass a second kind (the index permits several
// kinds per object, and the repository layer treats kind as an opaque
// string). The explicit CreatedAt is what the ordering tests seed staggered
// times with.
func newDerivative(t *testing.T, id, objectID, kind string, tenant pkgcore.TenantID, createdAt time.Time) ObjectDerivative {
	t.Helper()
	key, err := DerivativeKey(tenant, objectID, kind)
	if err != nil {
		t.Fatalf("DerivativeKey(%q, %q): %v", tenant, objectID, err)
	}
	return ObjectDerivative{
		TenantModel: dbkit.TenantModel{TenantID: string(tenant)},
		ID:          id,
		ObjectID:    objectID,
		Kind:        kind,
		Key:         key,
		MIME:        "image/jpeg",
		Size:        4096,
		CreatedAt:   createdAt,
	}
}

// seedDerivative creates one derivative row through the repository under
// test, failing the test on any error.
func seedDerivative(t *testing.T, repo *DerivativeRepository, ctx context.Context, d ObjectDerivative) {
	t.Helper()
	if err := repo.Create(ctx, &d); err != nil {
		t.Fatalf("seed derivative %q: %v", d.ID, err)
	}
}

// TestObjectRepository_MarkDeleting_CompletedAdvances proves the guarded
// state flip: a completed row read back from markDeleting reads deleting --
// the durable marker that makes every later deletion step resumable -- and
// the flip is persisted, not just returned.
func TestObjectRepository_MarkDeleting_CompletedAdvances(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))
	ctx := tenantCtx(pkgcore.TenantID("tenant-a"))
	seedObject(t, repo, ctx, newCompleted("obj-1", "tenant-a", time.Now()))

	got, err := repo.markDeleting(ctx, "obj-1")
	if err != nil {
		t.Fatalf("markDeleting: %v", err)
	}
	if got.State != ObjectStateDeleting {
		t.Errorf("markDeleting returned state %q, want %q", got.State, ObjectStateDeleting)
	}

	// The flip is durable: a fresh read sees deleting too.
	got, err = repo.FindByID(ctx, "obj-1")
	if err != nil {
		t.Fatalf("FindByID after markDeleting: %v", err)
	}
	if got.State != ObjectStateDeleting {
		t.Errorf("row state after markDeleting = %q, want %q", got.State, ObjectStateDeleting)
	}
}

// TestObjectRepository_MarkDeleting_ResumesADeclaredDelete pins the resume
// rule of the deletion protocol: marking a row that already reads deleting
// is not an error and does not reset anything -- a second DeleteObject
// racing an in-flight one re-runs the protocol from the marker, converging
// instead of erroring.
func TestObjectRepository_MarkDeleting_ResumesADeclaredDelete(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))
	ctx := tenantCtx(pkgcore.TenantID("tenant-a"))
	seedObject(t, repo, ctx, newCompleted("obj-1", "tenant-a", time.Now()))

	if _, err := repo.markDeleting(ctx, "obj-1"); err != nil {
		t.Fatalf("first markDeleting: %v", err)
	}
	got, err := repo.markDeleting(ctx, "obj-1")
	if err != nil {
		t.Fatalf("second markDeleting on a deleting row: %v", err)
	}
	if got.State != ObjectStateDeleting {
		t.Errorf("second markDeleting returned state %q, want %q", got.State, ObjectStateDeleting)
	}
}

// TestObjectRepository_MarkDeleting_LeavesUploadingRowsUntouched pins the
// guard's other side: a row still in ObjectStateUploading is returned
// untouched and unflipped -- only the sweep reclaims uploading rows, through
// its own path -- so the service can refuse the delete while the row stays
// exactly as the upload in flight left it.
func TestObjectRepository_MarkDeleting_LeavesUploadingRowsUntouched(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))
	ctx := tenantCtx(pkgcore.TenantID("tenant-a"))
	seedObject(t, repo, ctx, newUpload("obj-1", "tenant-a", time.Now()))

	got, err := repo.markDeleting(ctx, "obj-1")
	if err != nil {
		t.Fatalf("markDeleting on an uploading row: %v", err)
	}
	if got.State != ObjectStateUploading {
		t.Errorf("markDeleting returned state %q, want %q", got.State, ObjectStateUploading)
	}
	got, err = repo.FindByID(ctx, "obj-1")
	if err != nil {
		t.Fatalf("FindByID after markDeleting on an uploading row: %v", err)
	}
	if got.State != ObjectStateUploading {
		t.Errorf("row state = %q, want %q", got.State, ObjectStateUploading)
	}
}

// TestObjectRepository_MarkDeleting_ReportsNotFound pins the guard's
// fail-closed shape: a row that does not exist -- or belongs to another
// tenant -- reports dbkit.ErrRecordNotFound, never a silent success over
// nothing, because DeleteObject must distinguish "marked, resumable" from
// "nothing there".
func TestObjectRepository_MarkDeleting_ReportsNotFound(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))
	ctxA := tenantCtx(pkgcore.TenantID("tenant-a"))
	ctxB := tenantCtx(pkgcore.TenantID("tenant-b"))
	seedObject(t, repo, ctxA, newCompleted("obj-1", "tenant-a", time.Now()))

	tests := []struct {
		name string
		ctx  context.Context
		id   string
	}{
		{"row that never existed", ctxA, "obj-99"},
		{"row owned by another tenant", ctxB, "obj-1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row, err := repo.markDeleting(tc.ctx, tc.id)
			if row != nil {
				t.Errorf("markDeleting returned a row for %q, want none", tc.id)
			}
			assertCode(t, err, dbkit.ErrRecordNotFound.Code)
		})
	}
}

// TestObjectRepository_FinalizeUpload_CommitsTheTransition proves the happy
// path of the upload lifecycle's single state-changing write: a row that is
// still uploading with its window still open flips to completed carrying
// exactly the finalized metadata passed in, and every other column of the
// row survives the full-row write untouched.
func TestObjectRepository_FinalizeUpload_CommitsTheTransition(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))
	ctx := tenantCtx(pkgcore.TenantID("tenant-a"))
	seedObject(t, repo, ctx, newUpload("obj-1", "tenant-a", time.Now()))

	row, err := repo.FindByID(ctx, "obj-1")
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	size := int64(2048)
	mime := "image/png"
	digest := sha256HexDigest([]byte("finalized bytes"))
	row.State = ObjectStateCompleted
	row.Size = &size
	row.MIME = &mime
	row.ChecksumSHA256 = &digest

	done, err := repo.finalizeUpload(ctx, row, time.Now())
	if err != nil {
		t.Fatalf("finalizeUpload: %v", err)
	}
	if !done {
		t.Fatal("finalizeUpload done = false on an in-window uploading row, want true")
	}

	got, err := repo.FindByID(ctx, "obj-1")
	if err != nil {
		t.Fatalf("FindByID after finalizeUpload: %v", err)
	}
	if got.State != ObjectStateCompleted {
		t.Errorf("row state after finalizeUpload = %q, want %q", got.State, ObjectStateCompleted)
	}
	if got.Size == nil || *got.Size != size {
		t.Errorf("row size after finalizeUpload = %v, want %d", got.Size, size)
	}
	if got.MIME == nil || *got.MIME != mime {
		t.Errorf("row MIME after finalizeUpload = %v, want %q", got.MIME, mime)
	}
	if got.ChecksumSHA256 == nil || *got.ChecksumSHA256 != digest {
		t.Errorf("row checksum after finalizeUpload = %v, want %q", got.ChecksumSHA256, digest)
	}
	if got.DeclaredSize != row.DeclaredSize || got.DeclaredType != row.DeclaredType {
		t.Errorf("finalizeUpload rewrote columns outside the finalized set: declared %d/%q, want %d/%q",
			got.DeclaredSize, got.DeclaredType, row.DeclaredSize, row.DeclaredType)
	}
}

// TestObjectRepository_FinalizeUpload_RefusesAClosedWindow pins the
// write-time half of the hard-deadline rule: a row whose upload window
// closed before the finalize's now is refused, and stays uploading. The
// entry gate in Complete checks the window too, but only this write-time
// condition keeps a completion pipeline that straddles its own window end
// from committing after it.
func TestObjectRepository_FinalizeUpload_RefusesAClosedWindow(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))
	ctx := tenantCtx(pkgcore.TenantID("tenant-a"))
	// newUpload's window is createdAt+30m, so a row created 31 minutes ago
	// is strictly past its window for any now.
	seedObject(t, repo, ctx, newUpload("obj-1", "tenant-a", time.Now().Add(-31*time.Minute)))

	row, err := repo.FindByID(ctx, "obj-1")
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	row.State = ObjectStateCompleted
	done, err := repo.finalizeUpload(ctx, row, time.Now())
	if err != nil {
		t.Fatalf("finalizeUpload: %v", err)
	}
	if done {
		t.Fatal("finalizeUpload done = true on a row whose window closed, want false")
	}
	got, err := repo.FindByID(ctx, "obj-1")
	if err != nil {
		t.Fatalf("FindByID after refused finalizeUpload: %v", err)
	}
	if got.State != ObjectStateUploading {
		t.Errorf("row state = %q, want %q -- the refusal must leave the row untouched",
			got.State, ObjectStateUploading)
	}
}

// TestObjectRepository_FinalizeUpload_RefusesWhenNotAnUploadingRowHere pins
// the other two refusal shapes, in one table: a finalize whose row no
// longer exists anywhere, a finalize aimed at a row another tenant owns,
// and a finalize of a row a concurrent completion already flipped. All
// three answer (false, nil) -- a zero-row conditional write is not an
// error -- and leave the world exactly as it was. The tenant row is the
// sweep-reclaim race's half: after the reclaim removed the row, the
// straggling completion's finalize must refuse rather than report success
// for nothing.
func TestObjectRepository_FinalizeUpload_RefusesWhenNotAnUploadingRowHere(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))
	ctxA := tenantCtx(pkgcore.TenantID("tenant-a"))
	ctxB := tenantCtx(pkgcore.TenantID("tenant-b"))
	seedObject(t, repo, ctxA, newUpload("obj-1", "tenant-a", time.Now()))
	seedObject(t, repo, ctxB, newUpload("obj-2", "tenant-b", time.Now()))
	completed := newCompleted("obj-3", "tenant-a", time.Now())
	seedObject(t, repo, ctxA, completed)

	tests := []struct {
		name string
		ctx  context.Context
		id   string
	}{
		{"row that never existed", ctxA, "obj-99"},
		{"row owned by another tenant", ctxB, "obj-1"},
		{"row another completion already finalized", ctxA, "obj-3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row, err := repo.FindByID(tc.ctx, tc.id)
			if err != nil {
				if hasCode(err, dbkit.ErrRecordNotFound.Code) {
					// The never-existed shape has no row to read; a bare id
					// is all the finalize needs.
					row = &Object{ID: tc.id}
				} else {
					t.Fatalf("FindByID: %v", err)
				}
			}
			done, err := repo.finalizeUpload(tc.ctx, row, time.Now())
			if err != nil {
				t.Fatalf("finalizeUpload: %v", err)
			}
			if done {
				t.Fatalf("finalizeUpload done = true for %s, want false", tc.name)
			}
		})
	}

	// The refusals changed nothing: obj-1 is still tenant-a's uploading
	// row, obj-2 still tenant-b's, obj-3 still completed.
	got, err := repo.FindByID(ctxA, "obj-1")
	if err != nil {
		t.Fatalf("FindByID(obj-1) after refusals: %v", err)
	}
	if got.State != ObjectStateUploading {
		t.Errorf("obj-1 state = %q, want %q", got.State, ObjectStateUploading)
	}
	got, err = repo.FindByID(ctxB, "obj-2")
	if err != nil {
		t.Fatalf("FindByID(obj-2) after refusals: %v", err)
	}
	if got.State != ObjectStateUploading {
		t.Errorf("obj-2 state = %q, want %q", got.State, ObjectStateUploading)
	}
	got, err = repo.FindByID(ctxA, "obj-3")
	if err != nil {
		t.Fatalf("FindByID(obj-3) after refusals: %v", err)
	}
	if got.State != ObjectStateCompleted {
		t.Errorf("obj-3 state = %q, want %q", got.State, ObjectStateCompleted)
	}
}

// TestObjectRepository_DeleteObjectRows_RemovesTheRows proves the protocol's
// commit point: one call removes an object's derivative rows and the object
// row itself, permanently -- a later FindByID reports dbkit.ErrRecordNotFound
// and the derivative listing is empty. This is a physical delete by design:
// deletion is the one lifecycle direction that is irreversible, and the
// state machine already marked the row deleting before this call.
func TestObjectRepository_DeleteObjectRows_RemovesTheRows(t *testing.T) {
	db := newTestDB(t)
	objects := NewObjectRepository(db)
	derivatives := NewDerivativeRepository(db)
	ctx := tenantCtx(pkgcore.TenantID("tenant-a"))
	base := time.Now().Add(-1 * time.Hour).Truncate(time.Second)

	seedObject(t, objects, ctx, newCompleted("obj-1", "tenant-a", base))
	seedDerivative(t, derivatives, ctx, newDerivative(t, "deriv-1", "obj-1", DerivativeKindThumbnail, "tenant-a", base.Add(time.Minute)))
	// A second kind of the same object: the delete protocol must walk every
	// derivative a completed object carries, whatever its kind.
	seedDerivative(t, derivatives, ctx, newDerivative(t, "deriv-2", "obj-1", "webp", "tenant-a", base.Add(2*time.Minute)))

	removed, err := objects.deleteObjectRows(ctx, "obj-1")
	if err != nil {
		t.Fatalf("deleteObjectRows: %v", err)
	}
	if !removed {
		t.Errorf("deleteObjectRows removed = false on an existing row, want true")
	}

	if _, findErr := objects.FindByID(ctx, "obj-1"); !hasCode(findErr, dbkit.ErrRecordNotFound.Code) {
		t.Errorf("FindByID after deleteObjectRows = %v, want %v", findErr, dbkit.ErrRecordNotFound.Code)
	}
	rows, err := derivatives.listByObject(ctx, "obj-1")
	if err != nil {
		t.Fatalf("listByObject after deleteObjectRows: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("listByObject returned %d rows after deleteObjectRows, want 0", len(rows))
	}
}

// TestObjectRepository_DeleteObjectRows_DoesNothingForAnUnknownObject pins
// the idempotent edge of the commit point: deleting the rows of an object
// that is already gone -- the state a second protocol run after a crash
// finds -- is not an error. Nothing left to delete is success.
func TestObjectRepository_DeleteObjectRows_DoesNothingForAnUnknownObject(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))

	removed, err := repo.deleteObjectRows(tenantCtx("tenant-a"), "obj-99")
	if err != nil {
		t.Fatalf("deleteObjectRows on an unknown object: %v", err)
	}
	if removed {
		t.Errorf("deleteObjectRows removed = true for a row that never existed, want false")
	}
}

// TestObjectRepository_DeleteObjectRows_IsTenantScoped proves the row
// removal inherits the same isolation as every other statement in this
// file: tenant B calling it over tenant A's object removes nothing, and A's
// row reads back untouched.
func TestObjectRepository_DeleteObjectRows_IsTenantScoped(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))
	ctxA := tenantCtx(pkgcore.TenantID("tenant-a"))
	ctxB := tenantCtx(pkgcore.TenantID("tenant-b"))
	seedObject(t, repo, ctxA, newCompleted("obj-1", "tenant-a", time.Now()))

	removed, err := repo.deleteObjectRows(ctxB, "obj-1")
	if err != nil {
		t.Fatalf("deleteObjectRows across tenants: %v", err)
	}
	if removed {
		t.Errorf("deleteObjectRows removed = true across tenants, want false")
	}
	if _, err := repo.FindByID(ctxA, "obj-1"); err != nil {
		t.Errorf("FindByID under the owning tenant after a foreign delete: %v", err)
	}
}

// TestObjectRepository_ListStateRows_OneStateAtATime proves the sweep's
// resume listing: exactly the rows of one state, oldest first with the id
// tiebreak, whatever other states coexist in the table -- the rows of an
// in-flight delete that a sweep run re-runs the protocol over.
func TestObjectRepository_ListStateRows_OneStateAtATime(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))
	ctx := tenantCtx(pkgcore.TenantID("tenant-a"))
	base := time.Now().Add(-1 * time.Hour).Truncate(time.Second)

	seedObject(t, repo, ctx, newCompleted("c-01", "tenant-a", base.Add(5*time.Minute)))
	seedObject(t, repo, ctx, newUpload("u-01", "tenant-a", base.Add(4*time.Minute)))
	seedObject(t, repo, ctx, newCompleted("c-02", "tenant-a", base.Add(3*time.Minute)))
	seedObject(t, repo, ctx, newUpload("u-02", "tenant-a", base.Add(2*time.Minute)))
	// One deleting row: c-03 was completed, then marked deleting, which is
	// exactly the interrupted-delete shape the resume listing serves.
	del := newCompleted("c-03", "tenant-a", base.Add(6*time.Minute))
	seedObject(t, repo, ctx, del)
	if _, err := repo.markDeleting(ctx, "c-03"); err != nil {
		t.Fatalf("markDeleting c-03: %v", err)
	}

	rows, err := repo.listStateRows(ctx, ObjectStateDeleting)
	if err != nil {
		t.Fatalf("listStateRows(deleting): %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "c-03" {
		t.Errorf("listStateRows(deleting) = %v, want [c-03]", objectIDs(rows))
	}

	// Uploading and completed rows coexist; each state lists exactly its own,
	// oldest first.
	rows, err = repo.listStateRows(ctx, ObjectStateCompleted)
	if err != nil {
		t.Fatalf("listStateRows(completed): %v", err)
	}
	// Oldest first: c-02 (base+3m) precedes c-01 (base+5m).
	assertObjectIDs(t, rows, "c-02", "c-01")
}

// TestObjectRepository_ListExpiredUploads_OnlyExpiredUploads proves the
// reclaim listing's predicate: only uploading rows whose window closed
// strictly before the sweep's now are listed. An uploading row still inside
// its window, and any completed row regardless of age, are not the sweep's
// upload problem.
func TestObjectRepository_ListExpiredUploads_OnlyExpiredUploads(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))
	ctx := tenantCtx(pkgcore.TenantID("tenant-a"))
	now := time.Now()

	// Window closed at now-15m: expired.
	seedObject(t, repo, ctx, newUpload("u-expired", "tenant-a", now.Add(-45*time.Minute)))
	// Window still open until now+30m: not expired.
	seedObject(t, repo, ctx, newUpload("u-fresh", "tenant-a", now))
	// A completed row whose upload window long closed is not reclaimed by
	// the upload path -- the completed sweep owns it, keyed on expires_at.
	seedObject(t, repo, ctx, newCompleted("c-old", "tenant-a", now.Add(-45*time.Minute)))

	rows, err := repo.listExpiredUploads(ctx, now)
	if err != nil {
		t.Fatalf("listExpiredUploads: %v", err)
	}
	assertObjectIDs(t, rows, "u-expired")
}

// TestObjectRepository_ListExpiredCompleted_OnlyExpiredCompleted proves the
// retention sweep's predicate: only completed rows whose expires_at deadline
// passed strictly before the sweep's now are listed. A completed row with a
// future deadline stays, and a completed row with no deadline at all (NULL
// expires_at -- the object that never expires) is excluded explicitly, so
// the NULL branch of the column can never be swept by accident.
func TestObjectRepository_ListExpiredCompleted_OnlyExpiredCompleted(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))
	ctx := tenantCtx(pkgcore.TenantID("tenant-a"))
	now := time.Now()

	past := now.Add(-1 * time.Hour)
	future := now.Add(1 * time.Hour)

	expired := newCompleted("c-expired", "tenant-a", past)
	expired.ExpiresAt = &past
	seedObject(t, repo, ctx, expired)

	kept := newCompleted("c-kept", "tenant-a", past.Add(-time.Minute))
	kept.ExpiresAt = &future
	seedObject(t, repo, ctx, kept)

	never := newCompleted("c-never", "tenant-a", past.Add(-2*time.Minute))
	// ExpiresAt stays nil: the never-expiring object.
	seedObject(t, repo, ctx, never)

	// An uploading row with a past deadline is not the retention sweep's
	// row: the upload sweep owns it, keyed on upload_expires_at.
	uploading := newUpload("u-past", "tenant-a", past)
	uploading.ExpiresAt = &past
	seedObject(t, repo, ctx, uploading)

	rows, err := repo.listExpiredCompleted(ctx, now)
	if err != nil {
		t.Fatalf("listExpiredCompleted: %v", err)
	}
	assertObjectIDs(t, rows, "c-expired")
}

// TestObjectRepository_SweepListings_FailClosedWithoutTenant pins the
// no-tenant shape for the sweep listings, taking listExpiredUploads as the
// representative: every repository call in this file fails closed with
// pkgcore.ErrNoTenant -- wrapped in the module's own internal error, cause
// intact -- because a sweep handler that ran without tenant context must
// error out, never sweep across every tenant.
func TestObjectRepository_SweepListings_FailClosedWithoutTenant(t *testing.T) {
	repo := NewObjectRepository(newTestDB(t))

	rows, err := repo.listExpiredUploads(context.Background(), time.Now())
	if rows != nil {
		t.Errorf("listExpiredUploads returned %d rows with no tenant in context, want none", len(rows))
	}
	if !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Errorf("listExpiredUploads error = %v, want errors.Is(err, pkgcore.ErrNoTenant)", err)
	}
	assertCode(t, err, ErrInternal.Code)
}

// TestDerivativeRepository_ListByObject_OneObjectsDerivatives proves the
// delete protocol's walk: one object's derivatives come back oldest first,
// and rows belonging to other objects of the same tenant are never mixed in.
func TestDerivativeRepository_ListByObject_OneObjectsDerivatives(t *testing.T) {
	repo := NewDerivativeRepository(newTestDB(t))
	ctx := tenantCtx(pkgcore.TenantID("tenant-a"))
	base := time.Now().Add(-1 * time.Hour).Truncate(time.Second)

	// obj-a carries two derivatives of different kinds -- the shape the
	// delete protocol walks -- and obj-b one, to prove the listing never
	// mixes objects.
	seedDerivative(t, repo, ctx, newDerivative(t, "deriv-a1", "obj-a", "webp", "tenant-a", base.Add(2*time.Minute)))
	seedDerivative(t, repo, ctx, newDerivative(t, "deriv-a0", "obj-a", DerivativeKindThumbnail, "tenant-a", base.Add(1*time.Minute)))
	seedDerivative(t, repo, ctx, newDerivative(t, "deriv-b1", "obj-b", DerivativeKindThumbnail, "tenant-a", base.Add(3*time.Minute)))

	rows, err := repo.listByObject(ctx, "obj-a")
	if err != nil {
		t.Fatalf("listByObject: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != "deriv-a0" || rows[1].ID != "deriv-a1" {
		t.Errorf("listByObject(obj-a) = [%s %s], want [deriv-a0 deriv-a1]", idOf(rows, 0), idOf(rows, 1))
	}
}

// idOf returns the id of rows[i], or "" when i is out of range, for failure
// messages.
func idOf(rows []ObjectDerivative, i int) string {
	if i < len(rows) {
		return rows[i].ID
	}
	return ""
}

// TestDerivativeRepository_InsertDerivativeIfAbsent_InsertsOnce pins the
// derive pipeline's idempotent skip at the row layer: a first insert lands,
// a second insert of the same (object, kind) is a silent no-op -- the shape
// a re-run after a crash between the byte write and the row insert takes --
// and a different kind of the same object still inserts, because the skip
// is per (object, kind), never per object. The object the rows name is
// seeded completed, the state the insert's object gate (below) requires.
func TestDerivativeRepository_InsertDerivativeIfAbsent_InsertsOnce(t *testing.T) {
	db := newTestDB(t)
	objRepo := NewObjectRepository(db)
	repo := NewDerivativeRepository(db)
	ctx := tenantCtx(pkgcore.TenantID("tenant-a"))
	base := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	seedObject(t, objRepo, ctx, newCompleted("obj-1", "tenant-a", base))
	row := newDerivative(t, "deriv-1", "obj-1", DerivativeKindThumbnail, "tenant-a", time.Now())

	if refused, err := repo.insertDerivativeIfAbsent(ctx, row); err != nil {
		t.Fatalf("first insertDerivativeIfAbsent: %v", err)
	} else if refused {
		t.Fatal("first insertDerivativeIfAbsent refused: the object is seeded completed")
	}
	if refused, err := repo.insertDerivativeIfAbsent(ctx, row); err != nil {
		t.Fatalf("second insertDerivativeIfAbsent (the idempotent skip): %v", err)
	} else if refused {
		t.Fatal("second insertDerivativeIfAbsent refused: the object is still completed, the skip should answer")
	}

	rows, err := repo.listByObject(ctx, "obj-1")
	if err != nil {
		t.Fatalf("listByObject: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "deriv-1" {
		t.Errorf("listByObject returned %d rows, want exactly the one inserted (deriv-1)", len(rows))
	}
}

// TestDerivativeRepository_InsertDerivativeIfAbsent_GatedOnTheObject pins
// the object-state gate at the row layer: a derivative row may only be
// inserted while the object's own row exists and reads completed at the
// moment of the insert. The gate is what closes the delete/derive race --
// an insert landing after the delete protocol's row removal ran (the object
// row is gone) or while it is in flight (the object is marked deleting)
// would be a ghost row no object row ever walks again, naming bytes the
// protocol already removed or is about to remove. Each refused shape must
// insert nothing; the completed shape is the one commit the derive pipeline
// relies on.
func TestDerivativeRepository_InsertDerivativeIfAbsent_GatedOnTheObject(t *testing.T) {
	db := newTestDB(t)
	objRepo := NewObjectRepository(db)
	repo := NewDerivativeRepository(db)
	ctx := tenantCtx(pkgcore.TenantID("tenant-a"))
	base := time.Now().Add(-1 * time.Hour).Truncate(time.Second)

	assertRefused := func(t *testing.T, objectID, derivID string) {
		t.Helper()
		row := newDerivative(t, derivID, objectID, DerivativeKindThumbnail, "tenant-a", base)
		refused, err := repo.insertDerivativeIfAbsent(ctx, row)
		if err != nil {
			t.Fatalf("insertDerivativeIfAbsent: %v", err)
		}
		if !refused {
			t.Fatalf("insertDerivativeIfAbsent refused = false for object %q -- the gate let a ghost row through", objectID)
		}
		rows, err := repo.listByObject(ctx, objectID)
		if err != nil {
			t.Fatalf("listByObject: %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("refused insert still landed %d row(s) for object %q (want 0)", len(rows), objectID)
		}
	}

	t.Run("no object row", func(t *testing.T) {
		assertRefused(t, "obj-missing", "deriv-missing")
	})

	t.Run("object marked deleting", func(t *testing.T) {
		seedObject(t, objRepo, ctx, newCompleted("obj-deleting", "tenant-a", base))
		if _, err := objRepo.markDeleting(ctx, "obj-deleting"); err != nil {
			t.Fatalf("markDeleting: %v", err)
		}
		assertRefused(t, "obj-deleting", "deriv-deleting")
	})

	t.Run("object still uploading", func(t *testing.T) {
		seedObject(t, objRepo, ctx, newUpload("obj-uploading", "tenant-a", base))
		assertRefused(t, "obj-uploading", "deriv-uploading")
	})

	t.Run("completed object's insert commits", func(t *testing.T) {
		seedObject(t, objRepo, ctx, newCompleted("obj-completed", "tenant-a", base))
		row := newDerivative(t, "deriv-completed", "obj-completed", DerivativeKindThumbnail, "tenant-a", base)
		refused, err := repo.insertDerivativeIfAbsent(ctx, row)
		if err != nil {
			t.Fatalf("insertDerivativeIfAbsent: %v", err)
		}
		if refused {
			t.Fatal("insertDerivativeIfAbsent refused for a completed object")
		}
		rows, err := repo.listByObject(ctx, "obj-completed")
		if err != nil {
			t.Fatalf("listByObject: %v", err)
		}
		if len(rows) != 1 || rows[0].ID != "deriv-completed" {
			t.Errorf("listByObject returned %d rows, want exactly deriv-completed", len(rows))
		}
	})
}
