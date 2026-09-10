package flowtests

// authn_enterprise_oidc_login_start_test.go pins the closure of the
// enterprise-OIDC login-start gap AuthnAPIPath's own doc comment
// (go/app/kernel.go) records (the CONFIRMED GAP the composition fix below closes):
// an anonymous request to /api/v1/authn/social/oidc:<tenant>/authorize --
// the login-start step of a tenant that configured enterprise OIDC -- must
// reach authn's own Handler instead of being refused by
// tenancy.Middleware. Before the fix, authn's whole subtree sat behind the
// tenancy chain with per-literal allowlist entries, and the enterprise
// channel's provider value is the DYNAMIC "oidc:<tenant>" string
// (authn.ProviderOIDCPrefix + a tenant id) no literal allowlist entry can
// enumerate -- so tenancy's fail-closed default refused every such request
// with 403 tenancy.tenant_unresolved before authn ever saw it, and
// enabling enterprise OIDC for a real tenant would have silently needed a
// code change in this file. Post-fix, topMux dispatches the whole
// AuthnAPIPath subtree straight from authn.Middleware's own output (the
// AdminRoutePath branch's shape), tenancy never sees it, and authn's own
// Handler is the authority on who may call what.
//
// go/authn's enterprise relying party is service-level today -- no authn
// HTTP operation routes the "oidc:" provider into SSOService yet
// (go/authn/handler.go's "the SSO service has no mounted HTTP surface";
// go/authn/AGENTS.md's Service.SSO() row) -- so the reaching-proof here is
// the answer's provenance: the request is answered with authn's OWN coded
// refusal (authn.provider_unknown, because no social channel is registered
// under that name), never with the tenancy layer's 403. The day authn
// ships the oidc-routing half of its social authorize operation, this same
// request reaches that logic with no reference-app change -- which is the
// whole point of removing the gate in front of it rather than widening the
// gate.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/app/demo"
	"github.com/vislake/speed/go/authn"
)

// errEnvelope is the {code, params} wire shape both authn's and tenancy's
// error responses share; only the code field is asserted here.
type oidcErrEnvelope struct {
	Code string `json:"code"`
}

// TestAuthnEnterpriseOIDCLoginStart_ReachesAuthnNotTenancy drives the real
// composed stack (buildTestServer's output -- the exact handler main.go
// serves) through the enterprise channel's login-start request, anonymous
// exactly as the first step of a sign-in is, and asserts the answer is
// authn's own. Failing before the fix: tenancy.Middleware's fail-closed
// default refused the request -- no allowlist entry can name a per-tenant
// "oidc:<tenant>" literal -- with 403 tenancy.tenant_unresolved before
// authn's Handler ever ran.
func TestAuthnEnterpriseOIDCLoginStart_ReachesAuthnNotTenancy(t *testing.T) {
	srv, _, _ := buildTestServer(t)

	// The enterprise channel's provider value is the synthetic
	// "oidc:<tenant>" name (authn.ProviderOIDCPrefix + a tenant id) that no
	// literal tenancy allowlist entry could ever enumerate -- the property
	// that made the pre-fix composition a dead end for this path.
	provider := url.PathEscape(authn.ProviderOIDCPrefix + string(demo.DemoSingleTenantID))
	authorizeURL := srv.URL + "/api/v1/authn/social/" + provider +
		"/authorize?redirect_uri=" + url.QueryEscape("https://app.example.internal/callback")

	resp := authnJSON(t, srv.Client(), http.MethodGet, authorizeURL, "", nil, nil)
	defer resp.Body.Close()

	var envelope oidcErrEnvelope
	if decodeErr := json.NewDecoder(resp.Body).Decode(&envelope); decodeErr != nil {
		t.Fatalf("decode %s response: %v", authorizeURL, decodeErr)
	}
	if envelope.Code == "tenancy.tenant_unresolved" {
		t.Fatalf("login-start answered %q: tenancy refused the request before authn's handler ran (the pre-fix gap)", envelope.Code)
	}
	if envelope.Code != "authn.provider_unknown" {
		t.Fatalf("login-start code = %q, want %q -- the request must be answered by authn's own handler, not by a middleware in front of it", envelope.Code, "authn.provider_unknown")
	}
}

// TestAuthnProtectedOperation_AnonymousAnsweredByAuthn pins the composed
// answer a protected authn operation gives an anonymous caller after the
// subtree moved out of the tenancy chain: authn's own per-operation
// requirePrincipal gate answers authn.authentication_required (401) --
// the module's documented answer for its own surface -- where tenancy's
// fail-closed default used to pre-empt it with 403 tenancy.tenant_unresolved.
func TestAuthnProtectedOperation_AnonymousAnsweredByAuthn(t *testing.T) {
	srv, _, _ := buildTestServer(t)

	resp := authnJSON(t, srv.Client(), http.MethodGet, srv.URL+"/api/v1/authn/me", "", nil, nil)
	defer resp.Body.Close()

	var envelope oidcErrEnvelope
	if decodeErr := json.NewDecoder(resp.Body).Decode(&envelope); decodeErr != nil {
		t.Fatalf("decode GET /api/v1/authn/me response: %v", decodeErr)
	}
	if envelope.Code != "authn.authentication_required" {
		t.Fatalf("anonymous /api/v1/authn/me code = %q, want %q -- authn's own gate must answer for its own surface", envelope.Code, "authn.authentication_required")
	}
}
