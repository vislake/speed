package tenancy

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// This file investigates a question the unit tests for Middleware and
// WithSystemContext, taken separately, cannot answer: what actually happens
// when a context carrying a tenant (as Middleware produces) and a context
// carrying a granted system reason (as WithSystemContext produces) are
// combined, and that combined context is then handed to the ONE sanctioned
// data-access path, dbkit.Repository[T] (backend coding standard §3.2)?
//
// The short answer, proved below: WithSystemContext and dbkit.Repository[T]
// do not interact at all yet. WithSystemContext only ever adds a
// SystemReason value to the context; it never removes, replaces, or
// otherwise touches whatever tenant that context already carried (see its
// own doc comment: "a system context is orthogonal to a tenant context").
// dbkit.Repository[T]'s methods, in turn, never consult
// pkgcore.SystemReasonFromContext at all -- every method resolves the
// tenant with pkgcore.MustTenantFromContext and, when one is present, scopes
// its query to exactly that tenant regardless of any system reason also
// present; when none is present, every method fails closed with
// pkgcore.ErrNoTenant regardless of any system reason also present. dbkit's
// own tenant_scope.go documents this as a deliberate, temporary gap: "It
// also does not implement the system-context cross-tenant escape hatch
// ([...]) Routing an authorized cross-tenant admin or job query around
// tenant filtering is left to a higher layer (expected to be
// dbkit.Repository[T])" -- but Repository[T], as it stands, does not
// implement that either. So today, composing WithSystemContext with
// Repository[T] is inert: it changes nothing about which rows a query can
// see, in either direction. A caller reaching for WithSystemContext
// expecting it to unlock a cross-tenant Repository[T] read (an admin
// tenant-search feature, say) would find it does not, with no error to
// signal that expectation was wrong -- which is exactly why this composition
// is worth a standing test, not just a one-off investigation: if a future
// change wires the escape hatch into Repository[T], the "still scoped to
// tenant A" and "still fails with ErrNoTenant" assertions below must be
// deliberately updated, not silently broken.
//
// sysCtxWidget is a minimal tenant-scoped fixture local to this file --
// see sprocket's doc comment in tenancytest/assert_isolated_test.go for why
// every package that needs one defines its own rather than sharing dbkit's
// unexported internal/testutil.Widget.
type sysCtxWidget struct {
	ID       string `gorm:"column:id;primaryKey;size:64"`
	TenantID string `gorm:"column:tenant_id;primaryKey;size:64"`
}

func (w sysCtxWidget) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(w.TenantID) }

var _ dbkit.TenantScoped = sysCtxWidget{}

const createSysCtxWidgetsTableSQL = `CREATE TABLE sys_ctx_widgets (
	id VARCHAR(64) NOT NULL,
	tenant_id VARCHAR(64) NOT NULL,
	PRIMARY KEY (tenant_id, id)
)`

var sysCtxWidgetIDSeq atomic.Uint64

func newSysCtxWidgetRepo(t *testing.T, db *gorm.DB) *dbkit.Repository[sysCtxWidget] {
	t.Helper()
	if err := db.Exec(createSysCtxWidgetsTableSQL).Error; err != nil {
		t.Fatalf("create sys_ctx_widgets table: %v", err)
	}
	return dbkit.NewRepository[sysCtxWidget](db)
}

func newSysCtxWidget(tenant pkgcore.TenantID) *sysCtxWidget {
	return &sysCtxWidget{ID: fmt.Sprintf("w-%d", sysCtxWidgetIDSeq.Add(1)), TenantID: string(tenant)}
}

// TestSystemContext_ComposesWithTenantContext_WithoutClobberingEither proves
// the two context values genuinely coexist: elevating an already-tenant-
// scoped context (the shape Middleware hands a handler) neither drops the
// tenant nor is prevented by its presence, and the reverse read order works
// too. This is the "compose" half of the investigation; the next two tests
// cover what that composed context actually does (or, mostly, does not do)
// once it reaches Repository[T].
func TestSystemContext_ComposesWithTenantContext_WithoutClobberingEither(t *testing.T) {
	const purpose pkgcore.SystemPurpose = "tenancy_test.system_context_repository.compose"
	pkgcore.RegisterSystemPurpose(purpose)

	bus := pkgcore.NewMemoryEventBus()
	tenantA := pkgcore.TenantID("tenant-a")
	baseCtx := pkgcore.WithTenant(context.Background(), tenantA)

	elevated, err := WithSystemContext(baseCtx, bus, pkgcore.SystemReason{
		Actor: "admin@example.com", Purpose: purpose, Ticket: "SUP-1",
	})
	if err != nil {
		t.Fatalf("WithSystemContext error = %v, want success", err)
	}

	gotTenant, ok := pkgcore.TenantFromContext(elevated)
	if !ok || gotTenant != tenantA {
		t.Errorf("TenantFromContext(elevated) = (%q, %v), want (%q, true); WithSystemContext must not disturb an existing tenant", gotTenant, ok, tenantA)
	}
	if _, ok := pkgcore.SystemReasonFromContext(elevated); !ok {
		t.Error("SystemReasonFromContext(elevated) reported no reason after a successful WithSystemContext")
	}
}

// TestSystemContext_DoesNotWidenRepositoryVisibilityBeyondTheContextTenant
// is the key negative result: holding a validly-granted system reason,
// alongside a real tenant, changes nothing about what Repository[T] returns.
// List still returns exactly tenant A's rows; FindByID against tenant B's id
// is still denied. The escape hatch is not "partially" respected here --
// it has no effect at all on this path.
func TestSystemContext_DoesNotWidenRepositoryVisibilityBeyondTheContextTenant(t *testing.T) {
	const purpose pkgcore.SystemPurpose = "tenancy_test.system_context_repository.no_widen"
	pkgcore.RegisterSystemPurpose(purpose)

	db := dbtest.NewSQLite(t)
	repo := newSysCtxWidgetRepo(t, db)
	bus := pkgcore.NewMemoryEventBus()

	tenantA := pkgcore.TenantID("tenancytest-syswiden-a")
	tenantB := pkgcore.TenantID("tenancytest-syswiden-b")
	ctxA := pkgcore.WithTenant(context.Background(), tenantA)
	ctxB := pkgcore.WithTenant(context.Background(), tenantB)

	recA := newSysCtxWidget(tenantA)
	if err := repo.Create(ctxA, recA); err != nil {
		t.Fatalf("Create(tenant A) error = %v", err)
	}
	recB := newSysCtxWidget(tenantB)
	if err := repo.Create(ctxB, recB); err != nil {
		t.Fatalf("Create(tenant B) error = %v", err)
	}

	elevatedA, err := WithSystemContext(ctxA, bus, pkgcore.SystemReason{
		Actor: "admin@example.com", Purpose: purpose, Ticket: "SUP-2",
	})
	if err != nil {
		t.Fatalf("WithSystemContext(tenant A context) error = %v, want success", err)
	}

	rows, err := repo.List(elevatedA)
	if err != nil {
		t.Fatalf("List(system-context-elevated tenant A) error = %v", err)
	}
	if len(rows) != 1 || rows[0].ID != recA.ID {
		t.Errorf("List(system-context-elevated tenant A) = %+v, want exactly tenant A's own single row (%q); a system reason must not widen Repository[T] visibility to other tenants", rows, recA.ID)
	}

	if got, err := repo.FindByID(elevatedA, recB.ID); !isSysCtxRecordNotFound(err) {
		t.Errorf("FindByID(system-context-elevated tenant A, tenant B's id) = (%v, %v), want (nil, dbkit.ErrRecordNotFound); a system reason granted while scoped to tenant A must not unlock tenant B's row", got, err)
	}
}

// TestSystemContext_WithoutTenant_StillFailsClosedOnRepository covers the
// scenario an admin cross-tenant search or a background job would actually
// want: a context that carries a granted system reason but NO tenant at
// all, used to attempt a Repository[T] read that is meant to span every
// tenant. It still fails closed with pkgcore.ErrNoTenant, exactly as if no
// system reason had ever been granted -- proving the escape hatch, as wired
// today, grants no read capability through the one sanctioned data-access
// path at all. Reaching cross-tenant data legitimately today requires
// bypassing Repository[T] for the raw-SQL escape hatch documented in
// backend-coding-standards §3.2, not WithSystemContext plus Repository[T].
func TestSystemContext_WithoutTenant_StillFailsClosedOnRepository(t *testing.T) {
	const purpose pkgcore.SystemPurpose = "tenancy_test.system_context_repository.no_tenant"
	pkgcore.RegisterSystemPurpose(purpose)

	db := dbtest.NewSQLite(t)
	repo := newSysCtxWidgetRepo(t, db)
	bus := pkgcore.NewMemoryEventBus()

	elevatedNoTenant, err := WithSystemContext(context.Background(), bus, pkgcore.SystemReason{
		Actor: "jobs-worker", Purpose: purpose, Ticket: "SUP-3",
	})
	if err != nil {
		t.Fatalf("WithSystemContext(no tenant) error = %v, want success -- a system reason must be grantable with no tenant in context at all", err)
	}
	if _, ok := pkgcore.SystemReasonFromContext(elevatedNoTenant); !ok {
		t.Fatal("SystemReasonFromContext(elevatedNoTenant) reported no reason; test setup is broken")
	}

	if _, err := repo.List(elevatedNoTenant); !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Errorf("List(system-context-elevated, no tenant) error = %v, want errors.Is(err, pkgcore.ErrNoTenant); a granted system reason must not substitute for a tenant on Repository[T]'s sanctioned path", err)
	}
	rec := newSysCtxWidget("irrelevant-tenant")
	if err := repo.Create(elevatedNoTenant, rec); !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Errorf("Create(system-context-elevated, no tenant) error = %v, want errors.Is(err, pkgcore.ErrNoTenant)", err)
	}
}

// isSysCtxRecordNotFound mirrors tenancytest's own isRecordNotFound: dbkit's
// ErrRecordNotFound must be matched by Code, never by identity, since
// apperr's WithParam/WithCause always derive a new *apperr.Error rather than
// mutate the receiver.
func isSysCtxRecordNotFound(err error) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == dbkit.ErrRecordNotFound.Code
}
