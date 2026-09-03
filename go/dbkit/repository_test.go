package dbkit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/vislake/speed/go/dbkit/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// newWidgetRepo returns a Repository[testutil.Widget] backed by a fresh,
// private in-memory SQLite database (testutil.NewTestSQLite; see its own
// doc comment). That connection has no tenant-scoping GORM plugin installed
// — it is a plain gorm.Open, not built through dbkit.Open, which does not
// exist yet at this point in dbkit's build-out. So every isolation
// assertion in this file is exercising Repository's own, independent
// enforcement (layer 2 in the Repository doc comment), never layer 1's
// plugin. That is deliberate: it is exactly the property this file is
// required to prove, and it is also why every test below that expects
// cross-tenant access to be blocked would catch a regression in
// Repository itself, with nothing else able to paper over it.
func newWidgetRepo(t *testing.T) *Repository[testutil.Widget] {
	t.Helper()
	return NewRepository[testutil.Widget](testutil.NewTestSQLite(t))
}

// ctxTenant returns a context.Background() carrying tenant as the current
// tenant.
func ctxTenant(tenant string) context.Context {
	return pkgcore.WithTenant(context.Background(), pkgcore.TenantID(tenant))
}

// isRecordNotFound reports whether err is ErrRecordNotFound, matched by Code
// rather than by identity: apperr.WithParam always returns a new *apperr.Error,
// so the pointer returned by a Repository method is never the same pointer as
// the package-level ErrRecordNotFound sentinel (see apperr's own doc comment
// on this pattern).
func isRecordNotFound(err error) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == ErrRecordNotFound.Code
}

// idsOf returns the sorted ids of widgets, for order-independent comparison.
func idsOf(widgets []testutil.Widget) []string {
	ids := make([]string, len(widgets))
	for i, w := range widgets {
		ids[i] = w.ID
	}
	sort.Strings(ids)
	return ids
}

func TestRepository_FullCRUDLifecycle_SingleTenant(t *testing.T) {
	repo := newWidgetRepo(t)
	ctx := ctxTenant("tenant-a")

	widget := &testutil.Widget{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Name: "gadget", Value: 1}
	if err := repo.Create(ctx, widget); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	created, err := repo.FindByID(ctx, widget.ID)
	if err != nil {
		t.Fatalf("FindByID() after Create error = %v", err)
	}
	wantCreated := testutil.Widget{ID: widget.ID, TenantID: "tenant-a", Name: "gadget", Value: 1}
	if *created != wantCreated {
		t.Errorf("FindByID() after Create = %+v, want %+v", *created, wantCreated)
	}

	created.Name = "gadget-v2"
	created.Value = 2
	if err = repo.Update(ctx, created); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	updated, err := repo.FindByID(ctx, widget.ID)
	if err != nil {
		t.Fatalf("FindByID() after Update error = %v", err)
	}
	wantUpdated := testutil.Widget{ID: widget.ID, TenantID: "tenant-a", Name: "gadget-v2", Value: 2}
	if *updated != wantUpdated {
		t.Errorf("FindByID() after Update = %+v, want %+v", *updated, wantUpdated)
	}

	listed, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List() before Delete error = %v", err)
	}
	if len(listed) != 1 || listed[0] != wantUpdated {
		t.Errorf("List() before Delete = %+v, want exactly [%+v]", listed, wantUpdated)
	}

	if err = repo.Delete(ctx, widget.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	got, err := repo.FindByID(ctx, widget.ID)
	if !isRecordNotFound(err) {
		t.Errorf("FindByID() after Delete = (%v, %v), want (nil, ErrRecordNotFound)", got, err)
	}
}

func TestRepository_FindByID_DifferentTenant_ReturnsNotFound(t *testing.T) {
	repo := newWidgetRepo(t)
	owner := ctxTenant("tenant-a")
	other := ctxTenant("tenant-b")

	widget := &testutil.Widget{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Name: "gadget", Value: 1}
	if err := repo.Create(owner, widget); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// A real id, just owned by a different tenant than the caller.
	got, err := repo.FindByID(other, widget.ID)
	if got != nil {
		t.Errorf("FindByID() from a different tenant returned a row: %+v, want nil", got)
	}
	if !isRecordNotFound(err) {
		t.Errorf("FindByID() from a different tenant error = %v, want ErrRecordNotFound (not the row, and not a different, more revealing error)", err)
	}
}

func TestRepository_Update_DifferentTenant_ReturnsNotFoundAndLeavesRowUnchanged(t *testing.T) {
	repo := newWidgetRepo(t)
	owner := ctxTenant("tenant-a")
	other := ctxTenant("tenant-b")

	if err := repo.Create(owner, &testutil.Widget{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Name: "gadget", Value: 1}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Same id as the real row, attempted from the other tenant's context.
	attempt := &testutil.Widget{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Name: "hijacked", Value: 999}
	if err := repo.Update(other, attempt); !isRecordNotFound(err) {
		t.Fatalf("Update() from a different tenant error = %v, want ErrRecordNotFound", err)
	}

	// The real owner's row must be exactly as it was: an update whose
	// tenant-scoped WHERE clause matches nothing must never fall back to
	// creating or modifying anything (see the Update doc comment's
	// explanation of why Select("*") before Save is load-bearing here).
	got, err := repo.FindByID(owner, "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err != nil {
		t.Fatalf("FindByID() by the real owner after the failed cross-tenant Update error = %v", err)
	}
	if got.Name != "gadget" || got.Value != 1 {
		t.Errorf("row after failed cross-tenant Update = %+v, want unchanged {Name: gadget, Value: 1}", *got)
	}

	// And no phantom row was created under the attacker's tenant either.
	if _, err := repo.FindByID(other, "01ARZ3NDEKTSV4RRFFQ69G5FAV"); !isRecordNotFound(err) {
		t.Errorf("FindByID() under the attacking tenant after the failed Update error = %v, want ErrRecordNotFound (no phantom row)", err)
	}
}

func TestRepository_Delete_DifferentTenant_ReturnsNotFoundAndDoesNotDeleteRow(t *testing.T) {
	repo := newWidgetRepo(t)
	owner := ctxTenant("tenant-a")
	other := ctxTenant("tenant-b")

	widget := &testutil.Widget{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Name: "gadget", Value: 1}
	if err := repo.Create(owner, widget); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := repo.Delete(other, widget.ID); !isRecordNotFound(err) {
		t.Fatalf("Delete() from a different tenant error = %v, want ErrRecordNotFound", err)
	}

	if _, err := repo.FindByID(owner, widget.ID); err != nil {
		t.Errorf("FindByID() by the real owner after the failed cross-tenant Delete error = %v, want the row still present", err)
	}
}

func TestRepository_Create_ForgedTenantIDOnModel_IsOverwrittenByContextTenant(t *testing.T) {
	repo := newWidgetRepo(t)
	ctx := ctxTenant("tenant-a")

	// A caller must not be able to write a row into a different tenant than
	// ctx by setting TenantID on the struct passed to Create — Create
	// backfills (overwrites) it unconditionally. Set it to a different
	// tenant here deliberately to prove that.
	widget := &testutil.Widget{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", TenantID: "tenant-b", Name: "gadget"}
	if err := repo.Create(ctx, widget); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if widget.TenantID != "tenant-a" {
		t.Errorf("widget.TenantID on the struct after Create() = %q, want %q (Create must overwrite it from ctx)", widget.TenantID, "tenant-a")
	}

	if _, err := repo.FindByID(ctxTenant("tenant-b"), widget.ID); !isRecordNotFound(err) {
		t.Errorf("FindByID() under the forged tenant error = %v, want ErrRecordNotFound (the row must not exist there)", err)
	}

	got, err := repo.FindByID(ctx, widget.ID)
	if err != nil {
		t.Fatalf("FindByID() under the real context tenant error = %v", err)
	}
	if got.TenantID != "tenant-a" {
		t.Errorf("persisted TenantID = %q, want %q", got.TenantID, "tenant-a")
	}
}

func TestRepository_Update_EmptyID_ReturnsErrorWithoutInserting(t *testing.T) {
	repo := newWidgetRepo(t)
	ctx := ctxTenant("tenant-a")

	// gorm's Save unconditionally falls back to inserting a new row when a
	// model's primary-key field looks unset (see the Update doc comment) —
	// an empty id is exactly that. Update must reject it before ever
	// reaching Save, not silently create a bogus record.
	err := repo.Update(ctx, &testutil.Widget{Name: "no-id"})
	if err == nil {
		t.Fatal("Update() with an empty id error = nil, want an error")
	}
	if appErr, ok := apperr.As(err); !ok || appErr.Code != ErrMissingID.Code {
		t.Errorf("Update() with an empty id error = %v, want code %q", err, ErrMissingID.Code)
	}

	rows, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("List() after rejected empty-id Update = %+v, want no rows (Update must not have inserted anything)", rows)
	}
}

// newIDAndTenantOnlyMarkerRepo returns a
// Repository[testutil.IDAndTenantOnlyMarker] backed by a fresh, private
// in-memory SQLite database (the same testutil.NewTestSQLite connection
// newWidgetRepo uses), with its own table created by hand since
// testutil.IDAndTenantOnlyMarker is not part of the widgets fixture
// testutil.NewTestSQLite migrates. See that type's own doc comment
// (internal/testutil/id_and_tenant_only_marker.go) for why it exists and
// why it is shared with the integration tier rather than defined here.
func newIDAndTenantOnlyMarkerRepo(t *testing.T) *Repository[testutil.IDAndTenantOnlyMarker] {
	t.Helper()
	db := testutil.NewTestSQLite(t)
	if err := db.Exec(testutil.IDAndTenantOnlyMarkerTableSQL).Error; err != nil {
		t.Fatalf("create id_and_tenant_only_markers table: %v", err)
	}
	return NewRepository[testutil.IDAndTenantOnlyMarker](db)
}

// TestRepository_Update_IDAndTenantIDOnlyModel_SucceedsAsNoOp is the
// regression test for the bug where Update on a TenantScoped model whose
// only fields are ID and TenantID — a pure marker/link record with no
// other column — silently returned ErrRecordNotFound even though the row
// genuinely existed and was genuinely owned by the calling tenant.
//
// Root cause: gorm's Update callback computes its SET clause by excluding
// every primary-key column (gorm's callbacks/update.go,
// ConvertToAssignments). When T has no non-key column at all, that SET
// clause comes back empty, and gorm's own callback returns immediately --
// no SQL is built, let alone executed — leaving RowsAffected at its zero
// value. Update used to treat rowsAffected == 0 as "no such row for this
// tenant" unconditionally, which was wrong here: the row existed the whole
// time, confirmed by the FindByID call immediately before Update in this
// test. See Update's own doc comment in repository.go for the fix.
func TestRepository_Update_IDAndTenantIDOnlyModel_SucceedsAsNoOp(t *testing.T) {
	repo := newIDAndTenantOnlyMarkerRepo(t)
	ctx := ctxTenant("tenant-a")

	m := &testutil.IDAndTenantOnlyMarker{ID: "marker-1"}
	if err := repo.Create(ctx, m); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got, err := repo.FindByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("FindByID() after Create error = %v — the row must genuinely exist and be owned by tenant-a", err)
	}

	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("Update() on an existing, tenant-owned, ID+TenantID-only row error = %v, want nil (this is the bug: gorm's own empty-SET-clause no-op left RowsAffected at 0, which Update misread as ErrRecordNotFound)", err)
	}

	// The row must still be there afterward, unchanged — Update must not
	// have deleted it, and must not have fallen back to an upsert-style
	// Create either (see the Update doc comment's Select("*") discussion).
	if _, err := repo.FindByID(ctx, m.ID); err != nil {
		t.Errorf("FindByID() after the no-op Update error = %v, want the row still present", err)
	}
}

// TestRepository_Update_IDAndTenantIDOnlyModel_DifferentTenant_ReturnsNotFound
// proves the fix for the bug above does not weaken tenant isolation: an
// Update attempt against an ID+TenantID-only row from a DIFFERENT tenant
// must still return ErrRecordNotFound, exactly like every other T — not
// be silently treated as a successful no-op just because this T now falls
// back to an existence-check path when gorm's own SET clause is empty. The
// existence check that fallback runs is scoped by ctx's tenant exactly
// like every other Repository query, so it must never find, or report
// success for, a row belonging to a different tenant.
func TestRepository_Update_IDAndTenantIDOnlyModel_DifferentTenant_ReturnsNotFound(t *testing.T) {
	repo := newIDAndTenantOnlyMarkerRepo(t)
	owner := ctxTenant("tenant-a")
	other := ctxTenant("tenant-b")

	if err := repo.Create(owner, &testutil.IDAndTenantOnlyMarker{ID: "marker-1"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Same id as the real row, attempted from the other tenant's context.
	if err := repo.Update(other, &testutil.IDAndTenantOnlyMarker{ID: "marker-1"}); !isRecordNotFound(err) {
		t.Fatalf("Update() from a different tenant error = %v, want ErrRecordNotFound", err)
	}

	// The real owner's row must still exist, untouched.
	if _, err := repo.FindByID(owner, "marker-1"); err != nil {
		t.Errorf("FindByID() by the real owner after the failed cross-tenant Update error = %v, want the row still present", err)
	}

	// And no phantom row was created under the attacker's tenant either.
	if _, err := repo.FindByID(other, "marker-1"); !isRecordNotFound(err) {
		t.Errorf("FindByID() under the attacking tenant after the failed Update error = %v, want ErrRecordNotFound (no phantom row)", err)
	}
}

// TestRepository_Update_IDAndTenantIDOnlyModel_NoSuchID_ReturnsNotFound
// confirms the fallback existence check the fix above added still returns
// ErrRecordNotFound, not a false-positive success, when the id genuinely
// was never created at all (as opposed to existing under a different
// tenant, covered by the test above) — the fallback path must not treat
// "found nothing" as anything other than not-found.
func TestRepository_Update_IDAndTenantIDOnlyModel_NoSuchID_ReturnsNotFound(t *testing.T) {
	repo := newIDAndTenantOnlyMarkerRepo(t)
	ctx := ctxTenant("tenant-a")

	err := repo.Update(ctx, &testutil.IDAndTenantOnlyMarker{ID: "never-created"})
	if !isRecordNotFound(err) {
		t.Fatalf("Update() for an id that was never created error = %v, want ErrRecordNotFound", err)
	}
}

func TestRepository_List_TwoTenants_ReturnsOnlyCallingTenantRows(t *testing.T) {
	repo := newWidgetRepo(t)
	tenantA := ctxTenant("tenant-a")
	tenantB := ctxTenant("tenant-b")

	for _, w := range []testutil.Widget{
		{ID: "a-1", Name: "a-gadget-1"},
		{ID: "a-2", Name: "a-gadget-2"},
	} {
		w := w
		if err := repo.Create(tenantA, &w); err != nil {
			t.Fatalf("Create() tenant-a widget %q error = %v", w.ID, err)
		}
	}
	for _, w := range []testutil.Widget{
		{ID: "b-1", Name: "b-gadget-1"},
	} {
		w := w
		if err := repo.Create(tenantB, &w); err != nil {
			t.Fatalf("Create() tenant-b widget %q error = %v", w.ID, err)
		}
	}

	gotA, err := repo.List(tenantA)
	if err != nil {
		t.Fatalf("List(tenant-a) error = %v", err)
	}
	if want := []string{"a-1", "a-2"}; !slices.Equal(idsOf(gotA), want) {
		t.Errorf("List(tenant-a) ids = %v, want %v", idsOf(gotA), want)
	}

	gotB, err := repo.List(tenantB)
	if err != nil {
		t.Fatalf("List(tenant-b) error = %v", err)
	}
	if want := []string{"b-1"}; !slices.Equal(idsOf(gotB), want) {
		t.Errorf("List(tenant-b) ids = %v, want %v", idsOf(gotB), want)
	}
}

// countingLogger wraps another gormlogger.Interface and counts every SQL
// statement gorm traces through it. gorm calls Trace exactly once per
// statement it executes, regardless of log level — even the Silent-level
// logger testutil.NewTestSQLite installs still has Trace invoked on every
// query, it just decides not to print — so counting Trace calls is an
// accurate, log-level-independent signal of "SQL was issued against this
// *gorm.DB". This is the mechanism TestRepository_NoTenantInContext below
// uses to verify each method fails before touching the database, not just
// that it returns an error.
type countingLogger struct {
	gormlogger.Interface
	n *atomic.Int64
}

func (c countingLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	c.n.Add(1)
	c.Interface.Trace(ctx, begin, fc, err)
}

// newCountingWidgetRepo returns a Repository[testutil.Widget] whose queries
// are all counted, and the counter itself. See countingLogger's doc comment
// for how "no SQL was issued" is verified from a test.
func newCountingWidgetRepo(t *testing.T) (*Repository[testutil.Widget], *atomic.Int64) {
	t.Helper()
	db := testutil.NewTestSQLite(t)
	n := new(atomic.Int64)
	counted := db.Session(&gorm.Session{Logger: countingLogger{Interface: db.Logger, n: n}})
	return NewRepository[testutil.Widget](counted), n
}

func TestRepository_NoTenantInContext_FailsClosedBeforeAnyQuery(t *testing.T) {
	tests := []struct {
		name string
		call func(ctx context.Context, repo *Repository[testutil.Widget]) error
	}{
		{
			name: "Create",
			call: func(ctx context.Context, repo *Repository[testutil.Widget]) error {
				return repo.Create(ctx, &testutil.Widget{ID: "id-1", Name: "x"})
			},
		},
		{
			name: "FindByID",
			call: func(ctx context.Context, repo *Repository[testutil.Widget]) error {
				_, err := repo.FindByID(ctx, "id-1")
				return err
			},
		},
		{
			name: "Update",
			call: func(ctx context.Context, repo *Repository[testutil.Widget]) error {
				return repo.Update(ctx, &testutil.Widget{ID: "id-1", Name: "x"})
			},
		},
		{
			name: "Delete",
			call: func(ctx context.Context, repo *Repository[testutil.Widget]) error {
				return repo.Delete(ctx, "id-1")
			},
		},
		{
			name: "List",
			call: func(ctx context.Context, repo *Repository[testutil.Widget]) error {
				_, err := repo.List(ctx)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, queries := newCountingWidgetRepo(t)

			err := tt.call(context.Background(), repo)
			if err == nil {
				t.Fatal("error = nil, want an error for a context with no tenant")
			}
			if !errors.Is(err, pkgcore.ErrNoTenant) {
				t.Errorf("error = %v, want errors.Is(err, pkgcore.ErrNoTenant)", err)
			}
			// The load-bearing assertion: verified by wrapping this
			// Repository's *gorm.DB with countingLogger (see its doc
			// comment) and checking gorm's Trace hook was never invoked, so
			// this is not just "returned an error" but "returned before
			// issuing any SQL at all".
			if got := queries.Load(); got != 0 {
				t.Errorf("query count = %d, want 0 (%s must fail before touching the database)", got, tt.name)
			}
		})
	}
}

// softDeleteRepoTestDBSeq numbers this file's dbkit.Open-backed in-memory
// SQLite databases for the SoftDeletable tests below, so repeated or
// parallel runs never share one.
var softDeleteRepoTestDBSeq atomic.Int64

// newSoftDeletableWidgetRepo returns a Repository[testutil.SoftDeletableWidget]
// backed by a fresh, private in-memory SQLite database opened through Open
// itself (unlike newWidgetRepo's plain testutil.NewTestSQLite) — this is
// deliberate: Repository[T]'s FindByID and List issue no deleted_at
// condition of their own (the design's mark-delete auto-scope lives
// entirely in the query-callback plugin, soft_delete.go), so hiding a
// soft-deleted row from them depends on that plugin actually being
// installed on the underlying *gorm.DB, exactly as it would be in
// production. Open installs it unconditionally (see open.go).
func newSoftDeletableWidgetRepo(t *testing.T) *Repository[testutil.SoftDeletableWidget] {
	t.Helper()
	dsn := fmt.Sprintf("file:soft_delete_repo_test_%d?mode=memory&cache=shared", softDeleteRepoTestDBSeq.Add(1))
	db, err := Open(context.Background(), Options{Dialect: DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := db.Exec(testutil.SoftDeletableWidgetTableSQL).Error; err != nil {
		t.Fatalf("create soft_deletable_widgets table: %v", err)
	}
	return NewRepository[testutil.SoftDeletableWidget](db)
}

// rawSoftDeletableWidgetRow reads id's raw deleted_at/deleted_by columns
// directly through db.Raw, bypassing every GORM callback (including the
// soft-delete auto-scope plugin, which raw SQL never runs) — the
// "what actually landed in the database" check every test below uses
// instead of trusting Repository's own account of what it did. found is
// false when no row with id exists at all.
func rawSoftDeletableWidgetRow(t *testing.T, db *gorm.DB, id string) (found bool, deletedAt *time.Time, deletedBy string) {
	t.Helper()
	var (
		nullDeletedAt sql.NullTime
		nullDeletedBy sql.NullString
	)
	row := db.Raw(`SELECT deleted_at, deleted_by FROM soft_deletable_widgets WHERE id = ?`, id).Row()
	if err := row.Scan(&nullDeletedAt, &nullDeletedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil, ""
		}
		t.Fatalf("raw soft_deletable_widgets lookup for id %q: %v", id, err)
	}
	if nullDeletedAt.Valid {
		deletedAt = &nullDeletedAt.Time
	}
	return true, deletedAt, nullDeletedBy.String
}

// ctxTenantActor returns ctxTenant(tenant) additionally carrying actor as
// the current pkgcore.Actor, for the softDelete tests that need to prove
// deleted_by is populated from it.
func ctxTenantActor(tenant, actorID string) context.Context {
	return pkgcore.WithActor(ctxTenant(tenant), pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: actorID})
}

func TestRepository_Delete_SoftDeletable_MarksInsteadOfPhysicallyDeleting(t *testing.T) {
	repo := newSoftDeletableWidgetRepo(t)
	ctx := ctxTenantActor("tenant-a", "user-1")

	w := &testutil.SoftDeletableWidget{ID: "w1", Name: "gadget"}
	if err := repo.Create(ctx, w); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	before := time.Now()
	if err := repo.Delete(ctx, w.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	after := time.Now()

	found, deletedAt, deletedBy := rawSoftDeletableWidgetRow(t, repo.db, w.ID)
	if !found {
		t.Fatal("row physically gone after Delete() on a SoftDeletable model, want it still present (mark-delete, not physical delete)")
	}
	if deletedAt == nil {
		t.Fatal("deleted_at = nil, want a populated timestamp")
	}
	if deletedAt.Before(before.Add(-time.Second)) || deletedAt.After(after.Add(time.Second)) {
		t.Errorf("deleted_at = %v, want a timestamp between %v and %v", deletedAt, before, after)
	}
	if deletedBy != "user-1" {
		t.Errorf("deleted_by = %q, want %q", deletedBy, "user-1")
	}
}

func TestRepository_Delete_SoftDeletable_HiddenFromFindByIDAndList(t *testing.T) {
	repo := newSoftDeletableWidgetRepo(t)
	ctx := ctxTenantActor("tenant-a", "user-1")

	w := &testutil.SoftDeletableWidget{ID: "w1", Name: "gadget"}
	if err := repo.Create(ctx, w); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := repo.Delete(ctx, w.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if _, err := repo.FindByID(ctx, w.ID); !isRecordNotFound(err) {
		t.Errorf("FindByID() after Delete() error = %v, want ErrRecordNotFound", err)
	}
	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 0 {
		t.Errorf("List() after Delete() = %+v, want empty", list)
	}
}

func TestRepository_Delete_SoftDeletable_AlreadyDeleted_ReturnsNotFound(t *testing.T) {
	repo := newSoftDeletableWidgetRepo(t)
	ctx := ctxTenantActor("tenant-a", "user-1")

	w := &testutil.SoftDeletableWidget{ID: "w1", Name: "gadget"}
	if err := repo.Create(ctx, w); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := repo.Delete(ctx, w.ID); err != nil {
		t.Fatalf("first Delete() error = %v", err)
	}
	_, firstDeletedAt, firstDeletedBy := rawSoftDeletableWidgetRow(t, repo.db, w.ID)

	// A second delete, by a different actor, must not succeed and must not
	// clobber the first delete's own deleted_at/deleted_by attribution --
	// the deleted_at IS NULL guard in softDelete's own WHERE clause is what
	// makes this a no-match rather than a second write.
	secondCtx := ctxTenantActor("tenant-a", "user-2")
	if err := repo.Delete(secondCtx, w.ID); !isRecordNotFound(err) {
		t.Fatalf("second Delete() on an already-soft-deleted row error = %v, want ErrRecordNotFound", err)
	}

	_, secondDeletedAt, secondDeletedBy := rawSoftDeletableWidgetRow(t, repo.db, w.ID)
	if secondDeletedBy != firstDeletedBy {
		t.Errorf("deleted_by after the failed second Delete() = %q, want unchanged %q", secondDeletedBy, firstDeletedBy)
	}
	if !secondDeletedAt.Equal(*firstDeletedAt) {
		t.Errorf("deleted_at after the failed second Delete() = %v, want unchanged %v", secondDeletedAt, firstDeletedAt)
	}
}

func TestRepository_Restore_SoftDeletable_ClearsDeletedAtAndReappearsInQueries(t *testing.T) {
	repo := newSoftDeletableWidgetRepo(t)
	ctx := ctxTenantActor("tenant-a", "user-1")

	w := &testutil.SoftDeletableWidget{ID: "w1", Name: "gadget"}
	if err := repo.Create(ctx, w); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := repo.Delete(ctx, w.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if err := repo.Restore(ctx, w.ID); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}

	found, deletedAt, deletedBy := rawSoftDeletableWidgetRow(t, repo.db, w.ID)
	if !found {
		t.Fatal("row gone after Restore(), want it present")
	}
	if deletedAt != nil {
		t.Errorf("deleted_at after Restore() = %v, want nil", deletedAt)
	}
	if deletedBy != "" {
		t.Errorf("deleted_by after Restore() = %q, want empty", deletedBy)
	}

	got, err := repo.FindByID(ctx, w.ID)
	if err != nil {
		t.Fatalf("FindByID() after Restore() error = %v", err)
	}
	if got.ID != w.ID {
		t.Errorf("FindByID() after Restore() = %+v, want ID %q", got, w.ID)
	}
	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List() after Restore() error = %v", err)
	}
	if len(list) != 1 || list[0].ID != w.ID {
		t.Errorf("List() after Restore() = %+v, want exactly [%s]", list, w.ID)
	}
}

func TestRepository_Restore_SoftDeletable_NotCurrentlyDeleted_ReturnsNotFound(t *testing.T) {
	repo := newSoftDeletableWidgetRepo(t)
	ctx := ctxTenantActor("tenant-a", "user-1")

	w := &testutil.SoftDeletableWidget{ID: "w1", Name: "gadget"}
	if err := repo.Create(ctx, w); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := repo.Restore(ctx, w.ID); !isRecordNotFound(err) {
		t.Fatalf("Restore() on a never-deleted row error = %v, want ErrRecordNotFound", err)
	}
}

func TestRepository_Restore_NonSoftDeletable_ReturnsErrNotSoftDeletable(t *testing.T) {
	repo := newWidgetRepo(t)
	ctx := ctxTenant("tenant-a")

	widget := &testutil.Widget{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Name: "gadget"}
	if err := repo.Create(ctx, widget); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	err := repo.Restore(ctx, widget.ID)
	appErr, ok := apperr.As(err)
	if !ok || appErr.Code != ErrNotSoftDeletable.Code {
		t.Fatalf("Restore() on a non-SoftDeletable T error = %v, want ErrNotSoftDeletable", err)
	}
}

// TestRepository_Delete_NonSoftDeletable_PhysicalDeleteUnchanged is the
// backward-compatibility regression this round promises: a T that does not
// implement SoftDeletable (Widget) keeps today's real, physical DELETE,
// byte-for-byte -- the row is genuinely gone from the table, not merely
// hidden, and RowsAffected-derived ErrRecordNotFound behavior on a second
// Delete is unchanged.
func TestRepository_Delete_NonSoftDeletable_PhysicalDeleteUnchanged(t *testing.T) {
	repo := newWidgetRepo(t)
	ctx := ctxTenant("tenant-a")

	widget := &testutil.Widget{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Name: "gadget"}
	if err := repo.Create(ctx, widget); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := repo.Delete(ctx, widget.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	var count int64
	if err := repo.db.Raw(`SELECT COUNT(*) FROM widgets WHERE id = ?`, widget.ID).Scan(&count).Error; err != nil {
		t.Fatalf("raw widgets count: %v", err)
	}
	if count != 0 {
		t.Fatalf("raw row count after Delete() = %d, want 0 (physical delete, row genuinely gone)", count)
	}

	if err := repo.Delete(ctx, widget.ID); !isRecordNotFound(err) {
		t.Fatalf("second Delete() of an already-gone row error = %v, want ErrRecordNotFound", err)
	}
}

// TestRepository_Update_StaleModelAfterSoftDelete_SilentlyClearsDeletedAt
// documents (and pins, so a future change to Update's behavior here is a
// deliberate decision, not an accident) a real, un-fixed hazard this round
// explicitly chose not to address: Update's existing "Select(\"*\")"
// full-record save writes every column, deleted_at/deleted_by included,
// and is not scoped by deleted_at at all (the auto-scope plugin is
// query-only, per soft_delete.go's own doc comment). A caller that Delete's
// a row and then Update's a stale in-memory copy captured before the
// delete silently "undeletes" it as a side effect -- Update was
// deliberately left untouched by this round (the brief scoped changes to
// Delete and Restore only); see AGENTS.md's Known limitations for the
// documented guidance to future callers.
func TestRepository_Update_StaleModelAfterSoftDelete_SilentlyClearsDeletedAt(t *testing.T) {
	repo := newSoftDeletableWidgetRepo(t)
	ctx := ctxTenantActor("tenant-a", "user-1")

	w := &testutil.SoftDeletableWidget{ID: "w1", Name: "gadget"}
	if err := repo.Create(ctx, w); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	// stale is a copy of w's state as it was BEFORE the Delete below --
	// exactly what a caller would be holding if it read the row, then
	// raced (or simply came later) with someone else's Delete.
	stale := *w

	if err := repo.Delete(ctx, w.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if found, deletedAt, _ := rawSoftDeletableWidgetRow(t, repo.db, w.ID); !found || deletedAt == nil {
		t.Fatalf("row after Delete(): found=%v deletedAt=%v, want found with a populated deleted_at", found, deletedAt)
	}

	// Update's contract is unchanged by this round: full-record save, no
	// deleted_at awareness. Writing the stale copy (DeletedAt == nil,
	// DeletedBy == "") back over the row clears the very columns Delete
	// just set.
	staleCopy := stale
	if err := repo.Update(ctx, &staleCopy); err != nil {
		t.Fatalf("Update() with a stale pre-delete model error = %v", err)
	}

	found, deletedAt, deletedBy := rawSoftDeletableWidgetRow(t, repo.db, w.ID)
	if !found {
		t.Fatal("row missing after Update(), want it present")
	}
	if deletedAt != nil {
		t.Errorf("deleted_at after Update() with a stale pre-delete model = %v, want nil -- this is the documented, un-fixed hazard: Update silently 'undeletes' the row", deletedAt)
	}
	if deletedBy != "" {
		t.Errorf("deleted_by after Update() with a stale pre-delete model = %q, want empty -- same documented hazard", deletedBy)
	}
}
