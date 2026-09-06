package main

// apikey_authenticate_flow_test.go is go/integration round 6's own
// mandatory-first-consumer proof: the property go/integration/AGENTS.md's
// round-5 section named as unverifiable until this round shipped an
// Authenticate/Verify path -- "a request bearing a rotated-away or revoked
// key is refused" -- driven for real, through this app's own composed HTTP
// stack, against a real inbound endpoint (cmd/server/
// integration_authenticate.go's integrationWhoamiPath) gated by
// integration.AuthMiddleware. It shares apikey_flow_test.go's own helpers
// (apikeyRequest, decodeCreatedAPIKey) for the session-authenticated CRUD
// half of the flow, and never imports go/integration itself, matching that
// file's own wire-shape discipline.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// testIntegrationWhoami is the wire shape of integrationWhoamiResponse.
type testIntegrationWhoami struct {
	TenantID  string   `json:"tenant_id"`
	KeyID     string   `json:"key_id"`
	CreatedBy string   `json:"created_by"`
	Scopes    []string `json:"scopes"`
}

// whoamiRequest issues a GET against integrationWhoamiPath on srv, presenting
// rawKey as the X-API-Key bearer credential (integration.HeaderAPIKey) --
// never Authorization, which this app's own outer authn.Middleware already
// claims for session bearer tokens (see integration_authenticate.go's own
// HeaderAPIKey doc comment for why the two cannot share a header). An empty
// rawKey sends no header at all.
func whoamiRequest(t *testing.T, srv *httptest.Server, rawKey string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, srv.URL+integrationWhoamiPath, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if rawKey != "" {
		req.Header.Set("X-API-Key", rawKey)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", integrationWhoamiPath, err)
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
// drives the full, previously-impossible property end to end: create a key,
// use it as a bearer credential against integrationWhoamiPath (succeeds),
// rotate it, use the OLD raw key value again (refused), use the NEW key
// (succeeds), revoke it, use it again (refused).
func TestBuildServer_APIKeyAuthenticateFlow_RotateAndRevokeRefuseTheOldKey(t *testing.T) {
	srv, cfg, _ := buildTestServer(t)
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "apikey-authn-flow")

	// Create with an empty body, through the ordinary session-authenticated
	// CRUD surface apikey_flow_test.go's own helpers drive -- the identical
	// shape that test uses, since round 6 changes nothing about how a key is
	// issued.
	createResp := apikeyRequest(t, srv, http.MethodPost, apikeyBasePath, acmeToken, demoOwnerUserID)
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
	rotateResp := apikeyRequest(t, srv, http.MethodPost, apikeyBasePath+"/"+created.ID+"/rotate", acmeToken, demoOwnerUserID)
	rotated := decodeCreatedAPIKey(t, rotateResp, http.StatusOK, "rotate")

	// The OLD raw key value is now refused as a bearer credential -- THE
	// property go/integration/AGENTS.md's round-5 section named as
	// unverifiable until this round.
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
	revokeResp := apikeyRequest(t, srv, http.MethodDelete, apikeyBasePath+"/"+rotated.ID, acmeToken, demoOwnerUserID)
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
