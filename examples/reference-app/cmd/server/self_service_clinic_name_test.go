package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// registerFreshAccountNamed registers email with an explicit
// display_name -- the name a practice types into the register form --
// through authn's real register route, returning the user id authn
// assigned.
func registerFreshAccountNamed(t *testing.T, srv *httptest.Server, email, password, displayName string) (userID string) {
	t.Helper()

	body, err := json.Marshal(map[string]string{
		"email":        email,
		"password":     password,
		"display_name": displayName,
	})
	if err != nil {
		t.Fatalf("register %s: marshal body: %v", email, err)
	}
	resp, err := srv.Client().Post(srv.URL+"/api/v1/authn/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("register %s: %v", email, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("register %s: status = %d, want %d; body = %s", email, resp.StatusCode, http.StatusCreated, raw)
	}
	var user struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		t.Fatalf("decode register response for %s: %v", email, err)
	}
	if user.ID == "" {
		t.Fatalf("register %s: response carried no id", email)
	}
	return user.ID
}

// clinicNameAs GETs the app's own tenant-identity answer
// (/api/reference-app/clinic-name, cmd/server/clinic_name.go) with
// token's bearer and decodes the {name} answer, failing the test on
// anything but a 200 carrying the field.
func clinicNameAs(t *testing.T, srv *httptest.Server, token string) (name string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, srv.URL+clinicNamePath, nil)
	if err != nil {
		t.Fatalf("build clinic-name request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", clinicNamePath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: status = %d, want %d; body = %s", clinicNamePath, resp.StatusCode, http.StatusOK, raw)
	}
	var answer struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		t.Fatalf("decode clinic-name response: %v", err)
	}
	return answer.Name
}

// TestSelfServiceSignup_ClinicIsIdentifiedByTheNameItsRegistrationGave
// pins the product decision for what a self-registered clinic is
// called -- the name the practice typed into the register form's
// display-name field names the clinic's org root (self_service.go's
// clinicRootNameFor), and the app's own tenant-identity answer serves
// that root name back to the clinic's own signed-in principal. This is
// the server half of the acceptance gate
// (web/e2e/current-clinic-is-visible.spec.ts): the recognisable name
// the browser renders comes from this route, and a raw tenant id is
// never what it answers.
func TestSelfServiceSignup_ClinicIsIdentifiedByTheNameItsRegistrationGave(t *testing.T) {
	srv, _, _ := buildTestServer(t)

	const clinicName = "Northside Dental"
	const email = "named-clinic-founder@example.com"
	userID := registerFreshAccountNamed(t, srv, email, selfServicePassword, clinicName)
	wantTenant := pkgcore.TenantID("tenant-" + userID)

	// The browser-shaped sign-in lands in the clinic, and the clinic's
	// own bearer token answers the tenant-identity route with the name
	// the registration gave -- the recognisable identity the frame and
	// the work area render, never the derived tenant id.
	status, code, token, tenant := browserSignIn(t, srv, email, selfServicePassword)
	if status != http.StatusOK {
		t.Fatalf("browser-shaped sign-in: status = %d, code = %q, want %d", status, code, http.StatusOK)
	}
	if tenant != wantTenant {
		t.Fatalf("sign-in landed the principal in tenant %q, want its own clinic %q", tenant, wantTenant)
	}
	if name := clinicNameAs(t, srv, token); name != clinicName {
		t.Fatalf("clinic-name answer = %q, want %q -- the name the registration gave the clinic, never its tenant id %q",
			name, clinicName, tenant)
	}

	// The clinic's org root itself carries that name: org's row for
	// "what is this clinic called" is the route's source, read back
	// here through org's own HTTP surface (a tenant-scoped read under
	// the clinic owner's bearer token), so a future rename of the node
	// is what renames the clinic.
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/org/nodes", nil)
	if err != nil {
		t.Fatalf("build org nodes request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/org/nodes: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /api/v1/org/nodes: status = %d, want %d; body = %s", resp.StatusCode, http.StatusOK, raw)
	}
	var tree struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tree); err != nil {
		t.Fatalf("decode org nodes response: %v", err)
	}
	if len(tree.Nodes) != 1 || tree.Nodes[0].Name != clinicName {
		t.Fatalf("clinic org tree names = %+v, want the single root named %q", tree.Nodes, clinicName)
	}
}
