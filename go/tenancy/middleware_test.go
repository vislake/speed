package tenancy

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

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
