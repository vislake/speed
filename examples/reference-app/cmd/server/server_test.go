package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
)

// testPassword is the demo password every account registerAndAuthenticate
// creates uses. It exists as a named constant, not a repeated literal,
// purely so every test in this file that registers a demo account agrees on
// what "a perfectly fine passphrase" means -- go/authn's own password
// policy default (go/authn/password.go) accepts it.
const testPassword = "a perfectly fine passphrase"

// testConfig returns a serverConfig backed by a fresh, per-test temp-file
// SQLite database, so tests never share state and never touch a real file
// outside t.TempDir(). Memberships is always a fresh, empty demoMemberships
// -- tests that need a demo account to actually reach a tenant grant it
// explicitly via registerAndAuthenticate below, keeping the same reference
// buildServer itself wires so a test's grant is visible to the running
// server.
func testConfig(t *testing.T) serverConfig {
	t.Helper()
	return serverConfig{
		DeploymentMode: pkgcore.DeploymentModeStandalone,
		Port:           "0",
		SQLitePath:     filepath.Join(t.TempDir(), "reference-app-test.db"),
		ConfigKey:      devConfigKey,
		OrgIndexKey:    devOrgIndexKey,
		HostTenants:    demoHostTenants,
		Memberships:    newDemoMemberships(),
	}
}

// buildTestServer wires up buildServer's real output behind an
// httptest.Server, so tests exercise the exact composed handler main.go
// itself serves -- the authn+tenancy middleware chain, the notes Module's
// real handler, and a real (if temp-file) SQLite database -- not a mock of
// any of them. It returns the serverConfig alongside the server so a
// caller can reach cfg.Memberships to grant a demo account tenant
// membership after registering it (registerAndAuthenticate does this).
func buildTestServer(t *testing.T) (*httptest.Server, serverConfig) {
	t.Helper()

	cfg := testConfig(t)
	handler, cleanup, err := buildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, cfg
}

// registerAndAuthenticate registers a fresh demo account through authn's
// real HTTP surface (POST /api/v1/authn/register), grants it membership in
// tenant via cfg.Memberships (the seam buildServer itself wires authn's
// MembershipReader to -- see demoMemberships' own doc comment in
// server.go), signs it in with a tenant_id request naming tenant, and
// returns the resulting bearer access token.
//
// That token is now the ONLY thing that selects a tenant for a protected
// route in this app: with authn.Middleware running ahead of
// tenancy.Middleware(authn.NewPrincipalResolver()), Host plays no part in
// resolving the notes API's tenant at all (see server.go's middleware-chain
// doc comment) -- every test in this file that used to vary Host to reach a
// different tenant now varies the token it authenticates with instead.
func registerAndAuthenticate(t *testing.T, srv *httptest.Server, cfg serverConfig, tenant pkgcore.TenantID, emailLocalPart string) string {
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
		t.Fatal("registerAndAuthenticate: cfg.Memberships is nil -- testConfig always sets it, was a different serverConfig passed?")
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

type testNote struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type testListNotesResponse struct {
	Notes []testNote `json:"notes"`
}

// createNoteAs POSTs a note with the given text to srv, authenticated as
// token -- the bearer access token is the ONLY thing that selects the
// tenant a note is created under (registerAndAuthenticate's own doc
// comment explains why Host does not).
//
// The demo user header is a different thing entirely and must not be
// confused with the token: it names WHO is acting, which the rbac gate
// needs (see demo_subject.go's demoUserHeader). demoOwnerUserID holds
// every permission, so these two helpers exercise the happy path; the
// tests that exercise the gate itself send other users, or none.
func createNoteAs(t *testing.T, srv *httptest.Server, token, text string) {
	t.Helper()

	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/notes", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(demoUserHeader, demoOwnerUserID)

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/notes: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /api/v1/notes status = %d, want %d; body = %s",
			resp.StatusCode, http.StatusCreated, respBody)
	}
}

// listNotesAs GETs the notes visible to the tenant token authenticates for.
func listNotesAs(t *testing.T, srv *httptest.Server, token string) []testNote {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/notes", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(demoUserHeader, demoOwnerUserID)

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/notes: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /api/v1/notes status = %d, want %d; body = %s",
			resp.StatusCode, http.StatusOK, respBody)
	}

	var out testListNotesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out.Notes
}

// TestBuildServer_MultiTenantIsolation_EndToEnd is the genuine, automated,
// executable proof the root task asked for: two different demo accounts,
// each granted membership in a different tenant and each signed in for it,
// creating notes under each and asserting each tenant's list contains only
// its own notes -- through the real middleware + handler +
// dbkit.Repository[Note] stack this app actually serves, not a mocked
// shortcut at any layer. The tenant comes from each account's own access
// token, never from a request header the caller controls -- see
// registerAndAuthenticate's own doc comment.
//
// Scope boundary: this only exercises Create and List, the only two
// operations notes' HTTP API exposes. List's isolation comes from the SQL
// WHERE clause the GORM tenant-scope plugin injects (keyed off the
// tenant_id column directly), never from Note.GetTenantID() / FindByID --
// so a regression there (for example, a redeclared TenantID field on Note
// that shadows the one dbkit.TenantModel promotes; see model.go's own doc
// comment on why that must never happen) would NOT be caught by this
// test, because notes exposes no get-by-id endpoint for it to reach
// through HTTP. That gap is covered instead by
// internal/notes/repository_test.go's TestRepository_AssertIsolated,
// which drives dbkit.Repository[Note] directly and does exercise
// FindByID/Update/Delete's isolation guarantees. The two tests are
// complementary, not redundant: neither can substitute for the other.
func TestBuildServer_MultiTenantIsolation_EndToEnd(t *testing.T) {
	srv, cfg := buildTestServer(t)

	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "acme-isolation")
	globexToken := registerAndAuthenticate(t, srv, cfg, "tenant-globex", "globex-isolation")

	createNoteAs(t, srv, acmeToken, "acme secret 1")
	createNoteAs(t, srv, acmeToken, "acme secret 2")
	createNoteAs(t, srv, globexToken, "globex secret 1")

	acmeNotes := listNotesAs(t, srv, acmeToken)
	if len(acmeNotes) != 2 {
		t.Fatalf("tenant-acme sees %d notes, want 2 (%+v)", len(acmeNotes), acmeNotes)
	}
	for _, n := range acmeNotes {
		if strings.Contains(n.Text, "globex") {
			t.Fatalf("tenant-acme's list leaked a globex note: %+v", n)
		}
	}

	globexNotes := listNotesAs(t, srv, globexToken)
	if len(globexNotes) != 1 {
		t.Fatalf("tenant-globex sees %d notes, want 1 (%+v)", len(globexNotes), globexNotes)
	}
	if globexNotes[0].Text != "globex secret 1" {
		t.Fatalf("tenant-globex note text = %q, want %q", globexNotes[0].Text, "globex secret 1")
	}
	for _, n := range globexNotes {
		if strings.Contains(n.Text, "acme") {
			t.Fatalf("tenant-globex's list leaked an acme note: %+v", n)
		}
	}
}

// notesRequestAs issues method against /api/v1/notes with the given bearer
// token (empty means no Authorization header at all) and acting user,
// returning the raw response for the caller to assert on. An empty user
// sends no demo user header at all, which is how a request with a
// resolvable tenant (the token) but no identity is expressed.
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
		req.Header.Set(demoUserHeader, user)
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

// TestBuildServer_PermissionGate_EnforcesTheNotesPermissions is the
// reference app doing its job as rbac's mandatory first consumer: a real
// route, really gated, with the decision made by the real Service over a
// real database.
//
// The three users are chosen to separate three different reasons a request
// may be refused, which a single "denied" case would conflate:
//
//   - demo-owner holds every declared permission and passes both methods.
//   - demo-reader holds notes:read and nothing else, so it lists notes and
//     is refused when it tries to create one. This is the case that proves
//     the gate closes on a REAL, correctly identified user -- not merely on
//     an anonymous one.
//   - an unknown user is authenticated as far as this demo goes and holds
//     no grant at all, so it is refused both ways.
func TestBuildServer_PermissionGate_EnforcesTheNotesPermissions(t *testing.T) {
	srv, cfg := buildTestServer(t)
	// The token signs a real account into tenant-acme; the demo user header
	// then names which seeded demo grant the gate decides the request
	// against (demo_subject.go's seedDemoGrants).
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "pg-owner")

	// The owner may write.
	resp := notesRequestAs(t, srv, http.MethodPost, acmeToken, demoOwnerUserID,
		strings.NewReader(`{"text":"owner note"}`))
	func() {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("POST as %s: status = %d, want %d; body = %s", demoOwnerUserID, resp.StatusCode, http.StatusCreated, body)
		}
	}()

	// The reader may list...
	resp = notesRequestAs(t, srv, http.MethodGet, acmeToken, demoReaderUserID, nil)
	func() {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("GET as %s: status = %d, want %d; body = %s", demoReaderUserID, resp.StatusCode, http.StatusOK, body)
		}
	}()

	// ...and may not create. This is the whole point of the gate.
	assertPermissionDenied(t,
		notesRequestAs(t, srv, http.MethodPost, acmeToken, demoReaderUserID, strings.NewReader(`{"text":"reader note"}`)),
		"POST as the read-only demo user")

	// A user with no grant at all is refused in both directions.
	assertPermissionDenied(t,
		notesRequestAs(t, srv, http.MethodGet, acmeToken, "nobody", nil),
		"GET as an ungranted user")
	assertPermissionDenied(t,
		notesRequestAs(t, srv, http.MethodPost, acmeToken, "nobody", strings.NewReader(`{"text":"nope"}`)),
		"POST as an ungranted user")
}

// TestBuildServer_PermissionGate_NoSubject_IsRefused covers the request
// that carries a resolvable tenant and no identity at all. It must be
// refused by rbac -- not served, and not confused with tenancy's own
// fail-closed 403, which is why the assertion is on rbac's code rather
// than on the status alone.
func TestBuildServer_PermissionGate_NoSubject_IsRefused(t *testing.T) {
	srv, cfg := buildTestServer(t)
	// The token resolves the tenant; no demo user header means no Subject
	// for the rbac gate to decide for.
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "pg-nosubject")

	assertPermissionDenied(t,
		notesRequestAs(t, srv, http.MethodGet, acmeToken, "", nil),
		"GET with no demo user header")
	assertPermissionDenied(t,
		notesRequestAs(t, srv, http.MethodPost, acmeToken, "", strings.NewReader(`{"text":"anon"}`)),
		"POST with no demo user header")
}

// TestBuildServer_PermissionGate_GrantsDoNotCrossTenants is the isolation
// property at the AUTHORIZATION layer, which is a different layer from the
// data isolation TestBuildServer_MultiTenantIsolation_EndToEnd proves.
//
// Both demo tenants seed most of the same user ids, so a test using one of
// those would pass even against an engine keyed on the user alone. The
// sharp case is demoSingleTenantUserID, which is granted in tenant-acme
// and nowhere else: the identical user id, acting through a token that
// signs into tenant-globex, must be refused. The tenant the decision is
// made in comes from the bearer token, never from anything the caller
// sent -- the header only names WHO is acting.
func TestBuildServer_PermissionGate_GrantsDoNotCrossTenants(t *testing.T) {
	srv, cfg := buildTestServer(t)
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "pg-acme")
	globexToken := registerAndAuthenticate(t, srv, cfg, "tenant-globex", "pg-globex")

	resp := notesRequestAs(t, srv, http.MethodGet, acmeToken, demoSingleTenantUserID, nil)
	func() {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("GET in the tenant that granted %s: status = %d, want %d; body = %s",
				demoSingleTenantUserID, resp.StatusCode, http.StatusOK, body)
		}
	}()

	assertPermissionDenied(t,
		notesRequestAs(t, srv, http.MethodGet, globexToken, demoSingleTenantUserID, nil),
		"GET as the same user id in the tenant that never granted it")

	// And the refusal is genuinely about the tenant rather than the user
	// being unknown: the SAME tenant grants the same role to demo-reader.
	resp = notesRequestAs(t, srv, http.MethodGet, globexToken, demoReaderUserID, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET as %s in tenant-globex: status = %d, want %d; body = %s",
			demoReaderUserID, resp.StatusCode, http.StatusOK, body)
	}
}

// TestBuildServer_PublicConfigEndpoints_StayUngated guards the routePublic
// half of demoRouteGuards through the composed server: config's two
// pre-auth endpoints must keep answering with no identity whatsoever, or a
// login page could never render its own brand.
func TestBuildServer_PublicConfigEndpoints_StayUngated(t *testing.T) {
	srv, _ := buildTestServer(t)

	for _, path := range []string{config.PathPublic, config.PathSystemFeatures} {
		t.Run(path, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			// No demo user header, and a Host that resolves to no tenant.
			req.Host = "totally-unrecognized-host.example"

			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("GET %s with no identity: status = %d, want %d; body = %s",
					path, resp.StatusCode, http.StatusOK, body)
			}
		})
	}
}

// TestBuildServer_Healthz_NoTenantRequired proves /healthz responds 200
// through the real composed server regardless of Host -- Host plays no
// part in resolving anything on this route (it is allowlisted outright),
// so a liveness probe never depends on tenant-specific resolution
// succeeding, an authenticated caller, or any particular Host at all.
func TestBuildServer_Healthz_NoTenantRequired(t *testing.T) {
	srv, _ := buildTestServer(t)

	for _, host := range []string{"acme.demo.localhost", "totally-unrecognized-host.example"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			req, err := http.NewRequest(method, srv.URL+healthzPath, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Host = host

			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("%s %s (Host=%q): %v", method, healthzPath, host, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s %s (Host=%q) status = %d, want 200", method, healthzPath, host, resp.StatusCode)
			}
		}
	}
}

// failingResolver deliberately fails every resolution, standing in for any
// Resolver's failure mode in general -- an invalid or missing bearer token
// under authn.NewPrincipalResolver today (server.go's middleware-chain doc
// comment). Using a resolver that always fails, rather than driving
// buildServer's real composed chain with a missing/invalid token, keeps
// this test about the allowlist mechanism in isolation.
type failingResolver struct{}

func (failingResolver) Resolve(r *http.Request) (pkgcore.TenantID, error) {
	return "", errors.New("test: deliberately failing resolver")
}

func TestHealthzAllowlist_ResolutionFailure_StillReturns200(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(http.MethodGet+" "+healthzPath, healthzHandler)

	// The same construction buildServer uses: tenancy.Middleware wrapping
	// the mux, allowlisting both GET and HEAD for healthzPath -- see
	// server.go's own comment on why HEAD needs its own entry too.
	handler := tenancy.Middleware(failingResolver{},
		tenancy.WithAllowlist(http.MethodGet, healthzPath),
		tenancy.WithAllowlist(http.MethodHead, healthzPath),
	)(mux)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req := httptest.NewRequest(method, healthzPath, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s with a failing resolver: status = %d, want 200 (body: %s)", method, healthzPath, rec.Code, rec.Body.String())
		}
	}

	// The body is checked separately from the status-code loop above:
	// net/http's own HEAD handling correctly omits the response body (per
	// RFC 9110) even though the handler wrote one, so asserting on it only
	// for the GET request keeps this test honest about what HEAD actually
	// guarantees.
	req := httptest.NewRequest(http.MethodGet, healthzPath, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Body.String() != "ok" {
		t.Fatalf("GET %s body = %q, want %q", healthzPath, rec.Body.String(), "ok")
	}

	// Sanity check, proving the allowlist -- not general leniency in
	// healthzHandler or the mux -- is what let the requests above through:
	// the identical failing resolver still fails closed (403) for a path
	// that was never allowlisted.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("GET /api/v1/notes with a failing resolver and no allowlist entry: status = %d, want 403 (body: %s)",
			rec2.Code, rec2.Body.String())
	}
}

// TestHealthzAllowlist_GETOnlyAllowlist_LeavesHEADExposed is a regression
// test for a gap this app's own wiring had until it was caught by
// literally curling the running server during manual verification: a
// plain `curl -X POST /healthz` (unrelated) revealed net/http's ServeMux
// automatically serves HEAD healthzPath from a registered "GET
// "+healthzPath pattern (Go's long-standing GET-implies-HEAD convenience),
// but tenancy.Middleware does NOT extend WithAllowlist's (method, path)
// exemption the same way -- its own doc comment says so explicitly:
// "allowlist http.MethodHead explicitly if a health check needs it too."
// Allowlisting GET alone therefore looks fine under a resolver that never
// fails, while silently leaving HEAD one resolver failure away from a 403
// -- exactly the swap this comment originally warned about, and exactly
// what authn.NewPrincipalResolver genuinely does fail with today whenever
// no Principal is present (server.go's middleware-chain doc comment).
//
// This test reproduces exactly that gap (deliberately allowlisting GET
// only, unlike buildServer's real wiring) as a permanent canary: if it
// ever starts failing -- HEAD suddenly returning 200 -- either net/http's
// or tenancy.Middleware's GET/HEAD behavior changed, and server.go's
// buildServer may no longer need its explicit HEAD allowlist entry.
func TestHealthzAllowlist_GETOnlyAllowlist_LeavesHEADExposed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(http.MethodGet+" "+healthzPath, healthzHandler)

	handler := tenancy.Middleware(failingResolver{}, tenancy.WithAllowlist(http.MethodGet, healthzPath))(mux)

	req := httptest.NewRequest(http.MethodHead, healthzPath, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("HEAD %s with only GET allowlisted and a failing resolver: status = %d, want 403", healthzPath, rec.Code)
	}
}

// TestBuildServer_Metrics_NoTenantRequired proves /metrics responds through
// the real composed server regardless of Host, mirroring
// TestBuildServer_Healthz_NoTenantRequired above for the other route
// buildServer allowlists (see server.go's metricsPath doc comment: a
// scraper, like a liveness probe, has no demo Host to send and must not
// depend on one).
//
// Unlike the healthz version, this does not assert on a literal 200:
// metricsHandler (server.go) serves whatever obs.MetricsHandler() currently
// returns, and -- like every other test that drives buildServer directly in
// this file -- this test never calls obs.Init, so metricsHandler answers
// its documented "before Init has run" 404 here, not a real scrape (see
// MetricsHandler's own doc comment in go/observability/init.go). The
// property this test level can honestly verify is narrower, but is the one
// actually in question here: tenancy.Middleware's allowlist let the
// request through to metricsHandler at all, for every Host, instead of
// rejecting it with 403 -- ErrTenantUnresolved is the ONLY status
// Middleware itself ever produces (go/tenancy/middleware.go), so "not 403"
// is a precise proof of "no tenant required" at this level.
// TestMetricsAllowlist_ResolutionFailure_StillReturns200 below additionally
// proves the stronger "really answers 200" property the manual
// verification that found this gap relied on, in isolation from whatever
// obs.Init state this process happens to be in, by calling obs.Init itself.
func TestBuildServer_Metrics_NoTenantRequired(t *testing.T) {
	srv, _ := buildTestServer(t)

	for _, host := range []string{"acme.demo.localhost", "totally-unrecognized-host.example"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			req, err := http.NewRequest(method, srv.URL+metricsPath, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Host = host

			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("%s %s (Host=%q): %v", method, metricsPath, host, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusForbidden {
				t.Fatalf("%s %s (Host=%q) status = %d, want anything but 403 (tenant resolution must not be required for this route)",
					method, metricsPath, host, resp.StatusCode)
			}
		}
	}
}

// TestMetricsAllowlist_ResolutionFailure_StillReturns200 is metricsPath's
// counterpart to TestHealthzAllowlist_ResolutionFailure_StillReturns200
// above, proving the same property tenancy.WithAllowlist gives /healthz --
// the route stays reachable even when the Resolver fails outright -- for
// the other route buildServer allowlists.
//
// Unlike TestBuildServer_Metrics_NoTenantRequired above, this test calls
// obs.Init() itself first -- no deployment mode argument; Init's
// no-endpoint path wires the local exporters, which is exactly the
// wiring main.go's run() arranges before serving any production traffic
// -- so metricsHandler answers with a real Prometheus scrape (200)
// here, reproducing, as a permanent automated test, exactly what manual
// verification of this gap found: with Init having actually run, both
// GET and HEAD /metrics return 200 regardless of Host/resolution
// outcome.
// Init's returned shutdown is registered via t.Cleanup so the
// package-level handler obs.MetricsHandler() returns is restored to its
// unavailable-by-default state before any other test in this binary runs
// -- the same discipline go/observability's own tests use to keep
// repeated Init calls independent.
func TestMetricsAllowlist_ResolutionFailure_StillReturns200(t *testing.T) {
	shutdown, err := obs.Init(context.Background())
	if err != nil {
		t.Fatalf("obs.Init: %v", err)
	}
	t.Cleanup(func() {
		if shutdownErr := shutdown(context.Background()); shutdownErr != nil {
			t.Errorf("obs.Init shutdown: %v", shutdownErr)
		}
	})

	mux := http.NewServeMux()
	mux.HandleFunc(http.MethodGet+" "+metricsPath, metricsHandler)

	// The same construction buildServer uses: tenancy.Middleware wrapping
	// the mux, allowlisting both GET and HEAD for metricsPath -- see
	// server.go's own comment on why HEAD needs its own entry too.
	handler := tenancy.Middleware(failingResolver{},
		tenancy.WithAllowlist(http.MethodGet, metricsPath),
		tenancy.WithAllowlist(http.MethodHead, metricsPath),
	)(mux)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req := httptest.NewRequest(method, metricsPath, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s with a failing resolver: status = %d, want 200 (body: %s)", method, metricsPath, rec.Code, rec.Body.String())
		}
	}

	// Sanity check, proving the allowlist -- not general leniency in
	// metricsHandler or the mux -- is what let the requests above through:
	// the identical failing resolver still fails closed (403) for a path
	// that was never allowlisted.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("GET /api/v1/notes with a failing resolver and no allowlist entry: status = %d, want 403 (body: %s)",
			rec2.Code, rec2.Body.String())
	}
}

// TestMetricsAllowlist_GETOnlyAllowlist_LeavesHEADExposed is metricsPath's
// counterpart to TestHealthzAllowlist_GETOnlyAllowlist_LeavesHEADExposed
// above. buildServer allowlists metricsPath the same two-calls-one-per-
// method way it allowlists healthzPath (server.go), so it carries the exact
// same regression risk: net/http's ServeMux auto-serves HEAD from the
// registered "GET "+metricsPath pattern, but tenancy.Middleware does not
// extend WithAllowlist's exemption from GET to HEAD automatically (its own
// doc comment says so explicitly) -- so forgetting, or later deleting, the
// tenancy.WithAllowlist(http.MethodHead, metricsPath) call in buildServer
// would silently leave HEAD /metrics one resolver failure away from a 403.
//
// This test reproduces that gap deliberately (GET allowlisted only) as a
// permanent canary: if it ever starts failing -- HEAD suddenly returning
// 200 -- either net/http's or tenancy.Middleware's GET/HEAD behavior
// changed, or buildServer may no longer need its explicit HEAD allowlist
// entry for metricsPath.
func TestMetricsAllowlist_GETOnlyAllowlist_LeavesHEADExposed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(http.MethodGet+" "+metricsPath, metricsHandler)

	handler := tenancy.Middleware(failingResolver{}, tenancy.WithAllowlist(http.MethodGet, metricsPath))(mux)

	req := httptest.NewRequest(http.MethodHead, metricsPath, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("HEAD %s with only GET allowlisted and a failing resolver: status = %d, want 403", metricsPath, rec.Code)
	}
}

// TestBuildServer_DistributedDeploymentMode_FailsCapabilityValidation pins
// what requesting the distributed deployment mode means since the retrofit
// removed buildServer's hard refusal of it: the composition is no longer
// rejected up front, it is validated -- and with every seam resolved from
// the Preset, the distributed mode's required capabilities cannot be met.
// Kernel.Bootstrap must fail with ErrCapabilityUnsatisfied, naming the
// first shortfall: the "eventbus" seam's "eventbus.memory" implementation
// lacking MultiReplicaSafe while the mode is "distributed". Bootstrap
// performs that validation before any Subscribe or goroutine starts, so
// this test needs no Docker and never touches a network, and it guarantees
// the mode can never silently degrade into a SQLite-and-in-memory run
// under a "distributed" label.
func TestBuildServer_DistributedDeploymentMode_FailsCapabilityValidation(t *testing.T) {
	cfg := testConfig(t)
	cfg.DeploymentMode = pkgcore.DeploymentModeDistributed

	_, _, err := buildServer(context.Background(), cfg)
	if err == nil {
		t.Fatal("buildServer with DeploymentModeDistributed: want error, got nil")
	}
	if !errors.Is(err, pkgcore.ErrCapabilityUnsatisfied) {
		t.Fatalf("buildServer with DeploymentModeDistributed: error = %v, want errors.Is(err, pkgcore.ErrCapabilityUnsatisfied)", err)
	}
	for _, want := range []string{`seam "eventbus"`, `"eventbus.memory"`, "MultiReplicaSafe", `"distributed"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("buildServer with DeploymentModeDistributed: error %q does not mention %s", err, want)
		}
	}
}

// TestBuildServer_DistributedDeploymentMode_InjectedEventBus_StillFailsOnKV
// is the second half of the distributed-mode pin: even with a real
// Redis-backed EventBus injected (capabilities MultiReplicaSafe and
// SurvivesRestart -- exactly the composition the file's README worked
// example demonstrates in standalone mode), a distributed deployment still
// fails capability validation, now on the next seam: the "kv" seam's
// "kv.memory" implementation also lacks MultiReplicaSafe, and the Preset
// has no Redis-backed KVStore to swap in. An event bus alone does not make
// a distributed deployment viable -- every seam must satisfy the mode's
// requirements. Validation precedes module registration, so no Subscribe
// is ever reached, and the cleanup buildServer runs on this error path is
// equally network-free: RedisEventBus starts no goroutine and touches no
// network until the first Subscribe (its group-destroy sweep returns early
// with nothing subscribed), and a go-redis client dials lazily, so the
// unreachable 127.0.0.1:6379 address is never contacted -- this test needs
// no Docker.
func TestBuildServer_DistributedDeploymentMode_InjectedEventBus_StillFailsOnKV(t *testing.T) {
	cfg := testConfig(t)
	cfg.DeploymentMode = pkgcore.DeploymentModeDistributed
	cfg.RedisAddr = "127.0.0.1:6379"

	_, _, err := buildServer(context.Background(), cfg)
	if err == nil {
		t.Fatal("buildServer with DeploymentModeDistributed and an injected Redis EventBus: want error, got nil")
	}
	if !errors.Is(err, pkgcore.ErrCapabilityUnsatisfied) {
		t.Fatalf("buildServer with DeploymentModeDistributed and an injected Redis EventBus: error = %v, want errors.Is(err, pkgcore.ErrCapabilityUnsatisfied)", err)
	}
	for _, want := range []string{`seam "kv"`, `"kv.memory"`, "MultiReplicaSafe", `"distributed"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("buildServer with DeploymentModeDistributed and an injected Redis EventBus: error %q does not mention %s", err, want)
		}
	}
}

// notesRequest issues method against /api/v1/notes with the given bearer
// token (empty means no Authorization header at all) and optional body,
// returning the raw response for the caller to assert on -- unlike
// createNoteAs/listNotesAs above, which assert success internally, this is
// for tests that expect the request to be rejected.
func notesRequest(t *testing.T, srv *httptest.Server, method, token string, body io.Reader) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, srv.URL+"/api/v1/notes", body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set(demoUserHeader, demoOwnerUserID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s /api/v1/notes: %v", method, err)
	}
	return resp
}

// TestBuildServer_Unauthenticated_FailsClosed is the token-based
// counterpart of what was, before this round, a Host-based regression
// test: an unrecognized Host used to resolve to a shared demoDefaultTenant
// bucket any anonymous caller could read from and write to. That bucket no
// longer exists at all -- Host plays no part in the notes API's tenant
// resolution any more (server.go's middleware-chain doc comment) -- but
// the SAME fail-closed property has a new, equally real way to matter: a
// request carrying no credential, and one carrying a credential that does
// not verify, must both be refused rather than served from any shared or
// default state.
//
// Step 1 and 2 prove no Authorization header at all is refused (403 --
// tenancy.Middleware's ErrTenantUnresolved, because authn.NewPrincipalResolver
// has no Principal to read a tenant from). Step 3 proves a garbage bearer
// token is refused differently: authn.Middleware treats an unparseable
// credential as a FAILED assertion of identity, not an absence of one, and
// answers 401 immediately (before tenancy.Middleware ever runs) -- see
// go/authn/middleware.go's own doc comment on why those two failure modes
// are deliberately not the same status. Step 4 is the negative control the
// original live attack also ran: a second unauthenticated caller sees
// nothing the first one might have planted, proving there is no shared
// bucket left at all.
func TestBuildServer_Unauthenticated_FailsClosed(t *testing.T) {
	srv, _ := buildTestServer(t)

	// Step 1: GET with no Authorization header must not succeed against an
	// implicit shared tenant.
	getResp := notesRequest(t, srv, http.MethodGet, "", nil)
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusForbidden {
		respBody, _ := io.ReadAll(getResp.Body)
		t.Fatalf("GET /api/v1/notes (no Authorization) status = %d, want %d; body = %s",
			getResp.StatusCode, http.StatusForbidden, respBody)
	}

	// Step 2: POST with no Authorization header must not plant a note in a
	// shared bucket either.
	createBody, err := json.Marshal(map[string]string{"text": "planted with no credential at all"})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	postResp := notesRequest(t, srv, http.MethodPost, "", strings.NewReader(string(createBody)))
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusForbidden {
		respBody, _ := io.ReadAll(postResp.Body)
		t.Fatalf("POST /api/v1/notes (no Authorization) status = %d, want %d; body = %s",
			postResp.StatusCode, http.StatusForbidden, respBody)
	}

	// Step 3: a garbage bearer token is a FAILED assertion of identity,
	// answered 401 by authn.Middleware itself, before tenancy.Middleware
	// (and its 403) ever runs.
	garbageResp := notesRequest(t, srv, http.MethodGet, "not-a-real-token", nil)
	defer garbageResp.Body.Close()
	if garbageResp.StatusCode != http.StatusUnauthorized {
		respBody, _ := io.ReadAll(garbageResp.Body)
		t.Fatalf("GET /api/v1/notes (garbage bearer token) status = %d, want %d; body = %s",
			garbageResp.StatusCode, http.StatusUnauthorized, respBody)
	}

	// Step 4 (negative control): a second, completely independent
	// unauthenticated caller is refused too -- there is no shared bucket
	// for one anonymous caller to plant data into and another to read
	// back, which is exactly what made the original gap a real leak
	// rather than a per-caller-isolated refusal.
	getResp2 := notesRequest(t, srv, http.MethodGet, "", nil)
	defer getResp2.Body.Close()
	if getResp2.StatusCode != http.StatusForbidden {
		respBody, _ := io.ReadAll(getResp2.Body)
		t.Fatalf("GET /api/v1/notes (second unauthenticated caller) status = %d, want %d; body = %s",
			getResp2.StatusCode, http.StatusForbidden, respBody)
	}
}

// TestConfigFromEnv_Defaults verifies configFromEnv's zero-environment
// defaults. Every other test in this file drives buildServer directly
// through testConfig(t), bypassing configFromEnv (and its os.Getenv reads)
// entirely, so this closes the coverage gap round 3's smoke test flagged.
//
// Each variable configFromEnv reads is explicitly set to "" via t.Setenv,
// rather than left untouched, so this test's outcome does not depend on
// the ambient environment configFromEnv happens to run in -- PORT in
// particular is commonly preset by hosting platforms, and an ambient value
// would make this test spuriously fail (or, worse, spuriously pass for the
// wrong reason) outside a clean shell. t.Setenv also restores the previous
// value automatically once the test finishes.
func TestConfigFromEnv_Defaults(t *testing.T) {
	t.Setenv("SPEED_DEPLOYMENT_MODE", "")
	t.Setenv("PORT", "")
	t.Setenv("SPEED_DB_PATH", "")
	t.Setenv("SPEED_REDIS_ADDR", "")

	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}
	if cfg.DeploymentMode != pkgcore.DeploymentModeStandalone {
		t.Fatalf("DeploymentMode = %q, want %q", cfg.DeploymentMode, pkgcore.DeploymentModeStandalone)
	}
	if cfg.Port != defaultPort {
		t.Fatalf("Port = %q, want %q", cfg.Port, defaultPort)
	}
	if cfg.SQLitePath != defaultSQLitePath {
		t.Fatalf("SQLitePath = %q, want %q", cfg.SQLitePath, defaultSQLitePath)
	}
	if !bytes.Equal(cfg.ConfigKey, devConfigKey) {
		t.Fatalf("ConfigKey = %x, want the dev default %x", cfg.ConfigKey, devConfigKey)
	}
	if cfg.RedisAddr != "" {
		t.Fatalf("RedisAddr = %q, want the empty default (in-process bus)", cfg.RedisAddr)
	}
}

// TestConfigFromEnv_ReadsOverrides verifies each environment variable
// configFromEnv reads is actually honored.
func TestConfigFromEnv_ReadsOverrides(t *testing.T) {
	t.Setenv("SPEED_DEPLOYMENT_MODE", string(pkgcore.DeploymentModeDistributed))
	t.Setenv("PORT", "9999")
	t.Setenv("SPEED_DB_PATH", "/tmp/reference-app-configfromenv-test.db")
	t.Setenv("SPEED_CONFIG_KEY", "0f0e0d0c0b0a090807060504030201001f1e1d1c1b1a19181716151413121110")
	t.Setenv("SPEED_REDIS_ADDR", "127.0.0.1:6380")

	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}
	if cfg.DeploymentMode != pkgcore.DeploymentModeDistributed {
		t.Fatalf("DeploymentMode = %q, want %q", cfg.DeploymentMode, pkgcore.DeploymentModeDistributed)
	}
	if cfg.Port != "9999" {
		t.Fatalf("Port = %q, want %q", cfg.Port, "9999")
	}
	if cfg.SQLitePath != "/tmp/reference-app-configfromenv-test.db" {
		t.Fatalf("SQLitePath = %q, want %q", cfg.SQLitePath, "/tmp/reference-app-configfromenv-test.db")
	}
	if cfg.RedisAddr != "127.0.0.1:6380" {
		t.Fatalf("RedisAddr = %q, want %q", cfg.RedisAddr, "127.0.0.1:6380")
	}
	wantKey := []byte{
		0x0f, 0x0e, 0x0d, 0x0c, 0x0b, 0x0a, 0x09, 0x08,
		0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01, 0x00,
		0x1f, 0x1e, 0x1d, 0x1c, 0x1b, 0x1a, 0x19, 0x18,
		0x17, 0x16, 0x15, 0x14, 0x13, 0x12, 0x11, 0x10,
	}
	if !bytes.Equal(cfg.ConfigKey, wantKey) {
		t.Fatalf("ConfigKey = %x, want the decoded SPEED_CONFIG_KEY %x", cfg.ConfigKey, wantKey)
	}
}

// TestConfigFromEnv_ConfigKeyRejectsMalformedValues proves configFromEnv
// fails configuration loading on a malformed SPEED_CONFIG_KEY -- too short
// to be a 32-byte key, or not hex at all -- with a precise error, rather
// than letting a subtly wrong key reach dbkit.NewCipher (whose error would
// name only the key size) or, worse, silently sealing values with a key
// the operator did not intend.
func TestConfigFromEnv_ConfigKeyRejectsMalformedValues(t *testing.T) {
	t.Setenv("SPEED_DEPLOYMENT_MODE", "")
	t.Setenv("PORT", "")
	t.Setenv("SPEED_DB_PATH", "")

	for name, encoded := range map[string]string{
		"too short": "00ff",                                                             // 1 byte, not 32
		"not hex":   "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", // 64 chars, not hex
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SPEED_CONFIG_KEY", encoded)
			if _, err := configFromEnv(); err == nil {
				t.Fatalf("configFromEnv with SPEED_CONFIG_KEY=%q: want error, got nil", encoded)
			}
		})
	}
}

// TestConfigFromEnv_InvalidDeploymentMode_ReturnsError proves the
// pkgcore.ParseDeploymentMode error path actually propagates out of
// configFromEnv: an invalid SPEED_DEPLOYMENT_MODE value must fail
// configuration loading -- and therefore run() in main.go -- rather than
// silently fall back to the standalone default or panic.
func TestConfigFromEnv_InvalidDeploymentMode_ReturnsError(t *testing.T) {
	t.Setenv("SPEED_DEPLOYMENT_MODE", "not-a-real-deployment-mode")
	t.Setenv("PORT", "")
	t.Setenv("SPEED_DB_PATH", "")

	_, err := configFromEnv()
	if err == nil {
		t.Fatal("configFromEnv with SPEED_DEPLOYMENT_MODE=not-a-real-deployment-mode: want error, got nil")
	}
	if !errors.Is(err, pkgcore.ErrInvalidDeploymentMode) {
		t.Fatalf("configFromEnv error = %v, want it to wrap %v", err, pkgcore.ErrInvalidDeploymentMode)
	}
}

// TestBuildServer_ClientSuppliedTenantHints_Ignored is the automated,
// permanent proof that nothing a caller sends alongside a valid access
// token can override the tenant that token itself names: a forged
// "X-Tenant-ID" header, a "?tenant_id=" query parameter, and a "tenant_id"
// field smuggled into the JSON create body, every one claiming
// tenant-acme while authenticated with a token scoped to tenant-globex,
// must all be silently ignored.
//
// go/tenancy/middleware_test.go's
// TestMiddleware_IgnoresClientSuppliedTenantHints already proves the same
// property at the unit level, against a stub Resolver with no real
// authentication or persistence behind it. This test proves it again
// through the actual composed stack this app serves -- authn.Middleware,
// authn.NewPrincipalResolver, the notes Handler, and a real
// dbkit.Repository[Note] backed by SQLite. The access token's own "tid"
// claim is the only tenant source the composed server ever trusts: see
// go/authn/middleware.go's PrincipalResolver doc comment and
// go/tenancy/resolver.go's Resolver doc comment for the same rule stated
// as a hard requirement on every implementation.
func TestBuildServer_ClientSuppliedTenantHints_Ignored(t *testing.T) {
	srv, cfg := buildTestServer(t)

	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "acme-forgery-target")
	globexToken := registerAndAuthenticate(t, srv, cfg, "tenant-globex", "globex-forgery-attacker")

	createNoteAs(t, srv, acmeToken, "ACME-SECRET-forgery-target")

	// Attempt 1: a forged X-Tenant-ID header claiming tenant-acme, sent
	// alongside a token scoped to tenant-globex, must not surface acme's
	// note.
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/notes", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+globexToken)
	req.Header.Set(demoUserHeader, demoOwnerUserID)
	req.Header.Set("X-Tenant-ID", "tenant-acme")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET with forged X-Tenant-ID header: %v", err)
	}
	defer resp.Body.Close()
	var headerAttempt testListNotesResponse
	if err = json.NewDecoder(resp.Body).Decode(&headerAttempt); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(headerAttempt.Notes) != 0 {
		t.Fatalf("GET (globex token, forged header X-Tenant-ID: tenant-acme) leaked %d note(s): %+v",
			len(headerAttempt.Notes), headerAttempt.Notes)
	}

	// Attempt 2: the same forged tenant, this time as a "?tenant_id="
	// query parameter instead of a header.
	req2, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/notes?tenant_id=tenant-acme", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req2.Header.Set("Authorization", "Bearer "+globexToken)
	req2.Header.Set(demoUserHeader, demoOwnerUserID)
	resp2, err := srv.Client().Do(req2)
	if err != nil {
		t.Fatalf("GET with forged tenant_id query parameter: %v", err)
	}
	defer resp2.Body.Close()
	var queryAttempt testListNotesResponse
	if err = json.NewDecoder(resp2.Body).Decode(&queryAttempt); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(queryAttempt.Notes) != 0 {
		t.Fatalf("GET (globex token, ?tenant_id=tenant-acme) leaked %d note(s): %+v",
			len(queryAttempt.Notes), queryAttempt.Notes)
	}

	// Attempt 3: "tenant_id" smuggled into the JSON create body. It must
	// be silently ignored -- the handler decodes into the spec-generated
	// api.NotesCreateNoteRequest (internal/notes/api, derived from the
	// module's api/openapi.yaml fragment), which carries no tenant_id
	// field to decode it into -- and the created note must land under
	// globex (the token's tenant), never acme.
	forgeBody, err := json.Marshal(map[string]string{"text": "globex-body-forge-probe", "tenant_id": "tenant-acme"})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	postResp := notesRequest(t, srv, http.MethodPost, globexToken, strings.NewReader(string(forgeBody)))
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(postResp.Body)
		t.Fatalf("POST (globex token, tenant_id forged in body) status = %d, want %d; body = %s",
			postResp.StatusCode, http.StatusCreated, respBody)
	}

	// Negative control, symmetric with TestBuildServer_MultiTenantIsolation_EndToEnd
	// above: acme must still see exactly its own note (none of the three
	// forgery attempts authenticated as globex ever reached it), and
	// globex must see exactly the note attempt 3 planted under globex --
	// proving attempt 3 actually ran, not merely that it returned 201 --
	// with neither tenant's list containing the other's data.
	acmeNotes := listNotesAs(t, srv, acmeToken)
	if len(acmeNotes) != 1 || acmeNotes[0].Text != "ACME-SECRET-forgery-target" {
		t.Fatalf("acme notes after all forgery attempts = %+v, want exactly one note with text %q",
			acmeNotes, "ACME-SECRET-forgery-target")
	}
	globexNotes := listNotesAs(t, srv, globexToken)
	if len(globexNotes) != 1 || globexNotes[0].Text != "globex-body-forge-probe" {
		t.Fatalf("globex notes after all forgery attempts = %+v, want exactly one note with text %q",
			globexNotes, "globex-body-forge-probe")
	}
}

// TestBuildServer_NoteCreate_PersistsAuditEvent is this round's B3 proof:
// examples/reference-app -- root CLAUDE.md's mandatory first consumer of
// every module -- is a real consumer of go/dbkit/audit, not merely a
// package that compiles against it. It drives a real POST /api/v1/notes
// request through the full composed stack (tenancy.Middleware,
// notes.Handler, a real SQLite database) exactly as
// TestBuildServer_MultiTenantIsolation_EndToEnd above does, then reads the
// audit_events table back through a second dbkit.Open connection to the
// same SQLite file -- the identical "buildServer hands out neither its
// *gorm.DB nor a module's own service, so a second connection is the only
// reach a test has into storage" pattern public_config_test.go's
// buildSeededTestServer/seedConfigRows already use for the config
// module's own table -- and asserts on a real audit.Repository.ListByTenant
// result, not a mock or an in-memory event assertion (handler_test.go's
// TestHandler_Create_ValidText_RecordsAuditEvent already covers that
// narrower unit-level claim).
func TestBuildServer_NoteCreate_PersistsAuditEvent(t *testing.T) {
	cfg := testConfig(t)
	handler, cleanup, err := buildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			t.Errorf("cleanup: %v", cleanupErr)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	const tenantID = "tenant-acme"
	acmeToken := registerAndAuthenticate(t, srv, cfg, tenantID, "audit-creator")

	createNoteAs(t, srv, acmeToken, "buy milk")
	notes := listNotesAs(t, srv, acmeToken)
	if len(notes) != 1 {
		t.Fatalf("notes after create = %+v, want exactly 1", notes)
	}
	noteID := notes[0].ID

	auditDB, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: cfg.SQLitePath})
	if err != nil {
		t.Fatalf("open second connection to %q: %v", cfg.SQLitePath, err)
	}
	t.Cleanup(func() {
		sqlDB, dbErr := auditDB.DB()
		if dbErr != nil {
			t.Errorf("second connection handle: %v", dbErr)
			return
		}
		if closeErr := sqlDB.Close(); closeErr != nil {
			t.Errorf("close second connection: %v", closeErr)
		}
	})

	events, err := audit.NewRepository(auditDB).ListByTenant(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("ListByTenant(%q): %v", tenantID, err)
	}
	if len(events) != 1 {
		t.Fatalf("audit events for tenant %q = %+v, want exactly 1", tenantID, events)
	}

	got := events[0]
	if got.Action != "notes.note.create" {
		t.Fatalf("AuditEvent.Action = %q, want %q", got.Action, "notes.note.create")
	}
	if got.Resource().Type != "note" {
		t.Fatalf("AuditEvent.Resource().Type = %q, want %q", got.Resource().Type, "note")
	}
	if got.Resource().ID != noteID {
		t.Fatalf("AuditEvent.Resource().ID = %q, want %q", got.Resource().ID, noteID)
	}
	if got.TenantID != tenantID {
		t.Fatalf("AuditEvent.TenantID = %q, want %q", got.TenantID, tenantID)
	}
	if !got.Result().Success {
		t.Fatalf("AuditEvent.Result().Success = %v, want true", got.Result().Success)
	}
	if got.OccurredAt.IsZero() {
		t.Fatal("AuditEvent.OccurredAt is zero, want a real timestamp")
	}

	// A negative control symmetric with this test's own positive
	// assertions: an unrelated tenant's read must see none of acme's audit
	// trail -- audit_events carries a real tenant_id column precisely so
	// this remains true even though the table is platform data, not
	// dbkit.TenantScoped (see go/dbkit/audit's model.go doc comment).
	globexEvents, err := audit.NewRepository(auditDB).ListByTenant(context.Background(), "tenant-globex")
	if err != nil {
		t.Fatalf("ListByTenant(%q): %v", "tenant-globex", err)
	}
	if len(globexEvents) != 0 {
		t.Fatalf("audit events for tenant %q = %+v, want none", "tenant-globex", globexEvents)
	}
}
