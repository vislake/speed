package tenancy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
)

// This file exercises the literal end-to-end shape the tenancy module's
// three pieces are meant to support together: an http.Handler wrapped in
// Middleware that, within the same request, also calls WithSystemContext --
// something middleware_test.go and system_context_test.go each cover in
// isolation but neither can observe once composed through a real
// net/http round trip (httptest.NewRequest/NewRecorder), including the
// audit event WithSystemContext publishes.
//
// TestMiddlewareThenSystemContext_ResolvedTenant_AuditCorrelatesIt covers a
// request that DOES resolve a tenant: Middleware injects it, the handler
// additionally elevates to system context (e.g. an admin action that needs
// to be audited while still working one tenant's data), and the resulting
// audit event must correlate that same tenant -- proving the two pieces
// hand off cleanly across the net/http boundary, not merely across a bare
// context.Context value in a unit test.
func TestMiddlewareThenSystemContext_ResolvedTenant_AuditCorrelatesIt(t *testing.T) {
	const purpose pkgcore.SystemPurpose = "tenancy_test.middleware_system_context.resolved"
	pkgcore.RegisterSystemPurpose(purpose)

	const tenant = pkgcore.TenantID("acme-corp")
	bus := pkgcore.NewMemoryEventBus()
	var published []pkgcore.Event
	bus.Subscribe(EventSystemContextEntered, func(_ context.Context, evt pkgcore.Event) error {
		published = append(published, evt)
		return nil
	})

	var (
		handlerRan    bool
		elevateErr    error
		sawTenantOK   bool
		sawTenant     pkgcore.TenantID
		sawSystemOK   bool
		handlerStatus int
	)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerRan = true
		// A handler that resolved a normal per-request tenant via Middleware
		// additionally needing the system-context escape hatch mid-request
		// -- e.g. a support action ("resend this tenant's last webhook")
		// that must be audited as an elevated action even though it never
		// leaves the tenant it was already scoped to.
		elevated, err := WithSystemContext(r.Context(), bus, pkgcore.SystemReason{
			Actor: "support-agent@example.com", Purpose: purpose, Ticket: "SUP-42",
		})
		elevateErr = err
		sawTenant, sawTenantOK = pkgcore.TenantFromContext(elevated)
		_, sawSystemOK = pkgcore.SystemReasonFromContext(elevated)
		handlerStatus = http.StatusOK
		w.WriteHeader(handlerStatus)
	})

	mw := Middleware(stubResolver{tenant: tenant})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/support/resend-webhook", nil)
	rec := httptest.NewRecorder()
	mw(handler).ServeHTTP(rec, req)

	if !handlerRan {
		t.Fatal("handler did not run; Middleware should have resolved the tenant successfully")
	}
	if elevateErr != nil {
		t.Fatalf("WithSystemContext inside the handler error = %v, want success", elevateErr)
	}
	if !sawTenantOK || sawTenant != tenant {
		t.Errorf("after WithSystemContext, TenantFromContext = (%q, %v), want (%q, true); the Middleware-resolved tenant must survive elevation", sawTenant, sawTenantOK, tenant)
	}
	if !sawSystemOK {
		t.Error("after WithSystemContext, SystemReasonFromContext reported no reason")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("response status = %d, want %d", rec.Code, http.StatusOK)
	}

	if len(published) != 1 {
		t.Fatalf("audit events published = %d, want 1", len(published))
	}
	payload, ok := published[0].Payload.(SystemContextEnteredEvent)
	if !ok {
		t.Fatalf("event.Payload = %#v, want SystemContextEnteredEvent", published[0].Payload)
	}
	if published[0].TenantID != tenant {
		t.Errorf("event.TenantID = %q, want %q -- the audit trail must correlate the Middleware-resolved tenant, not come back empty", published[0].TenantID, tenant)
	}
	if payload.Actor != "support-agent@example.com" {
		t.Errorf("event.Payload.Actor = %q, want %q", payload.Actor, "support-agent@example.com")
	}
}

// TestMiddlewareThenSystemContext_AllowlistedNoTenant_GrantsWithNoCorrelation
// covers the other reachable shape: an allowlisted path (per WithAllowlist)
// where the Resolver fails, so Middleware proceeds with NO tenant in
// context at all -- a genuine cross-tenant admin endpoint might be
// allowlisted exactly for this reason. The handler still successfully
// elevates to system context (WithSystemContext never requires a tenant),
// and the audit event's TenantID stays at its zero value rather than
// panicking or fabricating one. It then additionally confirms the gap
// system_context_repository_test.go documents shows up here too, in a real
// request: even fully elevated, a Repository[T] call this handler makes
// still fails closed with pkgcore.ErrNoTenant, because nothing in this
// request ever supplied a tenant and the escape hatch does not substitute
// for one on Repository[T]'s path.
func TestMiddlewareThenSystemContext_AllowlistedNoTenant_GrantsWithNoCorrelation(t *testing.T) {
	const purpose pkgcore.SystemPurpose = "tenancy_test.middleware_system_context.allowlisted"
	pkgcore.RegisterSystemPurpose(purpose)

	const path = "/api/v1/admin/tenant-search"
	bus := pkgcore.NewMemoryEventBus()
	var published []pkgcore.Event
	bus.Subscribe(EventSystemContextEntered, func(_ context.Context, evt pkgcore.Event) error {
		published = append(published, evt)
		return nil
	})

	db := dbtest.NewSQLite(t)
	repo := newSysCtxWidgetRepo(t, db)

	var (
		handlerRan      bool
		elevateErr      error
		sawTenantOK     bool
		repoErr         error
		resolverFailure = errors.New("no tenant: this is a cross-tenant admin route")
	)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerRan = true
		elevated, err := WithSystemContext(r.Context(), bus, pkgcore.SystemReason{
			Actor: "platform-admin@example.com", Purpose: purpose, Ticket: "SUP-99",
		})
		elevateErr = err
		_, sawTenantOK = pkgcore.TenantFromContext(elevated)

		_, repoErr = repo.List(elevated)
		w.WriteHeader(http.StatusOK)
	})

	mw := Middleware(stubResolver{err: resolverFailure}, WithAllowlist(http.MethodGet, path))
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	mw(handler).ServeHTTP(rec, req)

	if !handlerRan {
		t.Fatal("handler did not run; the allowlisted path should proceed despite the resolver failure")
	}
	if elevateErr != nil {
		t.Fatalf("WithSystemContext on a no-tenant context error = %v, want success -- a system reason must be grantable with no tenant at all", elevateErr)
	}
	if sawTenantOK {
		t.Error("TenantFromContext reported a tenant present after an allowlisted no-tenant request; WithSystemContext must not fabricate one")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("response status = %d, want %d", rec.Code, http.StatusOK)
	}

	if len(published) != 1 {
		t.Fatalf("audit events published = %d, want 1", len(published))
	}
	if published[0].TenantID != "" {
		t.Errorf("event.TenantID = %q, want empty; an allowlisted no-tenant request has nothing to correlate", published[0].TenantID)
	}

	// The gap documented in system_context_repository_test.go, confirmed
	// once more through this file's real net/http request rather than a
	// bare context.Context: a granted system reason does not let this
	// handler's Repository[T] call read across tenants, or run at all,
	// despite the elevation having genuinely succeeded above.
	if !errors.Is(repoErr, pkgcore.ErrNoTenant) {
		t.Errorf("repo.List(elevated, no-tenant) error = %v, want errors.Is(err, pkgcore.ErrNoTenant); confirms WithSystemContext grants no Repository[T] bypass even for an allowlisted, intentionally cross-tenant route", repoErr)
	}
}
