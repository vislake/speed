package authn

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
)

// middlewareFixture is a signer, a verifier and a clock over one key set.
type middlewareFixture struct {
	signer   *Signer
	verifier *Verifier
	clock    *testutil.Clock
}

func newMiddlewareFixture(t *testing.T) *middlewareFixture {
	t.Helper()

	keys, _ := newTestKeySet(t)
	clock := testutil.NewClock(time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))
	signer, err := NewSigner(keys, WithTokenClock(clock.Now), WithTokenTTL(testAccessTTL))
	if err != nil {
		t.Fatalf("NewSigner() error = %v", err)
	}
	verifier, err := NewVerifier(keys, WithTokenClock(clock.Now))
	if err != nil {
		t.Fatalf("NewVerifier() error = %v", err)
	}
	return &middlewareFixture{signer: signer, verifier: verifier, clock: clock}
}

func (f *middlewareFixture) token(t *testing.T, p Principal) string {
	t.Helper()
	token, _, err := f.signer.Issue(p)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	return token
}

// observed records what a handler behind the middleware actually saw.
type observed struct {
	called          bool
	principal       Principal
	principalExists bool
	tenant          pkgcore.TenantID
	tenantExists    bool
}

// observingHandler records the request context and answers 200.
func observingHandler(out *observed) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out.called = true
		out.principal, out.principalExists = PrincipalFromContext(r.Context())
		out.tenant, out.tenantExists = pkgcore.TenantFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

// decodeErrorBody reads the {code, params} envelope out of a response.
func decodeErrorBody(t *testing.T, rec *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the error body %q: %v", rec.Body.String(), err)
	}
	return body
}

// TestMiddleware_OptionalAuthentication covers the three cases that are
// deliberately NOT the same: no credential, a failed credential, and a valid
// one.
func TestMiddleware_OptionalAuthentication(t *testing.T) {
	t.Parallel()

	f := newMiddlewareFixture(t)
	principal := Principal{UserID: "user-1", TenantID: testTenantA, SessionID: "session-1", AMR: []string{MethodPassword}}
	valid := f.token(t, principal)

	expiredFixture := newMiddlewareFixture(t)
	expired := expiredFixture.token(t, principal)
	expiredFixture.clock.Advance(testAccessTTL + time.Minute)

	cases := []struct {
		name        string
		header      string
		verifier    *Verifier
		wantStatus  int
		wantCode    string
		wantCalled  bool
		wantSubject string
	}{
		{
			name:     "no header proceeds without a principal, so a public route still works",
			verifier: f.verifier, wantStatus: http.StatusOK, wantCalled: true,
		},
		{
			name:   "a non-bearer scheme is treated as no credential at all",
			header: "Basic dXNlcjpwYXNz", verifier: f.verifier, wantStatus: http.StatusOK, wantCalled: true,
		},
		{
			name:   "an empty bearer value is treated as no credential",
			header: "Bearer   ", verifier: f.verifier, wantStatus: http.StatusOK, wantCalled: true,
		},
		{
			name:   "a valid token puts the principal in the context",
			header: "Bearer " + valid, verifier: f.verifier,
			wantStatus: http.StatusOK, wantCalled: true, wantSubject: "user-1",
		},
		{
			name:   "the bearer scheme is matched case-insensitively",
			header: "bearer " + valid, verifier: f.verifier,
			wantStatus: http.StatusOK, wantCalled: true, wantSubject: "user-1",
		},
		{
			name:   "a garbage token is refused rather than downgraded to anonymous",
			header: "Bearer not.a.token", verifier: f.verifier,
			wantStatus: http.StatusUnauthorized, wantCode: ErrTokenInvalid.Code,
		},
		{
			name:   "an expired token reports its own reason",
			header: "Bearer " + expired, verifier: expiredFixture.verifier,
			wantStatus: http.StatusUnauthorized, wantCode: ErrTokenExpired.Code,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out observed
			handler := Middleware(tc.verifier)(observingHandler(&out))

			req := httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil)
			if tc.header != "" {
				req.Header.Set(authorizationHeader, tc.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if out.called != tc.wantCalled {
				t.Fatalf("handler called = %v, want %v", out.called, tc.wantCalled)
			}
			if tc.wantCode != "" {
				if got := decodeErrorBody(t, rec).Code; got != tc.wantCode {
					t.Errorf("error code = %q, want %q", got, tc.wantCode)
				}
				return
			}
			if tc.wantSubject == "" {
				if out.principalExists {
					t.Errorf("a principal reached the handler for a request that carried no valid credential: %+v", out.principal)
				}
				return
			}
			if !out.principalExists {
				t.Fatal("no principal reached the handler for a valid token")
			}
			if out.principal.UserID != tc.wantSubject {
				t.Errorf("principal user = %q, want %q", out.principal.UserID, tc.wantSubject)
			}
		})
	}
}

// TestMiddleware_NeverInjectsTheTenant pins the boundary: injecting the tenant
// is tenancy.Middleware's single job. If this middleware also called
// pkgcore.WithTenant there would be two places that decide a request's tenant,
// which is exactly the split the chain order was designed to avoid.
func TestMiddleware_NeverInjectsTheTenant(t *testing.T) {
	t.Parallel()

	f := newMiddlewareFixture(t)
	token := f.token(t, Principal{UserID: "user-1", TenantID: testTenantA, SessionID: "session-1"})

	var out observed
	handler := Middleware(f.verifier)(observingHandler(&out))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil)
	req.Header.Set(authorizationHeader, "Bearer "+token)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !out.principalExists {
		t.Fatal("no principal reached the handler")
	}
	if out.tenantExists {
		t.Errorf("a tenant (%q) was in the context; authn.Middleware must never call pkgcore.WithTenant", out.tenant)
	}
}

func TestRequireAuthenticated(t *testing.T) {
	t.Parallel()

	f := newMiddlewareFixture(t)
	token := f.token(t, Principal{UserID: "user-1", TenantID: testTenantA, SessionID: "session-1"})

	var out observed
	handler := Middleware(f.verifier)(RequireAuthenticated(observingHandler(&out)))

	t.Run("without a credential", func(t *testing.T) {
		out = observed{}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
		if got := decodeErrorBody(t, rec).Code; got != ErrAuthenticationRequired.Code {
			t.Errorf("error code = %q, want %q", got, ErrAuthenticationRequired.Code)
		}
		if out.called {
			t.Error("the protected handler ran for an unauthenticated request")
		}
	})

	t.Run("with a credential", func(t *testing.T) {
		out = observed{}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		req.Header.Set(authorizationHeader, "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
		}
		if !out.called {
			t.Error("the protected handler did not run for an authenticated request")
		}
	})
}

// TestMiddleware_RevocationChecker covers the immediate-revocation mode's
// enforcement point, including the case that matters most: a checker that
// cannot answer must produce a refusal, not a pass.
func TestMiddleware_RevocationChecker(t *testing.T) {
	t.Parallel()

	f := newMiddlewareFixture(t)
	token := f.token(t, Principal{UserID: "user-1", TenantID: testTenantA, SessionID: "session-1"})

	cases := []struct {
		name       string
		checker    RevocationChecker
		wantStatus int
		wantCode   string
	}{
		{name: "no checker configured", checker: nil, wantStatus: http.StatusOK},
		{name: "a live session", checker: stubChecker{}, wantStatus: http.StatusOK},
		{
			name: "a revoked session", checker: stubChecker{revoked: true},
			wantStatus: http.StatusUnauthorized, wantCode: ErrSessionRevoked.Code,
		},
		{
			name:    "a checker that cannot answer fails closed",
			checker: stubChecker{err: testutil.ErrKVUnavailable},
			// Fail closed. Treating an unreachable revocation list as
			// "not revoked" would silently re-enable every session an
			// operator had just signed out, at the one moment the
			// guarantee matters.
			wantStatus: http.StatusInternalServerError, wantCode: ErrRevocationCheckFailed.Code,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out observed
			handler := Middleware(f.verifier, WithRevocationChecker(tc.checker))(observingHandler(&out))

			req := httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil)
			req.Header.Set(authorizationHeader, "Bearer "+token)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantCode == "" {
				if !out.called {
					t.Error("the handler did not run")
				}
				return
			}
			if out.called {
				t.Error("the handler ran despite the revocation outcome")
			}
			if got := decodeErrorBody(t, rec).Code; got != tc.wantCode {
				t.Errorf("error code = %q, want %q", got, tc.wantCode)
			}
		})
	}
}

// stubChecker is a RevocationChecker with a fixed answer.
type stubChecker struct {
	revoked bool
	err     error
}

func (s stubChecker) IsRevoked(context.Context, string) (bool, error) { return s.revoked, s.err }

func TestPrincipalResolver_Resolve(t *testing.T) {
	t.Parallel()

	resolver := NewPrincipalResolver()

	cases := []struct {
		name       string
		ctx        func() context.Context
		wantTenant pkgcore.TenantID
		wantCode   string
	}{
		{
			name: "a principal supplies its tenant",
			ctx: func() context.Context {
				return WithPrincipal(context.Background(), Principal{UserID: "u", TenantID: testTenantA, SessionID: "s"})
			},
			wantTenant: testTenantA,
		},
		{
			name:     "no principal is an error, never a default tenant",
			ctx:      context.Background,
			wantCode: ErrAuthenticationRequired.Code,
		},
		{
			name: "a principal with no tenant is an error",
			ctx: func() context.Context {
				return WithPrincipal(context.Background(), Principal{UserID: "u", SessionID: "s"})
			},
			wantCode: ErrTokenInvalid.Code,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil).WithContext(tc.ctx())
			tenant, err := resolver.Resolve(req)

			if tc.wantCode != "" {
				if !hasCode(err, tc.wantCode) {
					t.Fatalf("Resolve() error = %v, want code %q", err, tc.wantCode)
				}
				if tenant != "" {
					t.Errorf("Resolve() = %q alongside an error; a resolver must never invent a tenant", tenant)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if tenant != tc.wantTenant {
				t.Errorf("Resolve() = %q, want %q", tenant, tc.wantTenant)
			}
		})
	}
}

// TestComposedChain_AuthnThenTenancy exercises the deliberate deviation from
// the documented middleware order, end to end.
//
// authn.Middleware runs FIRST and verifies the token once; tenancy.Middleware
// then reads the already-verified Principal through PrincipalResolver and is
// the only thing that calls pkgcore.WithTenant. The alternative order cannot
// work: tenancy.Resolver returns (TenantID, error) and no context, so a
// resolver that verified the token would have nowhere to hand the claims, and
// the token would be verified twice by two code paths free to diverge.
func TestComposedChain_AuthnThenTenancy(t *testing.T) {
	t.Parallel()

	const publicPath = "/api/v1/authn/login"

	f := newMiddlewareFixture(t)
	token := f.token(t, Principal{UserID: "user-1", TenantID: testTenantA, SessionID: "session-1"})

	var out observed
	chain := Middleware(f.verifier)(
		tenancy.Middleware(NewPrincipalResolver(),
			tenancy.WithAllowlist(http.MethodPost, publicPath),
			tenancy.WithAllowlist(http.MethodGet, publicPath),
		)(observingHandler(&out)),
	)

	t.Run("an authenticated request reaches the handler with the token's tenant", func(t *testing.T) {
		out = observed{}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil)
		req.Header.Set(authorizationHeader, "Bearer "+token)
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
		}
		if !out.tenantExists {
			t.Fatal("no tenant in the handler's context; tenancy.Middleware did not inject one")
		}
		if out.tenant != testTenantA {
			t.Errorf("tenant = %q, want the token's own tenant %q", out.tenant, testTenantA)
		}
		if !out.principalExists {
			t.Error("no principal in the handler's context")
		}
	})

	t.Run("an unauthenticated request to a protected path fails closed", func(t *testing.T) {
		out = observed{}
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil))

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d from tenancy.Middleware", rec.Code, http.StatusForbidden)
		}
		if got := decodeErrorBody(t, rec).Code; got != "tenancy.tenant_unresolved" {
			t.Errorf("error code = %q, want tenancy's own unresolved-tenant code", got)
		}
		if out.called {
			t.Error("the handler ran for a request with no resolvable tenant")
		}
	})

	t.Run("an allowlisted pre-auth path works with no credential and no tenant", func(t *testing.T) {
		out = observed{}
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, publicPath, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
		}
		if !out.called {
			t.Fatal("the handler did not run for an allowlisted pre-auth path")
		}
		if out.tenantExists {
			t.Errorf("a tenant (%q) reached a pre-auth handler", out.tenant)
		}
	})

	t.Run("an invalid credential is refused by authn before tenancy sees it", func(t *testing.T) {
		out = observed{}
		req := httptest.NewRequest(http.MethodPost, publicPath, nil)
		req.Header.Set(authorizationHeader, "Bearer not.a.token")
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)

		// Even on an allowlisted path: the allowlist exempts a request
		// from needing a TENANT, not from a credential it presented
		// being valid.
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
		if got := decodeErrorBody(t, rec).Code; got != ErrTokenInvalid.Code {
			t.Errorf("error code = %q, want %q", got, ErrTokenInvalid.Code)
		}
	})
}

// TestPrincipalFromContext_IgnoresAnEmptyPrincipal guards the fail-closed
// reading: a zero-value Principal put in a context by mistake must not read
// back as an authenticated caller.
func TestPrincipalFromContext_IgnoresAnEmptyPrincipal(t *testing.T) {
	t.Parallel()

	ctx := WithPrincipal(context.Background(), Principal{})
	if _, ok := PrincipalFromContext(ctx); ok {
		t.Error("PrincipalFromContext() reported a zero-value Principal as authenticated")
	}
}
