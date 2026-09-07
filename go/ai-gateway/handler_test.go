package aigateway

// Tests for handler.go's Handler: the three operations it implements
// against api.ServerInterface, and the error-shaping edge cases
// handler_example_test.go's happy-path walk does not exercise -- an
// unknown provider's read, a malformed write body, and an empty api key on
// both write operations. The example proves the round's mandatory
// write-then-resolve connection; this file proves the handler's own
// request/response translation is correct in isolation.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// newTestHandler returns a Handler over a fresh, migrated test database.
// It registers SystemPurposeCredentialWrite itself -- ordinarily
// Module.Register's job -- because these tests build a Handler directly
// over a CredentialService rather than going through NewModule/Register,
// exactly mirroring credential_test.go's own systemTestCtx helper.
func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	pkgcore.RegisterSystemPurpose(SystemPurposeCredentialWrite)
	return NewHandler(NewCredentialService(newTestDB(t)))
}

// doHandlerRequest issues method against path on h with ctx and body,
// returning the decoded {code} or {provider, scope, baseUrl} JSON response
// alongside the status code.
func doHandlerRequest(t *testing.T, h *Handler, ctx context.Context, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var decoded map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decode response body %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, decoded
}

func TestHandler_GetCredential_UnknownProvider_NotFound(t *testing.T) {
	h := newTestHandler(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-1"))

	status, body := doHandlerRequest(t, h, ctx, http.MethodGet, "/api/v1/ai-gateway/credentials/chat.unknown", "")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", status, http.StatusNotFound)
	}
	if body["code"] != ErrCredentialNotFound.Code {
		t.Fatalf("code = %v, want %q", body["code"], ErrCredentialNotFound.Code)
	}
}

func TestHandler_SetTenantCredential_EmptyAPIKey_Invalid(t *testing.T) {
	h := newTestHandler(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-1"))

	status, body := doHandlerRequest(t, h, ctx, http.MethodPut,
		"/api/v1/ai-gateway/credentials/"+ProviderOpenAICompatible+"/tenant", `{"apiKey":""}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
	if body["code"] != ErrCredentialRequired.Code {
		t.Fatalf("code = %v, want %q", body["code"], ErrCredentialRequired.Code)
	}
}

func TestHandler_SetTenantCredential_MalformedBody_Invalid(t *testing.T) {
	h := newTestHandler(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-1"))

	status, _ := doHandlerRequest(t, h, ctx, http.MethodPut,
		"/api/v1/ai-gateway/credentials/"+ProviderOpenAICompatible+"/tenant", `{not json`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

func TestHandler_SetTenantCredential_NoTenant_Refused(t *testing.T) {
	h := newTestHandler(t)

	// No pkgcore.WithTenant call: an ordinary context carries no tenant,
	// which CredentialService.SetTenantCredential itself refuses --
	// proving the Handler passes the request's context through verbatim
	// rather than inventing a tenant of its own.
	status, body := doHandlerRequest(t, h, context.Background(), http.MethodPut,
		"/api/v1/ai-gateway/credentials/"+ProviderOpenAICompatible+"/tenant", `{"apiKey":"sk-test"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
	if body["code"] != ErrTenantScopeRequiresTenant.Code {
		t.Fatalf("code = %v, want %q", body["code"], ErrTenantScopeRequiresTenant.Code)
	}
}

func TestHandler_SetPlatformCredential_EmptyAPIKey_Invalid(t *testing.T) {
	h := newTestHandler(t)

	// No permission gate runs inside the handler itself (see Handler's own
	// doc comment) -- this proves the translation layer builds the
	// required system context on the caller's behalf regardless, so the
	// validation failure below is CredentialService's own, not a system-
	// context failure.
	status, body := doHandlerRequest(t, h, context.Background(), http.MethodPut,
		"/api/v1/ai-gateway/credentials/"+ProviderOpenAICompatible+"/platform", `{"apiKey":""}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
	if body["code"] != ErrCredentialRequired.Code {
		t.Fatalf("code = %v, want %q", body["code"], ErrCredentialRequired.Code)
	}
}

func TestHandler_SetTenantCredential_BlockedBaseURL_Refused(t *testing.T) {
	h := newTestHandler(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-1"))

	// The HTTP surface's own refusal shape: the tenant write naming a
	// loopback baseUrl answers 400 with the coded aigateway.base_url_blocked
	// envelope -- CredentialService's SSRF validation surfacing through the
	// handler verbatim, never an uncoded error.
	status, body := doHandlerRequest(t, h, ctx, http.MethodPut,
		"/api/v1/ai-gateway/credentials/"+ProviderOpenAICompatible+"/tenant",
		`{"apiKey":"sk-test","baseUrl":"http://127.0.0.1:9000/v1"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
	if body["code"] != ErrBaseURLBlocked.Code {
		t.Fatalf("code = %v, want %q", body["code"], ErrBaseURLBlocked.Code)
	}
}

func TestHandler_SetTenantCredential_PublicBaseURL_Stored(t *testing.T) {
	h := newTestHandler(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-1"))

	status, body := doHandlerRequest(t, h, ctx, http.MethodPut,
		"/api/v1/ai-gateway/credentials/"+ProviderOpenAICompatible+"/tenant",
		`{"apiKey":"sk-test","baseUrl":"https://93.184.216.34/v1"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %v", status, http.StatusOK, body)
	}
	if body["scope"] != string(CredentialScopeTenant) {
		t.Fatalf("scope = %v, want %q", body["scope"], CredentialScopeTenant)
	}
	if body["baseUrl"] != "https://93.184.216.34/v1" {
		t.Fatalf("baseUrl = %v, want %q", body["baseUrl"], "https://93.184.216.34/v1")
	}
}

func TestHandler_SetPlatformCredential_PrivateBaseURL_StillAccepted(t *testing.T) {
	h := newTestHandler(t)

	// The scope-boundary pin at the HTTP layer: the platform-wide write
	// naming a private baseUrl is still accepted -- the operator's own
	// default is outside the SSRF guard (ssrf.go's file header; the
	// reference app's boot-time platform credential lives on the same
	// trusted side).
	status, body := doHandlerRequest(t, h, context.Background(), http.MethodPut,
		"/api/v1/ai-gateway/credentials/"+ProviderOpenAICompatible+"/platform",
		`{"apiKey":"sk-platform","baseUrl":"http://10.0.0.9:9000/v1"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %v", status, http.StatusOK, body)
	}
	if body["scope"] != string(CredentialScopeSystem) {
		t.Fatalf("scope = %v, want %q", body["scope"], CredentialScopeSystem)
	}
}

func TestHandler_SetPlatformCredential_NoBaseURL_OmitsBaseURLField(t *testing.T) {
	h := newTestHandler(t)

	status, body := doHandlerRequest(t, h, context.Background(), http.MethodPut,
		"/api/v1/ai-gateway/credentials/"+ProviderOpenAICompatible+"/platform", `{"apiKey":"sk-platform"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if body["scope"] != string(CredentialScopeSystem) {
		t.Fatalf("scope = %v, want %q", body["scope"], CredentialScopeSystem)
	}
	if _, present := body["baseUrl"]; present {
		t.Fatalf("baseUrl present in response = %v, want omitted entirely", body["baseUrl"])
	}
	if _, present := body["apiKey"]; present {
		t.Fatalf("apiKey present in response, want never echoed back")
	}
}
