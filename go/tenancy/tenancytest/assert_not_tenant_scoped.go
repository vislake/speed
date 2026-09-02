package tenancytest

import (
	"context"
	"reflect"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// tenantScopedType is the reflect.Type of dbkit.TenantScoped. Comparing
// against it reflectively, rather than with a single plain type assertion,
// lets implementsTenantScoped answer correctly regardless of whether model
// was handed in as a value or a pointer relative to which one its
// GetTenantID method is actually declared on — see implementsTenantScoped.
var tenantScopedType = reflect.TypeOf((*dbkit.TenantScoped)(nil)).Elem()

// probeTenantX and probeTenantY are two arbitrary tenant ids AssertNotTenantScoped
// uses only to prove that findFn's result does not depend on which one (or
// neither) happens to be in the calling context. Nothing is ever created
// "for" either of them specifically — that is the entire point: a genuinely
// non-tenant-scoped model has no such thing as data belonging to one tenant
// versus another.
const (
	probeTenantX = pkgcore.TenantID("tenancytest-not-scoped-probe-x")
	probeTenantY = pkgcore.TenantID("tenancytest-not-scoped-probe-y")
)

// AssertNotTenantScoped verifies the OPPOSITE and equally important property
// from AssertIsolated: a model that must NOT be tenant-scoped (identity or
// platform data, per docs/internal/04-data-and-tenancy.md's data-domain
// table) is genuinely unaffected by dbkit's isolation plugin when queried
// through a plain *gorm.DB from dbkit.Open — i.e. confirms the model does
// NOT (accidentally or otherwise) implement dbkit.TenantScoped, and that
// querying/writing it works identically regardless of what tenant (if any)
// is in the calling context. A model that should be globally visible but got
// accidentally scoped shows up as "the data mysteriously disappeared" in
// production — this is the regression test that catches that class of bug
// before it ships.
//
// model is used only to answer that first question — is this Go type
// tenant-scoped at all — and is never queried directly; AssertNotTenantScoped
// has no way to know model's schema (identity and platform data carries no
// required shape the way dbkit.Repository[T]'s "ID"/"TenantID" convention
// does), so createFn and findFn carry the actual GORM calls instead. A
// typical findFn is a plain row count:
//
//	createFn := func(db *gorm.DB) error {
//	    return db.Create(&PlatformPlan{ID: newULID(), Name: "pro"}).Error
//	}
//	findFn := func(db *gorm.DB) (int64, error) {
//	    var n int64
//	    err := db.Model(&PlatformPlan{}).Count(&n).Error
//	    return n, err
//	}
//	tenancytest.AssertNotTenantScoped(t, db, PlatformPlan{}, createFn, findFn)
//
// AssertNotTenantScoped calls createFn and findFn several times, against db
// sessions carrying no tenant and carrying each of two arbitrary tenants in
// turn, and requires findFn's count to move in lockstep with createFn calls
// regardless of which — proving visibility is not tied to any particular
// tenant context, not merely that it happens to work under the one or two
// contexts a less thorough test might have tried. It never assumes db's
// table starts empty: every comparison is a delta against a freshly measured
// baseline, so AssertNotTenantScoped is safe to call against a table that
// already has rows in it.
//
// db is expected to come from dbkit.Open (directly, or through
// github.com/vislake/speed/go/dbkit/dbtest), with model's table already
// migrated — see that package's own doc comment for why neither it nor
// AssertNotTenantScoped applies a migration itself. createFn and findFn
// receive db.WithContext-derived sessions, never db itself, so they must
// read whatever tenant is current from the *gorm.DB they are given (via
// pkgcore.TenantFromContext(db.Statement.Context), the same mechanism
// dbkit's own tenant-scoping plugin uses) rather than from a captured
// variable, if they need it at all — a correctly non-tenant-scoped
// findFn/createFn pair does not need it for anything.
func AssertNotTenantScoped(t *testing.T, db *gorm.DB, model any, createFn func(db *gorm.DB) error, findFn func(db *gorm.DB) (int64, error)) {
	t.Helper()

	if implementsTenantScoped(model) {
		t.Fatalf("model %T implements dbkit.TenantScoped; AssertNotTenantScoped is for identity/platform data that must NOT be tenant-scoped — use AssertIsolated instead", model)
	}

	noTenant := db.WithContext(context.Background())
	withX := db.WithContext(pkgcore.WithTenant(context.Background(), probeTenantX))
	withY := db.WithContext(pkgcore.WithTenant(context.Background(), probeTenantY))

	baseline, err := findFn(noTenant)
	if err != nil {
		t.Fatalf("findFn(no tenant in context) baseline call error = %v", err)
	}

	if err = createFn(noTenant); err != nil {
		t.Fatalf("createFn(no tenant in context) error = %v, want success — a genuinely non-tenant-scoped model must never require a tenant in context to be written", err)
	}

	afterFirstCreate, err := findFn(withX)
	if err != nil {
		t.Fatalf("findFn(tenant %q in context) error = %v", probeTenantX, err)
	}
	if want := baseline + 1; afterFirstCreate != want {
		t.Errorf("findFn(tenant %q in context) = %d, want %d — a row created with no tenant in context must be visible under an arbitrary tenant", probeTenantX, afterFirstCreate, want)
	}

	if err = createFn(withY); err != nil {
		t.Fatalf("createFn(tenant %q in context) error = %v", probeTenantY, err)
	}

	afterSecondCreate, err := findFn(noTenant)
	if err != nil {
		t.Fatalf("findFn(no tenant in context) error = %v (after the second create)", err)
	}
	if want := baseline + 2; afterSecondCreate != want {
		t.Errorf("findFn(no tenant in context) = %d, want %d — a row created under tenant %q must remain visible with no tenant in context at all", afterSecondCreate, want, probeTenantY)
	}

	finalUnderX, err := findFn(withX)
	if err != nil {
		t.Fatalf("findFn(tenant %q in context) error = %v (final check)", probeTenantX, err)
	}
	if finalUnderX != afterSecondCreate {
		t.Errorf("findFn result depends on which tenant is in context: %d under tenant %q versus %d under none; a genuinely non-tenant-scoped model must return the identical result regardless of what tenant, if any, is in context", finalUnderX, probeTenantX, afterSecondCreate)
	}
}

// implementsTenantScoped reports whether model's type implements
// dbkit.TenantScoped. It checks model as given first, then unwraps any
// number of pointer indirections and checks both that unwrapped type and a
// pointer to it — the same double check dbkit's own tenant-scoping plugin
// performs internally (see tenant_scope.go's isTenantScopedValue) — because
// GetTenantID is conventionally declared on a value receiver (see dbkit's
// TenantModel and internal/testutil.Widget) while model might be handed to
// AssertNotTenantScoped as a pointer, or vice versa, and this check must
// answer correctly either way rather than only for whichever shape the
// caller happened to pass.
func implementsTenantScoped(model any) bool {
	if model == nil {
		return false
	}
	if _, ok := model.(dbkit.TenantScoped); ok {
		return true
	}

	t := reflect.TypeOf(model)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Implements(tenantScopedType) || reflect.PointerTo(t).Implements(tenantScopedType)
}
