package apptest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/app"
	"github.com/vislake/speed/examples/reference-app/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
)

// RegisterAndAuthenticate registers a fresh demo account through authn's
// real HTTP surface (POST /api/v1/authn/register), grants it membership in
// tenant via cfg.Memberships (the seam BuildServer itself wires authn's
// MembershipReader to -- see internal/app/sign_in_memberships.go's own doc comment:
// org's rows answer customer-tenant questions first, and the grant this
// helper records is the in-process test shortcut that answers when org has
// no row for the pair), signs it in with a tenant_id request naming
// tenant, and returns the resulting bearer access token.
//
// That token is now the ONLY thing that selects a tenant for a protected
// route in this app: with authn.Middleware running ahead of
// tenancy.Middleware(authn.NewPrincipalResolver()), Host plays no part in
// resolving the notes API's tenant at all (see internal/app/server.go's middleware-chain
// doc comment) -- every caller varies the token it authenticates with, never Host, to reach a
// different tenant.
func RegisterAndAuthenticate(t *testing.T, srv *httptest.Server, cfg app.ServerConfig, tenant pkgcore.TenantID, emailLocalPart string) string {
	t.Helper()

	email := emailLocalPart + "@example.com"
	registerBody, err := json.Marshal(map[string]string{"email": email, "password": testutil.TestPassword})
	if err != nil {
		t.Fatalf("marshal register body: %v", err)
	}
	registerResp, err := srv.Client().Post(srv.URL+"/api/v1/authn/register", "application/json", bytes.NewReader(registerBody))
	if err != nil {
		t.Fatalf("register %s: %v", email, err)
	}
	defer func() { _ = registerResp.Body.Close() }()
	if registerResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(registerResp.Body)
		t.Fatalf("register %s status = %d, want %d; body = %s", email, registerResp.StatusCode, http.StatusCreated, body)
	}
	var user struct {
		ID string `json:"id"`
	}
	if decodeErr := json.NewDecoder(registerResp.Body).Decode(&user); decodeErr != nil {
		t.Fatalf("decode register response for %s: %v", email, decodeErr)
	}
	if user.ID == "" {
		t.Fatalf("register %s: response carried no id", email)
	}

	if cfg.Memberships == nil {
		t.Fatal("RegisterAndAuthenticate: cfg.Memberships is nil -- ServerConfig always sets it, was a different app.ServerConfig passed?")
	}
	cfg.Memberships.Grant(user.ID, tenant)

	loginBody, err := json.Marshal(map[string]string{
		"identifier": email,
		"password":   testutil.TestPassword,
		"tenant_id":  string(tenant),
	})
	if err != nil {
		t.Fatalf("marshal login body: %v", err)
	}
	loginResp, err := srv.Client().Post(srv.URL+"/api/v1/authn/login/password", "application/json", bytes.NewReader(loginBody))
	if err != nil {
		t.Fatalf("login %s: %v", email, err)
	}
	defer func() { _ = loginResp.Body.Close() }()
	if loginResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(loginResp.Body)
		t.Fatalf("login %s status = %d, want %d; body = %s", email, loginResp.StatusCode, http.StatusOK, body)
	}
	var pair struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(loginResp.Body).Decode(&pair); err != nil {
		t.Fatalf("decode login response for %s: %v", email, err)
	}
	if pair.AccessToken == "" {
		t.Fatalf("login %s: response carried no access_token", email)
	}
	return pair.AccessToken
}
