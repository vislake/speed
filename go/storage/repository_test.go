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
