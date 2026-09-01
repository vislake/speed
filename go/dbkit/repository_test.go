package dbkit

import (
	"context"
	"errors"
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
