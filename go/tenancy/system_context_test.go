package tenancy

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// errAuditSinkUnavailable stands in for a subscriber failure such as a down
// audit-log sink. Subscribing a handler that always returns it is how these
// tests force the real pkgcore.EventBus.Publish to fail, instead of writing a
// bespoke EventBus implementation -- per pkgcore/AGENTS.md: "Do not write a
// mock for KVStore or EventBus. NewMemoryEventBus and NewMemoryKVStore are
// the test doubles."
var errAuditSinkUnavailable = errors.New("audit sink unavailable")

// TestWithSystemContext_Success covers the happy path: the bus receives
// exactly one audit event carrying the reason's Actor/Purpose/Ticket, and the
// returned context is genuinely elevated. It also covers how the event's
// TenantID is populated: correlated from the parent context's tenant when
// there is one, left at its zero value otherwise.
func TestWithSystemContext_Success(t *testing.T) {
	const (
		purposeNoTenant   pkgcore.SystemPurpose = "tenancy_test.system_context_success.no_tenant"
		purposeWithTenant pkgcore.SystemPurpose = "tenancy_test.system_context_success.with_tenant"
	)
	pkgcore.RegisterSystemPurpose(purposeNoTenant)
	pkgcore.RegisterSystemPurpose(purposeWithTenant)

	tests := []struct {
		name       string
		parent     context.Context
		reason     pkgcore.SystemReason
		wantTenant pkgcore.TenantID // zero value means the event carries no tenant
	}{
		{
			name:   "no tenant on the parent context",
			parent: context.Background(),
			reason: pkgcore.SystemReason{Actor: "admin@example.com", Purpose: purposeNoTenant, Ticket: "SUP-1234"},
		},
		{
			name:   "ticket is optional",
			parent: context.Background(),
			reason: pkgcore.SystemReason{Actor: "jobs-worker", Purpose: purposeNoTenant},
		},
		{
			name:       "tenant already on the parent context is correlated onto the event",
			parent:     pkgcore.WithTenant(context.Background(), pkgcore.TenantID("acme")),
			reason:     pkgcore.SystemReason{Actor: "admin@example.com", Purpose: purposeWithTenant, Ticket: "SUP-5678"},
			wantTenant: pkgcore.TenantID("acme"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bus := pkgcore.NewMemoryEventBus()
			var received []pkgcore.Event
			bus.Subscribe(EventSystemContextEntered, func(_ context.Context, evt pkgcore.Event) error {
				received = append(received, evt)
				return nil
			})

			before := time.Now()
			ctx, err := WithSystemContext(tt.parent, bus, tt.reason)
			after := time.Now()
			if err != nil {
				t.Fatalf("WithSystemContext returned unexpected error: %v", err)
			}

			// The returned context is genuinely elevated, and the parent is
			// untouched.
			gotReason, ok := pkgcore.SystemReasonFromContext(ctx)
			if !ok {
				t.Fatal("SystemReasonFromContext reported no reason after a successful WithSystemContext")
			}
			if gotReason != tt.reason {
				t.Errorf("SystemReasonFromContext = %+v, want %+v", gotReason, tt.reason)
			}
			if _, parentElevated := pkgcore.SystemReasonFromContext(tt.parent); parentElevated {
				t.Error("WithSystemContext mutated the parent context")
			}

			// Exactly one audit event was published, with the right shape.
			if len(received) != 1 {
				t.Fatalf("bus received %d events, want exactly 1: %+v", len(received), received)
			}
			evt := received[0]
			if evt.Type != EventSystemContextEntered {
				t.Errorf("event.Type = %q, want %q", evt.Type, EventSystemContextEntered)
			}
			if evt.TenantID != tt.wantTenant {
				t.Errorf("event.TenantID = %q, want %q", evt.TenantID, tt.wantTenant)
			}

			payload, ok := evt.Payload.(SystemContextEnteredEvent)
			if !ok {
				t.Fatalf("event.Payload = %#v (%T), want a SystemContextEnteredEvent", evt.Payload, evt.Payload)
			}
			if payload.Actor != tt.reason.Actor {
				t.Errorf("payload.Actor = %q, want %q", payload.Actor, tt.reason.Actor)
			}
			if payload.Purpose != tt.reason.Purpose {
				t.Errorf("payload.Purpose = %q, want %q", payload.Purpose, tt.reason.Purpose)
			}
			if payload.Ticket != tt.reason.Ticket {
				t.Errorf("payload.Ticket = %q, want %q", payload.Ticket, tt.reason.Ticket)
			}
			if payload.EnteredAt.Before(before) || payload.EnteredAt.After(after) {
				t.Errorf("payload.EnteredAt = %v, want between %v and %v", payload.EnteredAt, before, after)
			}
		})
	}
}

// TestWithSystemContext_RejectedByPkgcore_NoEventPublished covers reasons
// pkgcore.WithSystemContext itself rejects (an unregistered Purpose, or an
// empty Actor): WithSystemContext must fail the same way, with the returned
// context left unelevated, and it must not publish anything -- nothing was
// ever elevated, so there is nothing to audit.
func TestWithSystemContext_RejectedByPkgcore_NoEventPublished(t *testing.T) {
	const registeredPurpose pkgcore.SystemPurpose = "tenancy_test.system_context_rejected.registered"
	pkgcore.RegisterSystemPurpose(registeredPurpose)

	tests := []struct {
		name    string
		reason  pkgcore.SystemReason
		wantErr error
	}{
		{
			name: "unregistered purpose",
			reason: pkgcore.SystemReason{
				Actor:   "admin@example.com",
				Purpose: pkgcore.SystemPurpose("tenancy_test.system_context_rejected.never_registered"),
			},
			wantErr: pkgcore.ErrSystemPurposeNotRegistered,
		},
		{
			name:    "empty actor",
			reason:  pkgcore.SystemReason{Purpose: registeredPurpose},
			wantErr: pkgcore.ErrSystemActorRequired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bus := pkgcore.NewMemoryEventBus()
			var publishCalls int
			bus.Subscribe(EventSystemContextEntered, func(_ context.Context, _ pkgcore.Event) error {
				publishCalls++
				return nil
			})

			parent := context.Background()
			ctx, err := WithSystemContext(parent, bus, tt.reason)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("WithSystemContext error = %v, want one matching %v", err, tt.wantErr)
			}
			if ctx != parent {
				t.Error("WithSystemContext did not return the original context unchanged on failure")
			}
			if _, ok := pkgcore.SystemReasonFromContext(ctx); ok {
				t.Error("a rejected WithSystemContext still elevated the returned context")
			}
			if publishCalls != 0 {
				t.Errorf("bus received %d events, want 0: nothing was elevated, so there was nothing to audit", publishCalls)
			}
		})
	}
}

// TestWithSystemContext_PublishFails_FailsClosedWithoutElevatingContext is
// the core fail-closed guarantee this wrapper exists for: pkgcore's own
// WithSystemContext succeeds, but the audit publish itself fails (a
// subscriber -- e.g. the audit-log sink -- returns an error). WithSystemContext
// must not return the already-elevated context in that case: a granted escape
// hatch with no audit record is exactly the gap this wrapper closes.
func TestWithSystemContext_PublishFails_FailsClosedWithoutElevatingContext(t *testing.T) {
	const purpose pkgcore.SystemPurpose = "tenancy_test.system_context_publish_fails"
	pkgcore.RegisterSystemPurpose(purpose)
	reason := pkgcore.SystemReason{Actor: "admin@example.com", Purpose: purpose, Ticket: "SUP-9999"}

	// A real in-memory bus (per pkgcore/AGENTS.md, EventBus must not be
	// mocked) whose one subscriber always fails, so Publish itself returns a
	// non-nil error.
	bus := pkgcore.NewMemoryEventBus()
	var publishCalls int
	bus.Subscribe(EventSystemContextEntered, func(_ context.Context, _ pkgcore.Event) error {
		publishCalls++
		return errAuditSinkUnavailable
	})

	parent := context.Background()
	ctx, err := WithSystemContext(parent, bus, reason)

	if err == nil {
		t.Fatal("WithSystemContext returned a nil error despite the audit publish failing")
	}
	if publishCalls != 1 {
		t.Fatalf("bus subscriber invoked %d times, want exactly 1", publishCalls)
	}

	// The failure is reported through the structured tenancy error. Because
	// WithParam/WithCause always derive a new *apperr.Error rather than
	// mutating the receiver, the package-level sentinel must be matched by
	// .Code via apperr.As, not errors.Is/== -- see ErrAuditPublishFailed's own
	// doc comment.
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("WithSystemContext error = %v (%T), want an *apperr.Error", err, err)
	}
	if appErr.Code != ErrAuditPublishFailed.Code {
		t.Errorf("error code = %q, want %q", appErr.Code, ErrAuditPublishFailed.Code)
	}
	// The underlying subscriber failure must still be reachable, for logs and
	// errors.Is-based diagnosis.
	if !errors.Is(err, errAuditSinkUnavailable) {
		t.Errorf("WithSystemContext error does not wrap the publish failure: %v", err)
	}

	// The crux of the fail-closed requirement: the escape hatch must NOT have
	// been granted on the returned context, which must be the original,
	// unelevated one.
	if gotReason, ok := pkgcore.SystemReasonFromContext(ctx); ok {
		t.Errorf("WithSystemContext elevated the returned context despite the audit publish failing: %+v", gotReason)
	}
	if ctx != parent {
		t.Error("WithSystemContext did not return the original context unchanged on publish failure")
	}
}

// TestWithSystemContext_RepeatedCalls_OneEventPerCall proves repeated use
// produces exactly one audit event per call -- never zero (a dropped audit
// record) and never more than one (a duplicated record inflating the trail).
func TestWithSystemContext_RepeatedCalls_OneEventPerCall(t *testing.T) {
	const purpose pkgcore.SystemPurpose = "tenancy_test.system_context_repeated"
	pkgcore.RegisterSystemPurpose(purpose)

	bus := pkgcore.NewMemoryEventBus()
	var received []pkgcore.Event
	bus.Subscribe(EventSystemContextEntered, func(_ context.Context, evt pkgcore.Event) error {
		received = append(received, evt)
		return nil
	})

	reasons := []pkgcore.SystemReason{
		{Actor: "admin-1@example.com", Purpose: purpose, Ticket: "SUP-1"},
		{Actor: "admin-2@example.com", Purpose: purpose, Ticket: "SUP-2"},
		{Actor: "jobs-worker", Purpose: purpose},
	}

	for i, reason := range reasons {
		if _, err := WithSystemContext(context.Background(), bus, reason); err != nil {
			t.Fatalf("call %d: WithSystemContext returned unexpected error: %v", i, err)
		}

		if len(received) != i+1 {
			t.Fatalf("after call %d: bus received %d events total, want %d (one per call, no drops or duplicates)",
				i, len(received), i+1)
		}

		payload, ok := received[i].Payload.(SystemContextEnteredEvent)
		if !ok {
			t.Fatalf("call %d: event.Payload = %#v, want a SystemContextEnteredEvent", i, received[i].Payload)
		}
		if payload.Actor != reason.Actor || payload.Ticket != reason.Ticket {
			t.Errorf("call %d: payload = %+v, want it to match reason %+v", i, payload, reason)
		}
	}

	if len(received) != len(reasons) {
		t.Fatalf("total events received = %d, want %d", len(received), len(reasons))
	}
}

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
