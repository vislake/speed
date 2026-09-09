package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/app"
	"github.com/vislake/speed/go/compliance"
	"github.com/vislake/speed/go/pkgcore"
)

// test_support_test.go mirrors the server-boot, config and domain-request
// helpers the command's staying suites (demo_users_test.go,
// demo_admin_test.go, self_service_test.go and
// demo_user_header_kill_switch_test.go) still drive their journeys with:
// the originals moved to the flowtests package with the flow suites that
// use them (flowtests/server_test.go, cases_flow_test.go,
// notification_flow_test.go and fragment_wire_paths_test.go), and Go test
// helpers cannot be imported across packages, so the command's package
// carries this mirror. Keep it in step with the flowtests originals.

const testPassword = "a perfectly fine passphrase"

// testConfig returns a ServerConfig backed by a fresh, per-test temp-file
// SQLite database, so tests never share state and never touch a real file
// outside t.TempDir(). Memberships is always a fresh, empty
// signInMemberships -- tests that need an account to actually reach a
// tenant grant it explicitly via registerAndAuthenticate below, keeping
// the same reference BuildServer itself wires (and attaches to org) so a
// test's grant is visible to the running server.
//
// NotificationIndexKey is set because every boot wires the notification
// module's two contact indexers from the struct field directly -- the
// APP_NOTIFICATION_INDEX_KEY environment default only exists on the
// ConfigFromEnv path, which this helper never takes -- and an empty key
// fails the boot before the first request. PKILocalKeyCipherKey,
// AuthnBlindIndexKey and AuthnPIICipherKey are set for the identical
// reason: BuildServer reads all three straight off cfg (APP_ROOT_KEY's
// derivation and each key's own individual env var both live in
// ConfigFromEnv, which this helper bypasses entirely), so an empty value
// here fails the boot the same way an empty ConfigKey would.
func testConfig(t *testing.T) app.ServerConfig {
	t.Helper()
	return app.ServerConfig{
		DeploymentMode:       pkgcore.DeploymentModeStandalone,
		Port:                 "0",
		SQLitePath:           filepath.Join(t.TempDir(), "reference-app-test.db"),
		ConfigKey:            app.DevConfigKey,
		OrgIndexKey:          app.DevOrgIndexKey,
		NotificationIndexKey: app.DevNotificationIndexKey,
		PKILocalKeyCipherKey: app.DevPKILocalKeyCipherKey,
		AuthnBlindIndexKey:   app.DevBlindIndexKey,
		AuthnPIICipherKey:    app.DevPIICipherKey,
		HostTenants:          app.DemoHostTenants,
		Memberships:          app.NewSignInMemberships(),
	}
}

// buildTestServer wires up BuildServer's real output behind an
// httptest.Server, so tests exercise the exact composed handler main.go
// itself serves -- the authn+tenancy middleware chain, the notes Module's
// real handler, and a real (if temp-file) SQLite database -- not a mock of
// any of them. It returns the ServerConfig alongside the server so a
// caller can reach cfg.Memberships to grant a demo account tenant
// membership after registering it (registerAndAuthenticate does this), and
// BuildServer's wired *compliance.Module -- the one reach a test has into
// the retention/erasure/export services, which flowtests' compliance_flow_test.go
// drives (every other flow test in the flowtests package is HTTP-driven and
// discards it, exactly as BuildServer's own doc comment describes main.go doing).
func buildTestServer(t *testing.T) (*httptest.Server, app.ServerConfig, *compliance.Module) {
	t.Helper()

	cfg := testConfig(t)
	handler, cleanup, complianceModule, err := app.BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, cfg, complianceModule
}

// registerAndAuthenticate registers a fresh demo account through authn's
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
// doc comment) -- every test in this file varies the token it authenticates with, never Host, to reach a
// different tenant.
func registerAndAuthenticate(t *testing.T, srv *httptest.Server, cfg app.ServerConfig, tenant pkgcore.TenantID, emailLocalPart string) string {
	t.Helper()

	email := emailLocalPart + "@example.com"
	registerBody, err := json.Marshal(map[string]string{"email": email, "password": testPassword})
	if err != nil {
		t.Fatalf("marshal register body: %v", err)
	}
	registerResp, err := srv.Client().Post(srv.URL+"/api/v1/authn/register", "application/json", bytes.NewReader(registerBody))
	if err != nil {
		t.Fatalf("register %s: %v", email, err)
	}
	defer registerResp.Body.Close()
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
		t.Fatal("registerAndAuthenticate: cfg.Memberships is nil -- testConfig always sets it, was a different app.ServerConfig passed?")
	}
	cfg.Memberships.Grant(user.ID, tenant)

	loginBody, err := json.Marshal(map[string]string{
		"identifier": email,
		"password":   testPassword,
		"tenant_id":  string(tenant),
	})
	if err != nil {
		t.Fatalf("marshal login body: %v", err)
	}
	loginResp, err := srv.Client().Post(srv.URL+"/api/v1/authn/login/password", "application/json", bytes.NewReader(loginBody))
	if err != nil {
		t.Fatalf("login %s: %v", email, err)
	}
	defer loginResp.Body.Close()
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

// notesRequestAs issues method against /api/v1/notes with the given bearer
// token (empty means no Authorization header at all) and acting user,
// returning the raw response for the caller to assert on. An empty user
// sends no demo user header at all, which is how a request with a
// resolvable tenant (the token) but no identity is expressed.
//
// A non-empty user additionally sends X-Demo-User-Id (DemoNotesCreatorUserID):
// notes' create handler attributes the note through its own SubjectResolver
// (DemoNotesSubjectResolver in internal/app/server.go), which reads that header first and
// falls back to the verified Principal when no demo header is present -- so
// the requests that carry a demo user carry the creator header the demo
// flows were built around, while a token-only request (demo_users_test.go,
// the seeded accounts acting as real users) is attributed through the
// Principal instead.
func notesRequestAs(t *testing.T, srv *httptest.Server, method, token, user string, body io.Reader) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, srv.URL+"/api/v1/notes", body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if user != "" {
		req.Header.Set(app.DemoUserHeader, user)
		req.Header.Set(app.DemoOrgUserHeader, app.DemoNotesCreatorUserID)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s /api/v1/notes (user=%q): %v", method, user, err)
	}
	return resp
}

// assertPermissionDenied reads resp and requires it to be rbac's 403 with
// the structured code the client resolves against the module's locale
// files -- not merely "some 4xx", which tenancy's own fail-closed 403 would
// also satisfy.
func assertPermissionDenied(t *testing.T, resp *http.Response, what string) {
	t.Helper()
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: read body: %v", what, err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("%s: status = %d, want %d; body = %s", what, resp.StatusCode, http.StatusForbidden, body)
	}
	var decoded struct {
		Code string `json:"code"`
	}
	if err = json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("%s: decoding %s: %v", what, body, err)
	}
	if decoded.Code != "rbac.permission_denied" {
		t.Fatalf("%s: error code = %q, want %q; body = %s", what, decoded.Code, "rbac.permission_denied", body)
	}
}

type testNote struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type testListNotesResponse struct {
	Notes []testNote `json:"notes"`
}

// This file drives the case domain routes
// (internal/app/cases.go) through the real composed HTTP stack
// buildTestServer builds -- the authn+tenancy middleware chain, the real
// registration/sign-in surface, and a real temp-file SQLite database whose
// cases tables the boot's own EnsureSchema created -- not a mock of any of
// it. The tenant comes from the bearer token alone (registerAndAuthenticate's
// own doc comment explains why Host plays no part); where a request needs
// a creator (the create route only -- the clinic-wide list reads no
// creator), the attribution comes from the X-Demo-User-Id header
// (DemoNotesSubjectResolver, the same attribution seam notes' create
// handler uses), or from the verified Principal when no header rides
// along.

// caseCreateBody is the wire body this file's helpers send to POST
// /api/v1/cases.
type caseCreateBody struct {
	PatientName    string   `json:"patient_name"`
	PatientRef     string   `json:"patient_ref"`
	PhotoObjectIDs []string `json:"photo_object_ids"`
}

// testCase is the response shape a case's create/list/detail answers
// share (the spec fragment's CasesCase, encoded by toCasesCase in
// internal/app/cases.go), decoded enough for this file's assertions.
type testCase struct {
	ID            string `json:"id"`
	PatientName   string `json:"patient_name"`
	PatientRef    string `json:"patient_ref"`
	CreatorUserID string `json:"creator_user_id"`
	Photos        []struct {
		ObjectID string `json:"object_id"`
	} `json:"photos"`
}

// casesRequestAs issues method against path (a full path under the case
// surface, e.g. /api/v1/cases) with the given bearer token (empty means no
// Authorization header at all) and creator (empty means no X-Demo-User-Id
// header, which is how a request attributed through the verified Principal
// is expressed), returning the raw response.
func casesRequestAs(t *testing.T, srv *httptest.Server, method, path, token, creator string, body io.Reader) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, srv.URL+path, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if creator != "" {
		req.Header.Set(app.DemoOrgUserHeader, creator)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s (creator=%q): %v", method, path, creator, err)
	}
	return resp
}

// createCaseAs POSTs a case body to srv authenticated as token and
// attributed to creator, asserting the 201 and returning the decoded case
// (photos included). t.Fatal on any deviation -- this is the happy-path
// helper every journey leg builds on.
func createCaseAs(t *testing.T, srv *httptest.Server, token, creator string, body caseCreateBody) testCase {
	t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal case body: %v", err)
	}
	resp := casesRequestAs(t, srv, http.MethodPost, casesPath, token, creator, bytes.NewReader(raw))
	defer resp.Body.Close()

	decoded, respBody := decodeCasesResponse(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST %s (creator=%q) status = %d, want %d; body = %s",
			casesPath, creator, resp.StatusCode, http.StatusCreated, respBody)
	}
	return decoded
}

// decodeCasesResponse decodes resp's JSON body into a testCase and also
// returns the raw body bytes for failure messages.
func decodeCasesResponse(t *testing.T, resp *http.Response) (testCase, string) {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	var decoded testCase
	if len(body) > 0 {
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("decoding %s: %v", body, err)
		}
	}
	return decoded, string(body)
}

// notifRequest issues method against srv.URL+path with a bearer token, the
// acting subject (the X-Demo-User-Id header DemoOrgSubjectResolver reads;
// empty omits it) and an optional JSON body, and requires the response to
// carry wantStatus, decoding it into out (nil to skip decoding, for empty
// responses like the 204s and the demo route's 202). The envelope of every
// refusal decodes into a notifErrorBody through the same out slot.
func notifRequest(t *testing.T, srv *httptest.Server, method, path, token, subjectUserID string, body any, wantStatus int, out any) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if subjectUserID != "" {
		req.Header.Set(app.DemoOrgUserHeader, subjectUserID)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != wantStatus {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s status = %d, want %d; body = %s", method, path, resp.StatusCode, wantStatus, respBody)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode %s %s response: %v", method, path, err)
		}
	}
}

// notifError issues a request whose response must carry the given error
// status, and returns the decoded envelope so the caller can pin the code.
func notifError(t *testing.T, srv *httptest.Server, method, path, token, subjectUserID string, body any, wantStatus int) notifErrorBody {
	t.Helper()
	var env notifErrorBody
	notifRequest(t, srv, method, path, token, subjectUserID, body, wantStatus, &env)
	if env.Code == nil {
		t.Fatalf("%s %s: error response carried no code", method, path)
	}
	return env
}

type notifErrorBody struct {
	Code *string `json:"code"`
}

// casesPath is the cases fragment list-and-create route, mirrored from the
// flowtests fixture so the staying suites can name the wire path.
const casesPath = "/api/v1/cases"
