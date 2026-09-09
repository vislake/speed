package flowtests

// ai_gateway_flow_test.go drives go/ai-gateway's credential-write HTTP
// surface -- the module's own OpenAPI fragment mounted at
// AiGatewayRoutePath, gated by demoRouteGuards[AiGatewayRoutePath] through
// aiGatewayPermissionFor's three-permission selector -- end to end through
// the composed HTTP stack: the real authn+tenancy middleware chain, a real
// temp-file SQLite database carrying ai-gateway's real migrations, and the
// real rbac gate deciding the requests against seedDemoGrants' demo roles.
//
// Three legs cover the surface's acceptance shape:
//
//   - the two-tier permission gate is real on the composed stack: a
//     tenant-scoped actor (DemoAIGatewayTenantWriterUserID, whose custom
//     role carries aigateway:read and aigateway:write but NOT
//     aigateway:manage_platform) writes its own tenant's BYOK credential
//     and reads which scope answers, but is refused the platform-wide
//     write, while DemoOwnerUserID (BuiltinRoleOwner) is not.
//   - the SSRF guard is real on the composed stack: a tenant BYOK
//     write whose baseUrl names a loopback endpoint (the second fake
//     OpenAI-compatible server standing in for the platform's intranet) is
//     refused at creation time with aigateway.base_url_blocked and never
//     stored -- the subsequent real chat calls and image jobs keep
//     answering through the boot-time platform credential, and the refused
//     endpoint never receives a single request. A write naming a
//     PUBLIC-shaped endpoint still lands, and the read surface -- the same
//     CredentialService.Resolve the call path uses -- answers with the
//     tenant's own row, preserving the legitimate arbitrary-public-vendor
//     capability (a platform-declared whitelist stays unbuilt).
//
// None of the legs needs a live vendor API key: the fake OpenAI-compatible
// endpoints stand in for the real ones exactly as consult_flow_test.go and
// smilesim_flow_test.go already establish. The boot-time platform
// credential is written by BuildServer itself under cfg.AIGatewayBaseURL
// (an operator-chosen platform default, deliberately outside the SSRF
// guard's tenant-scope boundary -- go/ai-gateway/ssrf.go's file header).
import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/examples/reference-app/internal/app"

	aigateway "github.com/vislake/speed/go/ai-gateway"
)

// aiGatewayCredentialRequest issues method against an ai-gateway
// credential path on srv as the acting demo user -- under the tenant the
// given bearer token resolves, which is also the tenant the write or read
// is scoped to. It is storageRequest with the ai-gateway surface's JSON
// content type defaulted; body may be nil for the body-less GET.
func aiGatewayCredentialRequest(t *testing.T, srv *httptest.Server, method, path, token, user, body string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	return storageRequest(t, srv, method, path, token, user, "application/json", reader)
}

// assertAIGatewayCredentialAnswer reads resp, requires it to be the
// ai-gateway credential surface's 200 answer carrying provider and exactly
// wantScope (the CredentialScope the write or resolution landed at), and
// requires baseUrl to be present and equal wantBaseURL -- or absent
// entirely when wantBaseURL is empty. It additionally requires the raw
// body to contain no apiKey field at all: the surface's write-only rule,
// on every one of its operations.
func assertAIGatewayCredentialAnswer(t *testing.T, resp *http.Response, what, provider, wantScope, wantBaseURL string) {
	t.Helper()
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: read body: %v", what, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status = %d, want %d; body = %s", what, resp.StatusCode, http.StatusOK, raw)
	}
	if bytes.Contains(raw, []byte(`"apiKey"`)) {
		t.Fatalf("%s: response echoes an api key: %s", what, raw)
	}
	var out struct {
		Provider string  `json:"provider"`
		Scope    string  `json:"scope"`
		BaseURL  *string `json:"baseUrl"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: decoding %s: %v", what, raw, err)
	}
	if out.Provider != provider {
		t.Fatalf("%s: provider = %q, want %q", what, out.Provider, provider)
	}
	if out.Scope != wantScope {
		t.Fatalf("%s: scope = %q, want %q; body = %s", what, out.Scope, wantScope, raw)
	}
	switch {
	case wantBaseURL == "" && out.BaseURL != nil:
		t.Fatalf("%s: baseUrl = %q present, want omitted entirely", what, *out.BaseURL)
	case wantBaseURL != "" && (out.BaseURL == nil || *out.BaseURL != wantBaseURL):
		t.Fatalf("%s: baseUrl = %v, want %q", what, out.BaseURL, wantBaseURL)
	}
}

// TestAIGatewayCredentialWrites_TwoTierGateOnTheComposedStack is the
// permission-gating proof, run through the real composed stack: the
// platform-scoped write (PUT .../platform) is gated on
// aigateway.PermissionManagePlatform, a materially more privileged
// permission than the tenant-scoped write (PUT .../tenant) and read (GET)
// are gated on, and the two never interchange -- see go/ai-gateway/
// module.go's doc comment on the two write permissions for why they are
// deliberately distinct.
func TestAIGatewayCredentialWrites_TwoTierGateOnTheComposedStack(t *testing.T) {
	srv, cfg, _ := buildTestServer(t)
	// The token signs a real account into tenant-acme; the demo user header
	// then names which seeded demo grant the gate decides the request
	// against (internal/app/demo_subject.go's seedDemoGrants). No AIGateway* config keys
	// are set, so BuildServer writes no platform credential at boot -- the
	// gate decides every request before the handler's own validation ever
	// runs, which is exactly what this test isolates.
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "aigw-gate-owner")

	credentialPath := app.AiGatewayRoutePath + "/credentials/" + aigateway.ProviderOpenAICompatible
	tenantPath := credentialPath + "/tenant"
	platformPath := credentialPath + "/platform"

	// The tenant-writer actor may write its OWN tenant's BYOK credential...
	resp := aiGatewayCredentialRequest(t, srv, http.MethodPut, tenantPath, acmeToken,
		app.DemoAIGatewayTenantWriterUserID, `{"apiKey":"sk-acme-tenant-writer"}`)
	assertAIGatewayCredentialAnswer(t, resp, "tenant BYOK write as the tenant-writer actor",
		aigateway.ProviderOpenAICompatible, string(aigateway.CredentialScopeTenant), "")

	// ...and may read which scope answers (GET carries no scope suffix --
	// the read reports whichever scope Resolve actually answers with)...
	resp = aiGatewayCredentialRequest(t, srv, http.MethodGet, credentialPath, acmeToken,
		app.DemoAIGatewayTenantWriterUserID, "")
	assertAIGatewayCredentialAnswer(t, resp, "read as the tenant-writer actor",
		aigateway.ProviderOpenAICompatible, string(aigateway.CredentialScopeTenant), "")

	// ...but the platform-wide write is REFUSED: its role holds
	// aigateway:read and aigateway:write and nothing else, and the router
	// gate answers with rbac's own 403 rather than with any ai-gateway
	// error, proving the refusal is authorization, not validation.
	resp = aiGatewayCredentialRequest(t, srv, http.MethodPut, platformPath, acmeToken,
		app.DemoAIGatewayTenantWriterUserID, `{"apiKey":"sk-must-not-land","baseUrl":"https://platform.invalid/v1"}`)
	assertPermissionDenied(t, resp, "platform-wide write as the tenant-writer actor")

	// The owner (BuiltinRoleOwner carries every declared permission) is not
	// refused the platform-wide write.
	resp = aiGatewayCredentialRequest(t, srv, http.MethodPut, platformPath, acmeToken,
		app.DemoOwnerUserID, `{"apiKey":"sk-platform-owner","baseUrl":"https://platform.example.com/v1"}`)
	assertAIGatewayCredentialAnswer(t, resp, "platform-wide write as the owner",
		aigateway.ProviderOpenAICompatible, string(aigateway.CredentialScopeSystem), "https://platform.example.com/v1")

	// The owner's tenant read still answers with the tenant's own BYOK row
	// first -- the write above only set the platform fallback.
	resp = aiGatewayCredentialRequest(t, srv, http.MethodGet, credentialPath, acmeToken,
		app.DemoOwnerUserID, "")
	assertAIGatewayCredentialAnswer(t, resp, "read as the owner",
		aigateway.ProviderOpenAICompatible, string(aigateway.CredentialScopeTenant), "")
}

// TestAIGatewayCredential_TenantBYOKWrite_InternalBaseURLRefusedBySSRFGuard
// is the SSRF guard's chat-provider proof on the composed stack: a tenant
// BYOK credential naming a loopback fake endpoint would let the next real
// consult call reach that endpoint presenting the tenant key -- a tenant
// writes an address, the server dials it from the platform's network. The
// guard refuses the loopback write at creation time and stores it
// nowhere, so subsequent real calls keep answering through the boot-time
// platform credential and the refused endpoint never sees a request; a
// write naming a public-shaped endpoint still lands, and the read surface
// (the same Resolve the call path uses) answers with the tenant's row.
func TestAIGatewayCredential_TenantBYOKWrite_InternalBaseURLRefusedBySSRFGuard(t *testing.T) {
	const platformReply = "The platform default answers this consultation."
	const tenantReply = "The tenant's own BYOK model answers this consultation."
	fakePlatform := newFakeOpenAICompatibleServer(t, platformReply)
	fakeTenant := newFakeOpenAICompatibleServer(t, tenantReply)

	srv, cfg := buildConsultTestServer(t, fakePlatform)
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "aigw-byok-owner")
	const noteText = "Patient reports persistent discomfort under the new crown."
	noteID := createNoteAs(t, srv, acmeToken, noteText)

	credentialPath := app.AiGatewayRoutePath + "/credentials/" + aigateway.ProviderOpenAICompatible
	tenantPath := credentialPath + "/tenant"

	// Leg one: with only the boot-time platform credential present, the
	// consult call reaches fakePlatform presenting the boot key, and the
	// read surface reports the system scope answering. (The boot-time
	// platform credential is the operator's own default, deliberately
	// outside the guard's tenant-scope boundary -- see the file header.)
	resp := consultSuggestRequest(t, srv, acmeToken, noteID)
	assertConsultSuggestion(t, resp, platformReply, "consult call before the tenant BYOK write")
	if fakePlatform.lastAuthorization != "Bearer sk-test-consult-key" {
		t.Fatalf("fake platform server saw Authorization %q, want the boot-time platform key %q on the wire",
			fakePlatform.lastAuthorization, "Bearer sk-test-consult-key")
	}
	resp = aiGatewayCredentialRequest(t, srv, http.MethodGet, credentialPath, acmeToken, app.DemoOwnerUserID, "")
	assertAIGatewayCredentialAnswer(t, resp, "read before the tenant BYOK write",
		aigateway.ProviderOpenAICompatible, string(aigateway.CredentialScopeSystem), fakePlatform.URL)

	// Leg two -- the guard: a tenant BYOK write whose baseUrl names a
	// LOOPBACK endpoint (the fake stands in for the platform's intranet) is
	// refused at creation time with aigateway.base_url_blocked. The literal
	// address the caller typed may be echoed back in the refusal's ip
	// param -- zero disclosure.
	const tenantKey = "sk-tenant-acme-byok-key"
	resp = aiGatewayCredentialRequest(t, srv, http.MethodPut, tenantPath, acmeToken, app.DemoOwnerUserID,
		`{"apiKey":"`+tenantKey+`","baseUrl":"`+fakeTenant.URL+`"}`)
	assertAIGatewayCredentialRefused(t, resp, "tenant BYOK write naming a loopback endpoint",
		http.StatusBadRequest, aigateway.ErrBaseURLBlocked.Code, map[string]any{"ip": "127.0.0.1"})

	// ...and a write naming the SAME endpoint through a hostname whose DNS
	// answer is blocked is refused without the refusal disclosing the
	// resolved address -- no ip param -- so the refusal can never be used
	// as an internal-DNS reconnaissance oracle (ErrBaseURLBlocked's own doc
	// comment).
	localhostURL := strings.Replace(fakeTenant.URL, "127.0.0.1", "localhost", 1)
	resp = aiGatewayCredentialRequest(t, srv, http.MethodPut, tenantPath, acmeToken, app.DemoOwnerUserID,
		`{"apiKey":"`+tenantKey+`","baseUrl":"`+localhostURL+`"}`)
	assertAIGatewayCredentialRefused(t, resp, "tenant BYOK write naming a blocked hostname",
		http.StatusBadRequest, aigateway.ErrBaseURLBlocked.Code, nil)

	// Nothing was stored: the very same consult call still answers through
	// the platform row presenting the boot key, the read surface still
	// reports the system scope, and the refused endpoint never received a
	// single request.
	resp = consultSuggestRequest(t, srv, acmeToken, noteID)
	assertConsultSuggestion(t, resp, platformReply, "consult call after the refused tenant BYOK writes")
	if fakePlatform.lastAuthorization != "Bearer sk-test-consult-key" {
		t.Fatalf("fake platform server saw Authorization %q after the refused writes, want the boot-time platform key %q -- the refusals must store nothing",
			fakePlatform.lastAuthorization, "Bearer sk-test-consult-key")
	}
	if fakeTenant.lastAuthorization != "" {
		t.Fatalf("fake tenant server saw Authorization %q, want no request at all -- a refused destination must never be dialed",
			fakeTenant.lastAuthorization)
	}
	resp = aiGatewayCredentialRequest(t, srv, http.MethodGet, credentialPath, acmeToken, app.DemoOwnerUserID, "")
	assertAIGatewayCredentialAnswer(t, resp, "read after the refused tenant BYOK writes",
		aigateway.ProviderOpenAICompatible, string(aigateway.CredentialScopeSystem), fakePlatform.URL)

	// Leg three -- the capability the guard deliberately preserves: a
	// tenant BYOK write naming a PUBLIC-shaped endpoint still lands, and
	// the read surface -- the same Resolve per-call credential resolution
	// uses -- now answers with the tenant's own row. (No consult call is
	// made after this write: the public-shaped endpoint is never dialed in
	// a test.)
	resp = aiGatewayCredentialRequest(t, srv, http.MethodPut, tenantPath, acmeToken, app.DemoOwnerUserID,
		`{"apiKey":"`+tenantKey+`","baseUrl":"https://93.184.216.34/v1"}`)
	assertAIGatewayCredentialAnswer(t, resp, "tenant BYOK write naming a public-shaped endpoint",
		aigateway.ProviderOpenAICompatible, string(aigateway.CredentialScopeTenant), "https://93.184.216.34/v1")
	resp = aiGatewayCredentialRequest(t, srv, http.MethodGet, credentialPath, acmeToken, app.DemoOwnerUserID, "")
	assertAIGatewayCredentialAnswer(t, resp, "read after the public-shaped tenant BYOK write",
		aigateway.ProviderOpenAICompatible, string(aigateway.CredentialScopeTenant), "https://93.184.216.34/v1")
}

// assertAIGatewayCredentialRefused reads resp, requires it to be the
// ai-gateway credential surface's structured {code, params} error answer
// with exactly wantStatus and wantCode, and -- when wantParams is non-nil
// -- requires every entry of wantParams to be present with that value. A
// nil wantParams requires the error to carry NO params at all: the shape
// the no-IP-echo rule demands of the resolution path's blocked refusal.
func assertAIGatewayCredentialRefused(t *testing.T, resp *http.Response, what string, wantStatus int, wantCode string, wantParams map[string]any) {
	t.Helper()
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: read body: %v", what, err)
	}
	var out struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: decoding %s: %v", what, raw, err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s: status = %d, want %d; body = %s", what, resp.StatusCode, wantStatus, raw)
	}
	if out.Code != wantCode {
		t.Fatalf("%s: code = %q, want %q; body = %s", what, out.Code, wantCode, raw)
	}
	if wantParams == nil {
		if len(out.Params) != 0 {
			t.Fatalf("%s: refusal carries params %v, want none -- a resolution-path refusal must not disclose the resolved address", what, out.Params)
		}
		return
	}
	for k, wantV := range wantParams {
		if gotV, present := out.Params[k]; !present || gotV != wantV {
			t.Fatalf("%s: params[%q] = %v, want %v; body = %s", what, k, out.Params[k], wantV, raw)
		}
	}
}

// TestAIGatewayCredential_TenantBYOKWrite_InternalImageBaseURLRefusedBySSRFGuard
// is the guard's image-provider proof, run through the async smile-
// simulation pipeline: a tenant BYOK write for the image provider naming a
// loopback images endpoint is refused at creation time and stored nowhere
// (the next job still reaches the boot-time fakePlatform presenting the
// platform key, and the refused endpoint never sees a request), while a
// write naming a public-shaped endpoint still lands and the read surface
// answers with the tenant's row -- the image-side twin of the chat-side
// test above.
func TestAIGatewayCredential_TenantBYOKWrite_InternalImageBaseURLRefusedBySSRFGuard(t *testing.T) {
	fakePlatform := newFakeOpenAIImageServer(t)
	fakeTenant := newFakeOpenAIImageServer(t)

	srv, cfg := buildSmileSimTestServer(t, fakePlatform)
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "aigw-img-byok-owner")

	// Leg one: the boot-time image platform credential serves the first job.
	photo := uploadAndComplete(t, srv, acmeToken, jpegWithExif(t), "")
	final := smileSimulateAndWait(t, srv, acmeToken, photo, time.Now().Add(5*time.Second))
	if status, _ := final["status"].(string); status != "succeeded" {
		t.Fatalf("first job's final status = %v, want \"succeeded\"; body = %+v", final["status"], final)
	}
	if fakePlatform.lastAuthorization != "Bearer sk-test-smilesim-key" {
		t.Fatalf("fake platform image server saw Authorization %q, want the boot-time platform key %q on the wire",
			fakePlatform.lastAuthorization, "Bearer sk-test-smilesim-key")
	}
	if fakePlatform.lastModel != "dall-e-3" {
		t.Fatalf("fake platform image server saw model %q, want the routed vendor model %q",
			fakePlatform.lastModel, "dall-e-3")
	}
	platformCalls := fakePlatform.requests

	// Leg two -- the guard: a tenant BYOK write for the image provider
	// naming a LOOPBACK images endpoint is refused with
	// aigateway.base_url_blocked. (A write to one provider's row never
	// affects the other's -- the chat row written by the sibling test's own
	// server lives in that server's database, and this server's database
	// never saw it.)
	const tenantKey = "sk-tenant-acme-img-byok-key"
	imageCredentialPath := app.AiGatewayRoutePath + "/credentials/" + aigateway.ProviderOpenAICompatibleImage
	resp := aiGatewayCredentialRequest(t, srv, http.MethodPut, imageCredentialPath+"/tenant", acmeToken, app.DemoOwnerUserID,
		`{"apiKey":"`+tenantKey+`","baseUrl":"`+fakeTenant.URL+`"}`)
	assertAIGatewayCredentialRefused(t, resp, "image tenant BYOK write naming a loopback endpoint",
		http.StatusBadRequest, aigateway.ErrBaseURLBlocked.Code, map[string]any{"ip": "127.0.0.1"})

	// Nothing was stored: the next job's vendor call still lands on
	// fakePlatform presenting the platform key, and the refused endpoint
	// never receives a request.
	secondPhoto := uploadAndComplete(t, srv, acmeToken, jpegWithExif(t), "")
	final = smileSimulateAndWait(t, srv, acmeToken, secondPhoto, time.Now().Add(5*time.Second))
	if status, _ := final["status"].(string); status != "succeeded" {
		t.Fatalf("second job's final status = %v, want \"succeeded\"; body = %+v", final["status"], final)
	}
	if fakePlatform.requests != platformCalls+1 {
		t.Fatalf("fake platform image server received %d calls after the refused write, want %d -- the refusal must store nothing",
			fakePlatform.requests, platformCalls+1)
	}
	if fakePlatform.lastAuthorization != "Bearer sk-test-smilesim-key" {
		t.Fatalf("fake platform image server saw Authorization %q after the refused write, want the boot-time platform key %q on the wire",
			fakePlatform.lastAuthorization, "Bearer sk-test-smilesim-key")
	}
	if fakePlatform.lastModel != "dall-e-3" {
		t.Fatalf("fake platform image server saw model %q, want the routed vendor model %q",
			fakePlatform.lastModel, "dall-e-3")
	}
	if fakeTenant.requests != 0 {
		t.Fatalf("fake tenant image server received %d requests, want 0 -- a refused destination must never be dialed", fakeTenant.requests)
	}

	// Leg three -- the capability the guard deliberately preserves: a
	// tenant BYOK write naming a PUBLIC-shaped endpoint still lands, and
	// the read surface answers with the tenant's own row. (No further job
	// is run after this write: the public-shaped endpoint is never dialed
	// in a test.)
	resp = aiGatewayCredentialRequest(t, srv, http.MethodPut, imageCredentialPath+"/tenant", acmeToken, app.DemoOwnerUserID,
		`{"apiKey":"`+tenantKey+`","baseUrl":"https://93.184.216.34/v1"}`)
	assertAIGatewayCredentialAnswer(t, resp, "image tenant BYOK write naming a public-shaped endpoint",
		aigateway.ProviderOpenAICompatibleImage, string(aigateway.CredentialScopeTenant), "https://93.184.216.34/v1")
	resp = aiGatewayCredentialRequest(t, srv, http.MethodGet, imageCredentialPath, acmeToken, app.DemoOwnerUserID, "")
	assertAIGatewayCredentialAnswer(t, resp, "read after the public-shaped image BYOK write",
		aigateway.ProviderOpenAICompatibleImage, string(aigateway.CredentialScopeTenant), "https://93.184.216.34/v1")
}

// assertConsultSuggestion reads a consult-suggest response, requires it to
// be the 200 answer carrying exactly wantSuggestion, and closes it.
func assertConsultSuggestion(t *testing.T, resp *http.Response, wantSuggestion, what string) {
	t.Helper()
	defer resp.Body.Close()

	var out struct {
		Suggestion string `json:"suggestion"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s: decode response: %v", what, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status = %d, want %d; body = %+v", what, resp.StatusCode, http.StatusOK, out)
	}
	if out.Suggestion != wantSuggestion {
		t.Fatalf("%s: suggestion = %q, want %q -- the answer must come from whichever fake endpoint the resolved credential names",
			what, out.Suggestion, wantSuggestion)
	}
}
