package flowtests

// apikey_authenticate_flow_test.go is go/integration's
// mandatory-first-consumer proof for the inbound-authentication path: "a
// request bearing a rotated-away or revoked key is refused", driven for
// real, through this app's own composed HTTP
// stack, against a real inbound endpoint (internal/app/
// integration_authenticate.go's IntegrationWhoamiPath) gated by
// integration.AuthMiddleware. It shares apikey_flow_test.go's own helpers
// (apikeyRequest, decodeCreatedAPIKey) for the session-authenticated CRUD
// half of the flow, and never imports go/integration itself, matching that
// file's own wire-shape discipline.
//
// The rate-limit guard's regression lives here too: the same
// route carries its rate-limit guard BEFORE authentication (wired through
// integration.WithAuthenticationGuard), and
// TestBuildServer_APIKeyAuthenticateFlow_ForgedKeyFlood_GuardBudgetExhausted_Answers429
// proves a forged-X-API-Key flood pays that budget and answers 429 once it
// is spent -- never an unbounded database-hit Authenticate per forged
// key.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/app"
	"github.com/vislake/speed/examples/reference-app/internal/app/demo"

	"github.com/vislake/speed/go/ratelimit"
)

// testIntegrationWhoami is the wire shape of integrationWhoamiResponse.
type testIntegrationWhoami struct {
	TenantID  string   `json:"tenant_id"`
	KeyID     string   `json:"key_id"`
	CreatedBy string   `json:"created_by"`
	Scopes    []string `json:"scopes"`
}

// whoamiRequest issues a GET against IntegrationWhoamiPath on srv, presenting
// rawKey as the X-API-Key bearer credential (integration.HeaderAPIKey) --
// never Authorization, which this app's own outer authn.Middleware already
// claims for session bearer tokens (see internal/app/integration_authenticate.go's own
// HeaderAPIKey doc comment for why the two cannot share a header). An empty
// rawKey sends no header at all.
func whoamiRequest(t *testing.T, srv *httptest.Server, rawKey string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, srv.URL+app.IntegrationWhoamiPath, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if rawKey != "" {
		req.Header.Set("X-API-Key", rawKey)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", app.IntegrationWhoamiPath, err)
	}
	return resp
}

// decodeWhoami reads resp, requires its status to be wantStatus, and decodes
// its body as a testIntegrationWhoami wire shape.
func decodeWhoami(t *testing.T, resp *http.Response, wantStatus int, what string) testIntegrationWhoami {
	t.Helper()
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: read body: %v", what, err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s: status = %d, want %d; body = %s", what, resp.StatusCode, wantStatus, body)
	}
	var out testIntegrationWhoami
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("%s: decoding %s: %v", what, body, err)
	}
	return out
}

// TestBuildServer_APIKeyAuthenticateFlow_RotateAndRevokeRefuseTheOldKey
// drives the full property end to end: create a key,
// use it as a bearer credential against IntegrationWhoamiPath (succeeds),
// rotate it, use the OLD raw key value again (refused), use the NEW key
// (succeeds), revoke it, use it again (refused).
func TestBuildServer_APIKeyAuthenticateFlow_RotateAndRevokeRefuseTheOldKey(t *testing.T) {
	srv, cfg, _ := buildTestServer(t)
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "apikey-authn-flow")

	// Create with an empty body, through the ordinary session-authenticated
	// CRUD surface apikey_flow_test.go's own helpers drive -- the identical
	// shape that test uses, since the inbound path changes nothing about how a key is
	// issued.
	createResp := apikeyRequest(t, srv, http.MethodPost, apikeyBasePath, acmeToken, demo.DemoOwnerUserID)
	created := decodeCreatedAPIKey(t, createResp, http.StatusCreated, "create")
	if created.Key == "" {
		t.Fatal("created key carries no raw key")
	}

	// The freshly created key authenticates against the demo whoami route,
	// which requires no session token, no demo header, and no tenant known
	// ahead of the request at all -- Authenticate resolves tenant-acme from
	// the key itself.
	who := decodeWhoami(t, whoamiRequest(t, srv, created.Key), http.StatusOK, "whoami with the fresh key")
	if who.KeyID != created.ID {
		t.Errorf("whoami key_id = %q, want %q", who.KeyID, created.ID)
	}
	if who.CreatedBy != created.CreatedBy {
		t.Errorf("whoami created_by = %q, want %q", who.CreatedBy, created.CreatedBy)
	}

	// No credential at all is refused.
	noCredResp := whoamiRequest(t, srv, "")
	noCredResp.Body.Close()
	if noCredResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("whoami with no credential: status = %d, want %d", noCredResp.StatusCode, http.StatusUnauthorized)
	}

	// A key that was never issued at all is refused identically.
	wrongResp := whoamiRequest(t, srv, "sk_this-was-never-issued")
	wrongResp.Body.Close()
	if wrongResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("whoami with an unknown key: status = %d, want %d", wrongResp.StatusCode, http.StatusUnauthorized)
	}

	// Rotate through the ordinary CRUD surface.
	rotateResp := apikeyRequest(t, srv, http.MethodPost, apikeyBasePath+"/"+created.ID+"/rotate", acmeToken, demo.DemoOwnerUserID)
	rotated := decodeCreatedAPIKey(t, rotateResp, http.StatusOK, "rotate")

	// The OLD raw key value is now refused as a bearer credential -- THE
	// property named as
	// unverifiable without a real inbound endpoint.
	oldKeyResp := whoamiRequest(t, srv, created.Key)
	oldKeyResp.Body.Close()
	if oldKeyResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("whoami with the OLD, rotated-away key: status = %d, want %d", oldKeyResp.StatusCode, http.StatusUnauthorized)
	}

	// The NEW key authenticates in its place.
	whoNew := decodeWhoami(t, whoamiRequest(t, srv, rotated.Key), http.StatusOK, "whoami with the new, rotated key")
	if whoNew.KeyID != rotated.ID {
		t.Errorf("whoami key_id = %q, want %q", whoNew.KeyID, rotated.ID)
	}

	// Revoke the replacement through the ordinary CRUD surface.
	revokeResp := apikeyRequest(t, srv, http.MethodDelete, apikeyBasePath+"/"+rotated.ID, acmeToken, demo.DemoOwnerUserID)
	revokeResp.Body.Close()
	if revokeResp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: status = %d, want %d", revokeResp.StatusCode, http.StatusNoContent)
	}

	// The revoked key is refused identically to a wrong or rotated-away one.
	revokedResp := whoamiRequest(t, srv, rotated.Key)
	revokedResp.Body.Close()
	if revokedResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("whoami with the revoked key: status = %d, want %d", revokedResp.StatusCode, http.StatusUnauthorized)
	}
}

// TestBuildServer_APIKeyAuthenticateFlow_ForgedKeyFlood_GuardBudgetExhausted_Answers429
// is the app-level regression for the corrected rate-limit layering
// (go/integration/middleware.go's "Deliberately separate from HTTPGuard"
// section, and the WithAuthenticationGuard option's own doc comment): the
// whoami route's rate-limit guard must run BEFORE authentication, so
// requests bearing a forged X-API-Key pay its budget and -- once it is
// spent -- answer 429 with no Authenticate call at all. Before the
// correction this route composed the guard behind authentication (the
// classic chain), where a forged key never reached it: every forged request
// cost Service.Authenticate's two database lookups and answered 401,
// charged against no budget, forever.
//
// The test bounds the demo route's budget by overriding the
// IntegrationWhoamiLimits package var (field by field, without naming its
// integration.LayeredLimits type, keeping this file's own never-imports-
// go/integration wire-shape discipline) for the duration of the test:
// BuildServer freezes the var's current value into the guard's
// LayeredLimiter at construction, and no other test in this package runs
// concurrently (nothing here calls t.Parallel), so the override cannot leak
// into a sibling server. With a budget of two global hits per minute, the
// legs read: a valid key authenticates (leg 1, spending one hit); a forged
// key passes the guard with one hit left and is refused by Authenticate
// itself, 401 (leg 2, spending the budget); and every later attempt -- a
// forged key, then the route's OWN valid key -- is refused by the guard,
// 429, before authentication runs. That final leg is the strongest
// app-level "never a DB-hit authenticate" signal available: had the request
// reached Authenticate, the valid key would have answered 200.
func TestBuildServer_APIKeyAuthenticateFlow_ForgedKeyFlood_GuardBudgetExhausted_Answers429(t *testing.T) {
	originalLimits := app.IntegrationWhoamiLimits
	app.IntegrationWhoamiLimits.Global = ratelimit.Limit{Rate: 2, Per: app.IntegrationWhoamiRateLimitWindow}
	app.IntegrationWhoamiLimits.Tenant = ratelimit.Limit{}
	app.IntegrationWhoamiLimits.Key = ratelimit.Limit{}
	defer func() { app.IntegrationWhoamiLimits = originalLimits }()

	srv, cfg, _ := buildTestServer(t)
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "apikey-auth-flood")

	// Create a real key through the ordinary session-authenticated CRUD
	// surface, the identical shape the rotate/revoke flow above uses.
	createResp := apikeyRequest(t, srv, http.MethodPost, apikeyBasePath, acmeToken, demo.DemoOwnerUserID)
	created := decodeCreatedAPIKey(t, createResp, http.StatusCreated, "create")
	if created.Key == "" {
		t.Fatal("created key carries no raw key")
	}

	// Leg 1: a valid key still authenticates through the pre-auth guard --
	// the gate must not break the legitimate path. The request pays one of
	// the two budget hits.
	who := decodeWhoami(t, whoamiRequest(t, srv, created.Key), http.StatusOK, "whoami with the valid key")
	if who.KeyID != created.ID {
		t.Errorf("whoami key_id = %q, want %q", who.KeyID, created.ID)
	}

	// Leg 2: with one budget hit left, a forged key passes the guard and is
	// refused by authentication itself -- 401 -- spending the last hit.
	forgedOne := whoamiRequest(t, srv, "sk_forged-flood-1")
	forgedOne.Body.Close()
	if forgedOne.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged request before exhaustion: status = %d, want %d", forgedOne.StatusCode, http.StatusUnauthorized)
	}

	// assertGuard429 requires resp to be the guard's own denial: 429 with
	// the module's rate-limited envelope and quota headers -- the exact
	// answer a request refused BEFORE authentication produces, never a 401.
	assertGuard429 := func(resp *http.Response, what string) {
		t.Helper()
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("%s: read body: %v", what, err)
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("%s: status = %d, want %d; body = %s", what, resp.StatusCode, http.StatusTooManyRequests, body)
		}
		var envelope struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Fatalf("%s: decoding %s: %v", what, body, err)
		}
		if envelope.Code != "integration.rate_limited" {
			t.Errorf("%s: code = %q, want %q", what, envelope.Code, "integration.rate_limited")
		}
		if got := resp.Header.Get("X-RateLimit-Layer"); got != "global" {
			t.Errorf("%s: X-RateLimit-Layer = %q, want %q", what, got, "global")
		}
		if resp.Header.Get("Retry-After") == "" {
			t.Errorf("%s: Retry-After is empty on the guard's 429", what)
		}
	}

	// Leg 3: the very next forged request is refused BY THE GUARD -- 429,
	// never 401 -- with the budget exhausted and no Authenticate call run.
	assertGuard429(whoamiRequest(t, srv, "sk_forged-flood-2"), "forged request after exhaustion")

	// Leg 4: even the route's own valid key answers 429 now. The guard
	// gates authentication ATTEMPTS, not credentials, and this refusal --
	// where Authenticate would have answered 200 -- proves the exhausted
	// requests never reached authentication at all.
	assertGuard429(whoamiRequest(t, srv, created.Key), "valid key after exhaustion")
}
