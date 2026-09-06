package main

// ai_gateway_flow_test.go drives go/ai-gateway's round-3 credential-write
// HTTP surface -- the module's own OpenAPI fragment mounted at
// aiGatewayRoutePath, gated by demoRouteGuards[aiGatewayRoutePath] through
// aiGatewayPermissionFor's three-permission selector -- end to end through
// the composed HTTP stack: the real authn+tenancy middleware chain, a real
// temp-file SQLite database carrying ai-gateway's real migrations, and the
// real rbac gate deciding the requests against seedDemoGrants' demo roles.
//
// Three legs cover the round's acceptance shape:
//
//   - the two-tier permission gate is real on the composed stack: a
//     tenant-scoped actor (demoAIGatewayTenantWriterUserID, whose custom
//     role carries aigateway:read and aigateway:write but NOT
//     aigateway:manage_platform) writes its own tenant's BYOK credential
//     and reads which scope answers, but is refused the platform-wide
//     write, while demoOwnerUserID (BuiltinRoleOwner) is not.
//   - a tenant BYOK credential written over HTTP genuinely redirects
//     subsequent real chat calls: the boot-time platform credential sends
//     the consult route's first call to one fake OpenAI-compatible server
//     with the platform key on the wire; after a tenant-scope write names a
//     second fake server and a tenant key, the next call reaches THAT
//     server presenting THAT key -- the stored row, not the platform
//     default, is what per-call credential resolution answers with.
//   - the identical redirect is proven for the image provider through the
//     async smile-simulation pipeline: a second fake images endpoint,
//     reached only after the tenant-scope write, receives the subsequent
//     job with the tenant key, while the boot-time fake is never called
//     again.
//
// None of the legs needs a live vendor API key: the two fake
// OpenAI-compatible endpoints stand in for the real ones exactly as
// consult_flow_test.go and smilesim_flow_test.go already establish.
import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// TestAIGatewayCredentialWrites_TwoTierGateOnTheComposedStack is this
// round's permission-gating proof, run through the real composed stack: the
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
	// against (demo_subject.go's seedDemoGrants). No AIGateway* config keys
	// are set, so buildServer writes no platform credential at boot -- the
	// gate decides every request before the handler's own validation ever
	// runs, which is exactly what this test isolates.
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "aigw-gate-owner")

	credentialPath := aiGatewayRoutePath + "/credentials/" + aigateway.ProviderOpenAICompatible
	tenantPath := credentialPath + "/tenant"
	platformPath := credentialPath + "/platform"

	// The tenant-writer actor may write its OWN tenant's BYOK credential...
	resp := aiGatewayCredentialRequest(t, srv, http.MethodPut, tenantPath, acmeToken,
		demoAIGatewayTenantWriterUserID, `{"apiKey":"sk-acme-tenant-writer"}`)
	assertAIGatewayCredentialAnswer(t, resp, "tenant BYOK write as the tenant-writer actor",
		aigateway.ProviderOpenAICompatible, string(aigateway.CredentialScopeTenant), "")

	// ...and may read which scope answers (GET carries no scope suffix --
	// the read reports whichever scope Resolve actually answers with)...
	resp = aiGatewayCredentialRequest(t, srv, http.MethodGet, credentialPath, acmeToken,
		demoAIGatewayTenantWriterUserID, "")
	assertAIGatewayCredentialAnswer(t, resp, "read as the tenant-writer actor",
		aigateway.ProviderOpenAICompatible, string(aigateway.CredentialScopeTenant), "")

	// ...but the platform-wide write is REFUSED: its role holds
	// aigateway:read and aigateway:write and nothing else, and the router
	// gate answers with rbac's own 403 rather than with any ai-gateway
	// error, proving the refusal is authorization, not validation.
	resp = aiGatewayCredentialRequest(t, srv, http.MethodPut, platformPath, acmeToken,
		demoAIGatewayTenantWriterUserID, `{"apiKey":"sk-must-not-land","baseUrl":"https://platform.invalid/v1"}`)
	assertPermissionDenied(t, resp, "platform-wide write as the tenant-writer actor")

	// The owner (BuiltinRoleOwner carries every declared permission) is not
	// refused the platform-wide write.
	resp = aiGatewayCredentialRequest(t, srv, http.MethodPut, platformPath, acmeToken,
		demoOwnerUserID, `{"apiKey":"sk-platform-owner","baseUrl":"https://platform.example.com/v1"}`)
	assertAIGatewayCredentialAnswer(t, resp, "platform-wide write as the owner",
		aigateway.ProviderOpenAICompatible, string(aigateway.CredentialScopeSystem), "https://platform.example.com/v1")

	// The owner's tenant read still answers with the tenant's own BYOK row
	// first -- the write above only set the platform fallback.
	resp = aiGatewayCredentialRequest(t, srv, http.MethodGet, credentialPath, acmeToken,
		demoOwnerUserID, "")
	assertAIGatewayCredentialAnswer(t, resp, "read as the owner",
		aigateway.ProviderOpenAICompatible, string(aigateway.CredentialScopeTenant), "")
}

// TestAIGatewayCredential_TenantBYOKWriteRedirectsRealChatCalls is the
// round's mandatory write-then-resolve proof for the chat provider: the
// boot-time platform credential (buildConsultTestServer pointing cfg's
// AIGatewayBaseURL/APIKey at fakePlatform) actually sends the first consult
// call to fakePlatform with the platform key on the wire; a tenant BYOK
// credential written over this round's own HTTP surface (naming
// fakeTenant's base URL and a tenant key) then redirects the next consult
// call to fakeTenant presenting the tenant key -- proving the HTTP write
// changed what subsequent real calls resolve and use, and that no response
// ever echoed the key back.
func TestAIGatewayCredential_TenantBYOKWriteRedirectsRealChatCalls(t *testing.T) {
	const platformReply = "The platform default answers this consultation."
	const tenantReply = "The tenant's own BYOK model answers this consultation."
	fakePlatform := newFakeOpenAICompatibleServer(t, platformReply)
	fakeTenant := newFakeOpenAICompatibleServer(t, tenantReply)

	srv, cfg := buildConsultTestServer(t, fakePlatform)
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "aigw-byok-owner")
	const noteText = "Patient reports persistent discomfort under the new crown."
	noteID := createNoteAs(t, srv, acmeToken, noteText)

	credentialPath := aiGatewayRoutePath + "/credentials/" + aigateway.ProviderOpenAICompatible

	// Leg one: with only the boot-time platform credential present, the
	// consult call reaches fakePlatform presenting the boot key, and the
	// read surface reports the system scope answering.
	resp := consultSuggestRequest(t, srv, acmeToken, noteID)
	assertConsultSuggestion(t, resp, platformReply, "consult call before the tenant BYOK write")
	if fakePlatform.lastAuthorization != "Bearer sk-test-consult-key" {
		t.Fatalf("fake platform server saw Authorization %q, want the boot-time platform key %q on the wire",
			fakePlatform.lastAuthorization, "Bearer sk-test-consult-key")
	}
	resp = aiGatewayCredentialRequest(t, srv, http.MethodGet, credentialPath, acmeToken, demoOwnerUserID, "")
	assertAIGatewayCredentialAnswer(t, resp, "read before the tenant BYOK write",
		aigateway.ProviderOpenAICompatible, string(aigateway.CredentialScopeSystem), fakePlatform.URL)

	// The tenant BYOK write over this round's HTTP surface names a DIFFERENT
	// endpoint and a tenant key.
	const tenantKey = "sk-tenant-acme-byok-key"
	resp = aiGatewayCredentialRequest(t, srv, http.MethodPut, credentialPath+"/tenant", acmeToken, demoOwnerUserID,
		`{"apiKey":"`+tenantKey+`","baseUrl":"`+fakeTenant.URL+`"}`)
	assertAIGatewayCredentialAnswer(t, resp, "tenant BYOK write",
		aigateway.ProviderOpenAICompatible, string(aigateway.CredentialScopeTenant), fakeTenant.URL)

	// Leg two: the very same consult call now resolves the tenant's own row
	// -- fakeTenant receives it, presenting the tenant key -- and the read
	// surface agrees the tenant scope now answers.
	resp = consultSuggestRequest(t, srv, acmeToken, noteID)
	assertConsultSuggestion(t, resp, tenantReply, "consult call after the tenant BYOK write")
	if fakeTenant.lastAuthorization != "Bearer "+tenantKey {
		t.Fatalf("fake tenant server saw Authorization %q, want the tenant's own key %q on the wire",
			fakeTenant.lastAuthorization, "Bearer "+tenantKey)
	}
	resp = aiGatewayCredentialRequest(t, srv, http.MethodGet, credentialPath, acmeToken, demoOwnerUserID, "")
	assertAIGatewayCredentialAnswer(t, resp, "read after the tenant BYOK write",
		aigateway.ProviderOpenAICompatible, string(aigateway.CredentialScopeTenant), fakeTenant.URL)
}

// TestAIGatewayCredential_TenantBYOKWriteRedirectsRealImageCalls is the
// write-then-resolve proof's image-provider mirror, run through the async
// smile-simulation pipeline: with only the boot-time image platform
// credential (buildSmileSimTestServer's cfg.AIGatewayImageBaseURL/APIKey)
// present, the first simulation job's vendor call reaches fakePlatform;
// after a tenant BYOK credential for the image provider names a second
// fake endpoint and a tenant key, the next job's vendor call reaches THAT
// endpoint presenting the tenant key, and fakePlatform is never called
// again.
func TestAIGatewayCredential_TenantBYOKWriteRedirectsRealImageCalls(t *testing.T) {
	fakePlatform := newFakeOpenAIImageServer(t)
	fakeTenant := newFakeOpenAIImageServer(t)

	srv, cfg := buildSmileSimTestServer(t, fakePlatform)
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "aigw-img-byok-owner")

	// Leg one: the boot-time platform credential serves the first job.
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

	// The tenant BYOK write over this round's HTTP surface names a DIFFERENT
	// images endpoint and a tenant key. (A write to one provider's row never
	// affects the other's -- the chat row written by the sibling test's own
	// server lives in that server's database, and this server's database
	// never saw it.)
	const tenantKey = "sk-tenant-acme-img-byok-key"
	imageCredentialPath := aiGatewayRoutePath + "/credentials/" + aigateway.ProviderOpenAICompatibleImage
	resp := aiGatewayCredentialRequest(t, srv, http.MethodPut, imageCredentialPath+"/tenant", acmeToken, demoOwnerUserID,
		`{"apiKey":"`+tenantKey+`","baseUrl":"`+fakeTenant.URL+`"}`)
	assertAIGatewayCredentialAnswer(t, resp, "image tenant BYOK write",
		aigateway.ProviderOpenAICompatibleImage, string(aigateway.CredentialScopeTenant), fakeTenant.URL)

	// Leg two: the next job's vendor call lands on fakeTenant presenting the
	// tenant key, and fakePlatform is never called again.
	secondPhoto := uploadAndComplete(t, srv, acmeToken, jpegWithExif(t), "")
	final = smileSimulateAndWait(t, srv, acmeToken, secondPhoto, time.Now().Add(5*time.Second))
	if status, _ := final["status"].(string); status != "succeeded" {
		t.Fatalf("second job's final status = %v, want \"succeeded\"; body = %+v", final["status"], final)
	}
	if fakeTenant.lastAuthorization != "Bearer "+tenantKey {
		t.Fatalf("fake tenant image server saw Authorization %q, want the tenant's own key %q on the wire",
			fakeTenant.lastAuthorization, "Bearer "+tenantKey)
	}
	if fakeTenant.lastModel != "dall-e-3" {
		t.Fatalf("fake tenant image server saw model %q, want the routed vendor model %q",
			fakeTenant.lastModel, "dall-e-3")
	}
	if fakePlatform.requests != platformCalls {
		t.Fatalf("fake platform image server received %d calls after the tenant BYOK write, want it to stay at %d -- the second job must resolve the tenant's own row",
			fakePlatform.requests, platformCalls)
	}
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
