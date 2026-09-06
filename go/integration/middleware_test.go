package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// assertErrorEnvelope decodes rec's body as this module's own errorBody
// envelope (httpguard.go) and requires status and code to match, mirroring
// httpguard_test.go's own inline assertion shape.
func assertErrorEnvelope(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, wantStatus, rec.Body.String())
	}
	var body errorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response body %q: %v", rec.Body.String(), err)
	}
	if body.Code != wantCode {
		t.Errorf("body.Code = %q, want %q", body.Code, wantCode)
	}
}

// newTestModuleWithService builds a Module through the real
// Register-then-Attach sequence (the same shape newTestHandler uses) and
// returns both it and the Service Attach produced, so a test can Create a
// real key through the ordinary Service API and then authenticate it
// through AuthMiddleware over HTTP.
func newTestModuleWithService(t *testing.T) (*Module, *Service) {
	t.Helper()
	m := NewModule(newTestDB(t))
	reg := newTestRegistry(t)
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	svc, err := m.Attach(reg)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	return m, svc
}

// nextCalledHandler records whether it was ever invoked, and echoes back
// what AuthenticatedAPIKeyFromContext and pkgcore.TenantFromContext see, so
// a test can assert on exactly what Middleware attached.
type nextCalledHandler struct {
	called      bool
	gotKey      *AuthenticatedAPIKey
	gotKeyOK    bool
	gotTenant   pkgcore.TenantID
	gotTenantOK bool
}

func (h *nextCalledHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.called = true
	h.gotKey, h.gotKeyOK = AuthenticatedAPIKeyFromContext(r.Context())
	h.gotTenant, h.gotTenantOK = pkgcore.TenantFromContext(r.Context())
	w.WriteHeader(http.StatusOK)
}

func TestAuthMiddleware_MissingHeader_WritesAuthenticationFailed(t *testing.T) {
	m, _ := newTestModuleWithService(t)
	next := &nextCalledHandler{}
	h := NewAuthMiddleware(m).Middleware(next)

	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if next.called {
		t.Error("next was called for a request with no HeaderAPIKey at all")
	}
	assertErrorEnvelope(t, rec, http.StatusUnauthorized, "integration.authentication_failed")
}

func TestAuthMiddleware_WrongKey_WritesAuthenticationFailed(t *testing.T) {
	m, _ := newTestModuleWithService(t)
	next := &nextCalledHandler{}
	h := NewAuthMiddleware(m).Middleware(next)

	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.Header.Set(HeaderAPIKey, "sk_this-was-never-issued")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if next.called {
		t.Error("next was called for a request bearing an unrecognized key")
	}
	assertErrorEnvelope(t, rec, http.StatusUnauthorized, "integration.authentication_failed")
}

// TestAuthMiddleware_ValidKey_AttachesTenantAndAuthenticatedAPIKey_CallsNext
// is the end-to-end proof of Middleware's own success path: a real key,
// created through the ordinary Service.Create call, authenticates over HTTP
// and next.ServeHTTP sees both pkgcore.WithTenant's tenant and the full
// AuthenticatedAPIKey.
func TestAuthMiddleware_ValidKey_AttachesTenantAndAuthenticatedAPIKey_CallsNext(t *testing.T) {
	m, svc := newTestModuleWithService(t)
	created, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	next := &nextCalledHandler{}
	h := NewAuthMiddleware(m).Middleware(next)

	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.Header.Set(HeaderAPIKey, created.Key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !next.called {
		t.Fatalf("next was never called; response: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !next.gotTenantOK || next.gotTenant != testTenant {
		t.Errorf("tenant seen by next = (%q, %v), want (%q, true)", next.gotTenant, next.gotTenantOK, testTenant)
	}
	if !next.gotKeyOK {
		t.Fatal("next saw no AuthenticatedAPIKey in context")
	}
	if next.gotKey.KeyID != created.ID {
		t.Errorf("KeyID seen by next = %q, want %q", next.gotKey.KeyID, created.ID)
	}
}

// TestAuthMiddleware_ServedBeforeAttach_WritesInternalError mirrors
// TestHandler_ServedBeforeAttach_WritesInternalError: a Middleware built
// over a Module with no Service yet (the Register-time-Middleware/
// Attach-time-Service race) answers a coded internal error rather than
// panicking on a nil Service.
func TestAuthMiddleware_ServedBeforeAttach_WritesInternalError(t *testing.T) {
	m := &Module{}
	h := NewAuthMiddleware(m).Middleware(&nextCalledHandler{})

	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.Header.Set(HeaderAPIKey, "sk_whatever")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
}
