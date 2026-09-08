package aigateway

// Tests for handler.go's Handler: the three operations it implements
// against api.ServerInterface, and the error-shaping edge cases
// handler_example_test.go's happy-path walk does not exercise -- an
// unknown provider's read, a malformed write body, and an empty api key on
// both write operations. The example proves the write-then-resolve
// connection; this file proves the handler's own request/response
// translation is correct in isolation.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
)

// newTestHandler returns a Handler over a fresh, migrated test database.
// It registers SystemPurposeCredentialWrite itself -- ordinarily
// Module.Register's job -- because these tests build a Handler directly
// over a CredentialService rather than going through NewModule/Register,
// exactly mirroring credential_test.go's own systemTestCtx helper. The
// handler is built over a fresh memory bus (NewHandler's second argument
// is the bus the platform-credential operation publishes its audited
// system-context event on); a test that must observe that event passes its
// own bus and subscribes to it, as
// TestHandler_SetPlatformCredential_PublishesAuditedSystemContextEvent
// does.
func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	pkgcore.RegisterSystemPurpose(SystemPurposeCredentialWrite)
	return NewHandler(NewCredentialService(newTestDB(t)), pkgcore.NewMemoryEventBus())
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

// TestHandler_SetPlatformCredential_PublishesAuditedSystemContextEvent is
// the audit-trail regression test for the platform-credential HTTP path: a
// platform-wide credential write is a privileged platform-row write that
// must never happen unrecorded, so the handler must take its system context
// through tenancy.WithSystemContext (the audited wrapper), which publishes
// an EventSystemContextEntered event on the bus it was built with -- never
// through pkgcore's bare WithSystemContext, which publishes nothing (see
// handler.go's AiGatewaySetPlatformCredential). The wrapper is the
// mandatory path for code that can import tenancy; the test fails if the
// path ever regresses to the bare primitive, which emits zero events.
func TestHandler_SetPlatformCredential_PublishesAuditedSystemContextEvent(t *testing.T) {
	pkgcore.RegisterSystemPurpose(SystemPurposeCredentialWrite)

	bus := pkgcore.NewMemoryEventBus()
	var mu sync.Mutex
	var entered []tenancy.SystemContextEnteredEvent
	bus.Subscribe(tenancy.EventSystemContextEntered, func(_ context.Context, evt pkgcore.Event) error {
		if s, ok := evt.Payload.(tenancy.SystemContextEnteredEvent); ok {
			mu.Lock()
			defer mu.Unlock()
			entered = append(entered, s)
		}
		return nil
	})

	h := NewHandler(NewCredentialService(newTestDB(t)), bus)

	status, body := doHandlerRequest(t, h, context.Background(), http.MethodPut,
		"/api/v1/ai-gateway/credentials/"+ProviderOpenAICompatible+"/platform", `{"apiKey":"sk-platform"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %v", status, http.StatusOK, body)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(entered) != 1 {
		t.Fatalf("platform-credential write published %d audited system-context events, want exactly 1 — the write must go through tenancy.WithSystemContext, never the bare pkgcore primitive", len(entered))
	}
	got := entered[0]
	if got.Actor != handlerSystemActor {
		t.Errorf("event Actor = %q, want %q", got.Actor, handlerSystemActor)
	}
	if got.Purpose != SystemPurposeCredentialWrite {
		t.Errorf("event Purpose = %q, want %q", got.Purpose, SystemPurposeCredentialWrite)
	}
}

// TestHandler_SetPlatformCredential_NilBus_FailsClosedBeforeAnyGrant pins
// the nil-bus guard in AiGatewaySetPlatformCredential: a Handler wired by
// hand without the event bus its audited system-context publish needs must
// refuse the write outright, never panic mid-publish after the escape hatch
// was already granted (see the guard's own comment for why that ordering
// matters). Module.Register always passes the registry's resolved bus, so
// this is a construction-error path, refused closed rather than assumed
// away.
func TestHandler_SetPlatformCredential_NilBus_FailsClosedBeforeAnyGrant(t *testing.T) {
	pkgcore.RegisterSystemPurpose(SystemPurposeCredentialWrite)

	h := NewHandler(NewCredentialService(newTestDB(t)), nil)

	status, body := doHandlerRequest(t, h, context.Background(), http.MethodPut,
		"/api/v1/ai-gateway/credentials/"+ProviderOpenAICompatible+"/platform", `{"apiKey":"sk-platform"}`)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", status, http.StatusInternalServerError)
	}
	if body["code"] != ErrInternal.Code {
		t.Fatalf("code = %v, want %q", body["code"], ErrInternal.Code)
	}
}
