package tenancy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
)

// stubResolver is a Resolver test double whose behavior is fixed at
// construction, so Middleware can be exercised without depending on
// DomainResolver or on the authenticated-request Resolver that authn will
// eventually supply.
type stubResolver struct {
	tenant pkgcore.TenantID
	err    error
}

func (s stubResolver) Resolve(*http.Request) (pkgcore.TenantID, error) {
	return s.tenant, s.err
}

// recordingHandler is the downstream http.Handler double used to observe
// whether Middleware called the next handler and, if so, what tenant (if
// any) it found in the request context.
type recordingHandler struct {
	called      bool
	sawTenant   pkgcore.TenantID
	sawTenantOK bool
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.called = true
	h.sawTenant, h.sawTenantOK = pkgcore.TenantFromContext(r.Context())
	w.WriteHeader(http.StatusOK)
}

func TestMiddleware_Success_InjectsResolvedTenantIntoContext(t *testing.T) {
	tests := []struct {
		name   string
		tenant pkgcore.TenantID
	}{
		{name: "ulid style id", tenant: pkgcore.TenantID("01HQ8Z3XK9V0T7M2P5RGBW4NCD")},
		{name: "slug style id", tenant: pkgcore.TenantID("acme-corp")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := &recordingHandler{}
			mw := Middleware(stubResolver{tenant: tt.tenant})

			req := httptest.NewRequest(http.MethodGet, "/api/v1/billing/plans", nil)
			rec := httptest.NewRecorder()
			mw(handler).ServeHTTP(rec, req)

			if !handler.called {
				t.Fatal("next handler was not called on successful resolution")
			}
			if !handler.sawTenantOK {
				t.Fatal("pkgcore.TenantFromContext reported no tenant in the downstream request context")
			}
			if handler.sawTenant != tt.tenant {
				t.Errorf("downstream tenant = %q, want %q", handler.sawTenant, tt.tenant)
			}
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
			}
		})
	}
}

func TestMiddleware_ResolutionFailure(t *testing.T) {
	resolveErr := errors.New("resolver deliberately failed")

	tests := []struct {
		name            string
		opts            []MiddlewareOption
		path            string
		wantHandlerCall bool
		wantStatus      int
	}{
		{
			name:            "no allowlist configured: rejected with 403, handler never runs",
			opts:            nil,
			path:            "/api/v1/billing/plans",
			wantHandlerCall: false,
			wantStatus:      http.StatusForbidden,
		},
		{
			name:            "allowlisted path proceeds with no tenant, not 403",
			opts:            []MiddlewareOption{WithAllowlist(http.MethodGet, "/healthz", "/api/v1/authn/register")},
			path:            "/healthz",
			wantHandlerCall: true,
			wantStatus:      http.StatusOK,
		},
		{
			name:            "second path from the same WithAllowlist call also proceeds",
			opts:            []MiddlewareOption{WithAllowlist(http.MethodGet, "/healthz", "/api/v1/authn/register")},
			path:            "/api/v1/authn/register",
			wantHandlerCall: true,
			wantStatus:      http.StatusOK,
		},
		{
			name:            "path not on the configured allowlist still 403s",
			opts:            []MiddlewareOption{WithAllowlist(http.MethodGet, "/healthz")},
			path:            "/api/v1/billing/plans",
			wantHandlerCall: false,
			wantStatus:      http.StatusForbidden,
		},
		{
			name: "multiple WithAllowlist calls accumulate rather than overwrite",
			opts: []MiddlewareOption{
				WithAllowlist(http.MethodGet, "/healthz"),
				WithAllowlist(http.MethodGet, "/api/v1/config/public"),
			},
			path:            "/api/v1/config/public",
			wantHandlerCall: true,
			wantStatus:      http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := &recordingHandler{}
			mw := Middleware(stubResolver{err: resolveErr}, tt.opts...)

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()
			mw(handler).ServeHTTP(rec, req)

			if handler.called != tt.wantHandlerCall {
				t.Errorf("handler called = %t, want %t", handler.called, tt.wantHandlerCall)
			}
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			// Whether or not the request was let through, it must never
			// carry a tenant: an allowlisted route bypasses resolution
			// entirely, it never receives a zero-value tenant standing in
			// for a real one.
			if handler.sawTenantOK {
				t.Errorf("downstream saw tenant %q, want none: a failed resolution must never reach a handler with a tenant set", handler.sawTenant)
			}
			if tt.wantStatus == http.StatusForbidden {
				var body tenantErrorBody
				if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
					t.Fatalf("decoding error body: %v", err)
				}
				if body.Code != ErrTenantUnresolved.Code {
					t.Errorf("error code = %q, want %q", body.Code, ErrTenantUnresolved.Code)
				}
			}
		})
	}
}

// TestMiddleware_AllowlistScopedToMethod proves the security property the
// package doc now states explicitly: allowlisting a path for one HTTP
// method must not exempt every other method that same exact path happens
// to serve. Before WithAllowlist took a method parameter, allowlisting
// "/api/v1/authn/register" (no method scoping existed) exempted GET, POST,
// PUT, DELETE and PATCH alike on that path -- so a path spec'd as
// POST-only would have silently lost tenant-resolution enforcement for
// every other method too, the moment it was allowlisted for the one method
// that legitimately needs to be public. This reproduces that exact
// (resolver-always-fails, single allowlisted path, five methods) scenario
// and asserts only the allowlisted method bypasses resolution.
func TestMiddleware_AllowlistScopedToMethod(t *testing.T) {
	const path = "/api/v1/authn/register"
	resolveErr := errors.New("resolver deliberately failed")

	methods := []string{
		http.MethodGet,
		http.MethodPost,
		http.MethodPut,
		http.MethodDelete,
		http.MethodPatch,
	}

	for _, m := range methods {
		t.Run(m, func(t *testing.T) {
			handler := &recordingHandler{}
			// Only POST on this path is allowlisted.
			mw := Middleware(stubResolver{err: resolveErr}, WithAllowlist(http.MethodPost, path))

			req := httptest.NewRequest(m, path, nil)
			rec := httptest.NewRecorder()
			mw(handler).ServeHTTP(rec, req)

			if m == http.MethodPost {
				if !handler.called {
					t.Fatal("POST is the allowlisted method on this path: handler should have run")
				}
				if rec.Code != http.StatusOK {
					t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
				}
				return
			}

			// Every other method on the exact same path must still fail
			// closed: allowlisting POST here must not leak into GET, PUT,
			// DELETE or PATCH on that same literal path.
			if handler.called {
				t.Errorf("%s reached the handler; only POST was allowlisted for %s", m, path)
			}
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s status = %d, want %d", m, rec.Code, http.StatusForbidden)
			}
		})
	}
}

func TestMiddleware_ResolverReturnsEmptyTenantWithNilError_TreatsAsResolutionFailure(t *testing.T) {
	// A Resolver is never supposed to report success with a zero-value
	// tenant, but Middleware must not trust it if one does: propagating an
	// empty TenantID into the context is indistinguishable, to code that
	// only checks the error return, from "resolved successfully" -- exactly
	// the forgotten-tenant-filter shape this package must not produce.
	handler := &recordingHandler{}
	mw := Middleware(stubResolver{tenant: "", err: nil})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/billing/plans", nil)
	rec := httptest.NewRecorder()
	mw(handler).ServeHTTP(rec, req)

	if handler.called {
		t.Fatal("next handler was called despite the resolver reporting an empty tenant")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// TestMiddleware_IgnoresClientSuppliedTenantHints proves there is no code
// path, allowlisted or not, that reads a client-supplied header or query
// parameter as a tenant hint. The resolver here always fails, so the only
// way a plausible-looking "X-Tenant-ID" header or "?tenant=" query
// parameter could turn into a successful, tenant-scoped request is if
// Middleware fell back to trusting the request -- which it must never do.
func TestMiddleware_IgnoresClientSuppliedTenantHints(t *testing.T) {
	const attackerTenant = pkgcore.TenantID("attacker-tenant")
	resolveErr := errors.New("resolver deliberately failed")

	tests := []struct {
		name            string
		opts            []MiddlewareOption
		wantHandlerCall bool
		wantStatus      int
	}{
		{
			name:            "not allowlisted: rejected despite attacker-supplied hints",
			opts:            nil,
			wantHandlerCall: false,
			wantStatus:      http.StatusForbidden,
		},
		{
			name:            "allowlisted: proceeds, but still without trusting the hints",
			opts:            []MiddlewareOption{WithAllowlist(http.MethodGet, "/public/config")},
			wantHandlerCall: true,
			wantStatus:      http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := &recordingHandler{}
			mw := Middleware(stubResolver{err: resolveErr}, tt.opts...)

			req := httptest.NewRequest(http.MethodGet, "/public/config?tenant="+string(attackerTenant), nil)
			req.Header.Set("X-Tenant-ID", string(attackerTenant))
			rec := httptest.NewRecorder()
			mw(handler).ServeHTTP(rec, req)

			if handler.called != tt.wantHandlerCall {
				t.Fatalf("handler called = %t, want %t", handler.called, tt.wantHandlerCall)
			}
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if handler.sawTenantOK {
				t.Errorf("downstream saw tenant %q sourced from client input, want none", handler.sawTenant)
			}
			if handler.sawTenant == attackerTenant {
				t.Errorf("Middleware leaked the client-supplied header/query value into the tenant context")
			}
		})
	}
}

// stubStatusResolver is a TenantStatusResolver test double whose behavior
// is fixed at construction: statuses maps a tenant to the TenantStatus
// Status reports for it (a tenant absent from the map is treated as
// TenantStatusActive, matching a real resolver that has never heard of
// suspension for a tenant it does track), and err, when non-nil, makes
// every call fail instead.
type stubStatusResolver struct {
	statuses map[pkgcore.TenantID]TenantStatus
	err      error
	calls    []pkgcore.TenantID
}

func (s *stubStatusResolver) Status(_ context.Context, tenant pkgcore.TenantID) (TenantStatus, error) {
	s.calls = append(s.calls, tenant)
	if s.err != nil {
		return "", s.err
	}
	if status, ok := s.statuses[tenant]; ok {
		return status, nil
	}
	return TenantStatusActive, nil
}

// TestMiddleware_NoTenantStatusResolver_BehaviorUnchanged pins D4's
// central compatibility contract: a host that never calls
// WithTenantStatusResolver sees exactly today's Middleware behavior,
// unchanged -- a resolved tenant's request proceeds regardless of
// whatever status it might have anywhere else in the system, because
// nothing here consults one.
func TestMiddleware_NoTenantStatusResolver_BehaviorUnchanged(t *testing.T) {
	handler := &recordingHandler{}
	mw := Middleware(stubResolver{tenant: "acme"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/billing/plans", nil)
	rec := httptest.NewRecorder()
	mw(handler).ServeHTTP(rec, req)

	if !handler.called {
		t.Fatal("next handler was not called; an unwired TenantStatusResolver must never itself gate a request")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestMiddleware_TenantStatusResolver_ActiveTenant_Proceeds proves the
// wired-but-active case is indistinguishable from the unwired case for
// the caller.
func TestMiddleware_TenantStatusResolver_ActiveTenant_Proceeds(t *testing.T) {
	handler := &recordingHandler{}
	resolver := &stubStatusResolver{statuses: map[pkgcore.TenantID]TenantStatus{"acme": TenantStatusActive}}
	mw := Middleware(stubResolver{tenant: "acme"}, WithTenantStatusResolver(resolver))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/billing/plans", nil)
	rec := httptest.NewRecorder()
	mw(handler).ServeHTTP(rec, req)

	if !handler.called {
		t.Fatal("next handler was not called for an active tenant")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if len(resolver.calls) != 1 || resolver.calls[0] != "acme" {
		t.Errorf("resolver.calls = %v, want exactly one call for tenant %q", resolver.calls, "acme")
	}
}

// TestMiddleware_TenantStatusResolver_SuspendedTenant_RefusesWithCodedError
// is D4's core enforcement proof: a suspended tenant's request never
// reaches the handler and is refused with the coded ErrTenantSuspended
// error, never a bare HTTP status with no code.
func TestMiddleware_TenantStatusResolver_SuspendedTenant_RefusesWithCodedError(t *testing.T) {
	handler := &recordingHandler{}
	resolver := &stubStatusResolver{statuses: map[pkgcore.TenantID]TenantStatus{"acme": TenantStatusSuspended}}
	mw := Middleware(stubResolver{tenant: "acme"}, WithTenantStatusResolver(resolver))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/billing/plans", nil)
	rec := httptest.NewRecorder()
	mw(handler).ServeHTTP(rec, req)

	if handler.called {
		t.Fatal("next handler was called despite the resolved tenant being suspended")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	var body tenantErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding error body: %v", err)
	}
	if body.Code != ErrTenantSuspended.Code {
		t.Errorf("error code = %q, want %q", body.Code, ErrTenantSuspended.Code)
	}
}

// TestMiddleware_TenantStatusResolver_StatusCallFails_FailsClosed proves
// the resolver's own discipline extends to TenantStatusResolver: an
// unreachable status source refuses the request rather than assuming the
// tenant is active.
func TestMiddleware_TenantStatusResolver_StatusCallFails_FailsClosed(t *testing.T) {
	handler := &recordingHandler{}
	resolver := &stubStatusResolver{err: errors.New("status store unreachable")}
	mw := Middleware(stubResolver{tenant: "acme"}, WithTenantStatusResolver(resolver))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/billing/plans", nil)
	rec := httptest.NewRecorder()
	mw(handler).ServeHTTP(rec, req)

	if handler.called {
		t.Fatal("next handler was called despite the status resolver failing")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	var body tenantErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding error body: %v", err)
	}
	if body.Code != ErrTenantStatusUnavailable.Code {
		t.Errorf("error code = %q, want %q", body.Code, ErrTenantStatusUnavailable.Code)
	}
}

// TestMiddleware_TenantStatusResolver_AllowlistedNoTenant_NeverConsulted
// proves the status check is skipped entirely -- not merely defaulted to
// active -- when a request proceeds with no resolved tenant at all (an
// allowlisted path whose Resolver failed): there is nothing to look up a
// status for, and a resolver that would panic or error on an empty
// pkgcore.TenantID must never be called with one.
func TestMiddleware_TenantStatusResolver_AllowlistedNoTenant_NeverConsulted(t *testing.T) {
	handler := &recordingHandler{}
	resolver := &stubStatusResolver{err: errors.New("must never be called")}
	mw := Middleware(
		stubResolver{err: errors.New("resolver deliberately failed")},
		WithAllowlist(http.MethodGet, "/healthz"),
		WithTenantStatusResolver(resolver),
	)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mw(handler).ServeHTTP(rec, req)

	if !handler.called {
		t.Fatal("next handler was not called for an allowlisted path")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if len(resolver.calls) != 0 {
		t.Errorf("resolver.calls = %v, want none: a request with no resolved tenant must never consult TenantStatusResolver", resolver.calls)
	}
}

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
// panicking or fabricating one. It then additionally confirms, in a real
// request rather than only a bare context.Context, that WithSystemContext
// still does not compose with Repository[T]: even fully elevated, a
// Repository[T] call this handler makes still fails closed with
// pkgcore.ErrNoTenant, because nothing in this request ever supplied a
// tenant and the escape hatch does not substitute for one on
// Repository[T]'s path.
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

	// The same WithSystemContext/Repository[T] non-composition property,
	// confirmed once more through this file's real net/http request rather
	// than a bare context.Context: a granted system reason does not let this
	// handler's Repository[T] call read across tenants, or run at all,
	// despite the elevation having genuinely succeeded above.
	if !errors.Is(repoErr, pkgcore.ErrNoTenant) {
		t.Errorf("repo.List(elevated, no-tenant) error = %v, want errors.Is(err, pkgcore.ErrNoTenant); confirms WithSystemContext grants no Repository[T] bypass even for an allowlisted, intentionally cross-tenant route", repoErr)
	}
}
