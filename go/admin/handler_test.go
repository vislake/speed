package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/pkgcore"
)

// decodedErrorCode decodes w's body as api.AdminError's own {"code": ...}
// envelope shape and returns the code, failing the test if the body does
// not decode.
func decodedErrorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error envelope %q: %v", w.Body.String(), err)
	}
	return body.Code
}

// TestHandler_MalformedRequestBody_ReportsRequestBodyInvalid is Finding
// P3-5's regression test: a genuinely malformed JSON body against
// AdminUpdateTenant (one of the five sites that used to wrap a decode
// failure in the semantically wrong ErrTenantIDRequired) must now be
// reported as admin.request_body_invalid, never admin.tenant_id_required
// -- proven against the real composed HTTP handler, not a bare service call.
func TestHandler_MalformedRequestBody_ReportsRequestBodyInvalid(t *testing.T) {
	env := buildTestAdminModule(t)

	const tenant = pkgcore.TenantID("tenant-malformed-body")
	if _, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenant), "Malformed Body Co", "workspace"); err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}

	body := strings.NewReader(`{"displayName": 12345}`) // displayName must be a string, not a number
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/tenants/"+string(tenant), body)
	req = req.WithContext(authn.WithPrincipal(req.Context(), authn.Principal{UserID: "operator-malformed-1"}))
	w := httptest.NewRecorder()

	env.Admin.handler.ServeHTTP(w, req)

	if got := decodedErrorCode(t, w); got != ErrRequestBodyInvalid.Code {
		t.Fatalf("code = %q, want %q (NOT %q, the pre-fix semantically-wrong code)",
			got, ErrRequestBodyInvalid.Code, ErrTenantIDRequired.Code)
	}
}

// TestHandler_StartImpersonation_MalformedBody_ReportsRequestBodyInvalid
// covers the sixth site, which wrapped its decode failure in
// ErrImpersonationTargetRequired rather than ErrTenantIDRequired -- a
// different wrong code, but the identical defect (a decode failure
// reported as a semantically unrelated validation refusal).
func TestHandler_StartImpersonation_MalformedBody_ReportsRequestBodyInvalid(t *testing.T) {
	env := buildTestAdminModule(t)

	body := strings.NewReader(`{"targetUserId": true}`) // targetUserId must be a string, not a bool
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/impersonation", body)
	req = req.WithContext(authn.WithPrincipal(req.Context(), authn.Principal{UserID: "operator-malformed-2"}))
	w := httptest.NewRecorder()

	env.Admin.handler.ServeHTTP(w, req)

	if got := decodedErrorCode(t, w); got != ErrRequestBodyInvalid.Code {
		t.Fatalf("code = %q, want %q (NOT %q, the pre-fix semantically-wrong code)",
			got, ErrRequestBodyInvalid.Code, ErrImpersonationTargetRequired.Code)
	}
}
