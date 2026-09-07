package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/compliance"
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

// demoNotesCreatorUserID is declared in demo_subject.go, next to the other
// demo identity constants, because the running server's own glue reads it
// too (demo_notification.go's demo address table keys on it); the test
// helpers here and in notification_flow_test.go reference the same constant
// so a test's X-Demo-User-Id header always names the user the server's
// subscription will dispatch to. See demo_subject.go's comment there for
// what the id means.

// testConfig returns a serverConfig backed by a fresh, per-test temp-file
// SQLite database, so tests never share state and never touch a real file
// outside t.TempDir(). Memberships is always a fresh, empty
// signInMemberships -- tests that need an account to actually reach a
// tenant grant it explicitly via registerAndAuthenticate below, keeping
// the same reference buildServer itself wires (and attaches to org) so a
// test's grant is visible to the running server.
//
// NotificationIndexKey is set because every boot wires the notification
// module's two contact indexers from the struct field directly -- the
// APP_NOTIFICATION_INDEX_KEY environment default only exists on the
// configFromEnv path, which this helper never takes -- and an empty key
// fails the boot before the first request. PKILocalKeyCipherKey,
// AuthnBlindIndexKey and AuthnPIICipherKey are set for the identical
// reason: buildServer reads all three straight off cfg (APP_ROOT_KEY's
// derivation and each key's own individual env var both live in
// configFromEnv, which this helper bypasses entirely), so an empty value
// here fails the boot the same way an empty ConfigKey would.
func testConfig(t *testing.T) serverConfig {
	t.Helper()
	return serverConfig{
		DeploymentMode:       pkgcore.DeploymentModeStandalone,
		Port:                 "0",
		SQLitePath:           filepath.Join(t.TempDir(), "reference-app-test.db"),
		ConfigKey:            devConfigKey,
		OrgIndexKey:          devOrgIndexKey,
		NotificationIndexKey: devNotificationIndexKey,
		PKILocalKeyCipherKey: devPKILocalKeyCipherKey,
		AuthnBlindIndexKey:   devBlindIndexKey,
		AuthnPIICipherKey:    devPIICipherKey,
		HostTenants:          demoHostTenants,
		Memberships:          newSignInMemberships(),
	}
}

// buildTestServer wires up buildServer's real output behind an
// httptest.Server, so tests exercise the exact composed handler main.go
// itself serves -- the authn+tenancy middleware chain, the notes Module's
// real handler, and a real (if temp-file) SQLite database -- not a mock of
// any of them. It returns the serverConfig alongside the server so a
// caller can reach cfg.Memberships to grant a demo account tenant
// membership after registering it (registerAndAuthenticate does this), and
// buildServer's wired *compliance.Module -- the one reach a test has into
// the retention/erasure/export services, which compliance_flow_test.go
// drives (every other flow test in this package is HTTP-driven and discards
// it, exactly as buildServer's own doc comment describes main.go doing).
func buildTestServer(t *testing.T) (*httptest.Server, serverConfig, *compliance.Module) {
	t.Helper()

	cfg := testConfig(t)
	handler, cleanup, complianceModule, err := buildServer(context.Background(), cfg)
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
	return srv, cfg, complianceModule
}

// registerAndAuthenticate registers a fresh demo account through authn's
// real HTTP surface (POST /api/v1/authn/register), grants it membership in
// tenant via cfg.Memberships (the seam buildServer itself wires authn's
// MembershipReader to -- see sign_in_memberships.go's own doc comment:
// org's rows answer customer-tenant questions first, and the grant this
// helper records is the in-process test shortcut that answers when org has
// no row for the pair), signs it in with a tenant_id request naming
// tenant, and returns the resulting bearer access token.
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
// comment explains why Host does not) -- and returns the created note's id,
// for a caller that needs to act on that exact note afterward (consult_flow_test.go's
// suggestion requests, keyed on note_id).
//
// Two demo headers ride along, each naming a different thing:
//
//   - X-Demo-User names WHO is acting for the rbac gate (demo_subject.go's
//     demoUserHeader). demoOwnerUserID holds every permission, so these two
//     helpers exercise the happy path; the tests that exercise the gate
//     itself send other users, or none.
//   - X-Demo-User-Id names the creating user for notes' own SubjectResolver
//     (demoNotesSubjectResolver in server.go) -- the value that lands in the
//     note's CreatorUserID; see demoNotesCreatorUserID above.
func createNoteAs(t *testing.T, srv *httptest.Server, token, text string) string {
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
	req.Header.Set(demoOrgUserHeader, demoNotesCreatorUserID)

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

	var created testNote
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create-note response: %v", err)
	}
	if created.ID == "" {
		t.Fatal("create-note response carried no id")
	}
	return created.ID
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
	srv, cfg, _ := buildTestServer(t)

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
//
// A non-empty user additionally sends X-Demo-User-Id (demoNotesCreatorUserID):
// notes' create handler attributes the note through its own SubjectResolver
// (demoNotesSubjectResolver in server.go), which reads that header first and
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
		req.Header.Set(demoUserHeader, user)
		req.Header.Set(demoOrgUserHeader, demoNotesCreatorUserID)
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
	srv, cfg, _ := buildTestServer(t)
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
	srv, cfg, _ := buildTestServer(t)
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
	srv, cfg, _ := buildTestServer(t)
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
	srv, _, _ := buildTestServer(t)

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
	srv, _, _ := buildTestServer(t)

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
	srv, _, _ := buildTestServer(t)

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

// fakeSMSGatewayURL is a never-dialed HTTP endpoint used by every
// distributed-mode test below that needs authn's own wiring-time "SMS
// sender" validation to pass (so the Kernel-level assertion the test
// actually pins is what runs) without touching a network:
// authn.NewHTTPSMSSender's own construction never dials anything, and none
// of these tests exercises the phone-login flow that would actually POST
// to it.
const fakeSMSGatewayURL = "http://127.0.0.1:1/sms"

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
//
// cfg.SMSGatewayURL is set to fakeSMSGatewayURL so that authn's own
// wiring-time SMS-sender validation, which buildServer reaches BEFORE
// Kernel.Bootstrap (see buildServer's authn wiring comment), does not mask
// the Kernel-level failure this test actually pins;
// TestBuildServer_DistributedDeploymentMode_NoSMSGateway_FailsClosed below
// is what proves that earlier validation on its own.
func TestBuildServer_DistributedDeploymentMode_FailsCapabilityValidation(t *testing.T) {
	cfg := testConfig(t)
	cfg.DeploymentMode = pkgcore.DeploymentModeDistributed
	cfg.SMSGatewayURL = fakeSMSGatewayURL

	_, _, _, err := buildServer(context.Background(), cfg)
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

// TestBuildServer_DistributedDeploymentMode_RedisConfigured_StillFailsOnMailer
// is the second half of the distributed-mode pin, rewritten for this
// round's env-driven wiring: APP_REDIS_ADDR now composes BOTH the
// "eventbus" and the "kv" seam onto one shared *redis.Client (buildServer's
// kernel-options doc comment explains why one Redis instance backs both),
// so a distributed deployment with only cfg.RedisAddr set no longer fails
// on "kv" the way it used to before this round -- it clears both
// "eventbus" and "kv" and now fails capability validation on the NEXT seam
// Kernel.Bootstrap resolves: "mailer", whose Preset default
// ("mailer.console") also lacks MultiReplicaSafe, and this test configures
// no APP_SMTP_* composition to swap it for. This is exactly the "one
// seam wired isn't enough" property root CLAUDE.md's distributed-mode
// section documents, now demonstrated one seam later than before this
// round. Validation precedes module registration, so no Subscribe is ever
// reached, and the cleanup buildServer runs on this error path is equally
// network-free: RedisEventBus starts no goroutine and touches no network
// until the first Subscribe (its group-destroy sweep returns early with
// nothing subscribed), a go-redis client dials lazily, and kv/redis.
// NewKVStore's own first operation is what reaches for the server -- so the
// unreachable 127.0.0.1:6379 address is never contacted by either seam,
// and this test needs no Docker.
func TestBuildServer_DistributedDeploymentMode_RedisConfigured_StillFailsOnMailer(t *testing.T) {
	cfg := testConfig(t)
	cfg.DeploymentMode = pkgcore.DeploymentModeDistributed
	cfg.RedisAddr = "127.0.0.1:6379"
	cfg.SMSGatewayURL = fakeSMSGatewayURL

	_, _, _, err := buildServer(context.Background(), cfg)
	if err == nil {
		t.Fatal("buildServer with DeploymentModeDistributed and Redis configured: want error, got nil")
	}
	if !errors.Is(err, pkgcore.ErrCapabilityUnsatisfied) {
		t.Fatalf("buildServer with DeploymentModeDistributed and Redis configured: error = %v, want errors.Is(err, pkgcore.ErrCapabilityUnsatisfied)", err)
	}
	for _, want := range []string{`seam "mailer"`, `"mailer.console"`, "MultiReplicaSafe", `"distributed"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("buildServer with DeploymentModeDistributed and Redis configured: error %q does not mention %s", err, want)
		}
	}
}

// TestBuildServer_DistributedDeploymentMode_NoSMSGateway_FailsClosed proves
// the negative half of this round's authn "SMS sender" wiring: a
// distributed composition that forgets APP_SMS_GATEWAY_URL must fail
// closed with authn.ErrMissingDistributedSMSSender, rather than silently
// keeping the console transport nobody in a distributed replica pool is
// reading -- the exact property docs/internal/03-deployment-modes.md's
// authn round note describes and this app's own wiring never actually
// exercised before this round, since it used to pass WithSMSSender(
// NewConsoleSMSSender(...)) unconditionally regardless of deployment mode.
// This is authn's OWN wiring-time validation (authn.NewModule's
// newOptions), which buildServer reaches before it ever calls
// pkgcore.NewKernel(...).Bootstrap -- so this failure fires regardless of
// whether any other seam (Redis, S3, SMTP) is configured, and this test
// configures none of them, needing no Docker and touching no network.
func TestBuildServer_DistributedDeploymentMode_NoSMSGateway_FailsClosed(t *testing.T) {
	cfg := testConfig(t)
	cfg.DeploymentMode = pkgcore.DeploymentModeDistributed

	_, _, _, err := buildServer(context.Background(), cfg)
	if err == nil {
		t.Fatal("buildServer with DeploymentModeDistributed and no APP_SMS_GATEWAY_URL: want error, got nil")
	}
	if !errors.Is(err, authn.ErrMissingDistributedSMSSender) {
		t.Fatalf("buildServer with DeploymentModeDistributed and no APP_SMS_GATEWAY_URL: error = %v, want errors.Is(err, authn.ErrMissingDistributedSMSSender)", err)
	}
}

// A no-Docker "positive" counterpart to the three tests above -- one that
// composes every seam onto a fake, unreachable address and asserts
// buildServer succeeds -- was deliberately NOT added here, and this is a
// real finding rather than a silent gap: this app's own seedDemoGrants
// (demo_subject.go), which every buildServer call runs unconditionally
// after Bootstrap to seed the demo tenants' built-in roles, makes a
// SYNCHRONOUS rbac.Service call that publishes on whatever EventBus
// Bootstrap resolved -- eventbus/redis.EventBus.Publish genuinely appends
// to the Redis stream inline, unlike Subscribe's fire-and-forget background
// reader. Pointing cfg.RedisAddr at a fake address therefore fails this
// seeding step with a real "connection refused" the moment Redis is
// unreachable, regardless of deployment mode -- proven empirically while
// writing this round's tests. So the positive half of this property
// (assembly succeeds when every seam is genuinely satisfied) cannot be
// proven at the unit tier without either standing up real infrastructure
// (which belongs in a Docker-backed integration tier, not here) or
// special-casing seedDemoGrants for tests (which would test a different
// wiring than main() runs, the exact anti-pattern buildServer's own doc
// comment warns against). The genuine, real-infrastructure proof is
// examples/reference-app/integration_test/distributed_mode_test.go.

// notesRequest issues method against /api/v1/notes with the given bearer
// token (empty means no Authorization header at all) and optional body,
// returning the raw response for the caller to assert on -- unlike
// createNoteAs/listNotesAs above, which assert success internally, this is
// mostly for tests that expect the request to be rejected.
//
// It always carries X-Demo-User-Id too (demoNotesCreatorUserID): one POST
// caller -- the tenant-hint forgery test below -- expects 201 and reaches
// notes' create handler, which resolves the creator through the same seam,
// while every rejection here dies upstream of that handler, where the
// additional header is inert.
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
	req.Header.Set(demoOrgUserHeader, demoNotesCreatorUserID)
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
	srv, _, _ := buildTestServer(t)

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
	t.Setenv("APP_DEPLOYMENT_MODE", "")
	t.Setenv("PORT", "")
	t.Setenv("APP_DB_PATH", "")
	t.Setenv("APP_REDIS_ADDR", "")
	t.Setenv("APP_DEMO_USERS_PASSWORD", "")
	t.Setenv("APP_OBJECT_STORE_ROOT", "")
	t.Setenv("APP_DISABLE_DEMO_USER_HEADER", "")
	t.Setenv("APP_TRUSTED_PROXIES", "")
	t.Setenv("APP_READ_FLY_CLIENT_IP", "")
	t.Setenv("APP_FAIL_SELF_SERVICE_PROVISION", "")

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
	if cfg.DemoUsersPassword != "" {
		t.Fatalf("DemoUsersPassword = %q, want the empty default (demo-user seed skipped)", cfg.DemoUsersPassword)
	}
	if cfg.ObjectStoreRoot != "" {
		t.Fatalf("ObjectStoreRoot = %q, want the empty default (Preset local-store directory)", cfg.ObjectStoreRoot)
	}
	if cfg.DisableDemoUserHeader {
		t.Fatal("DisableDemoUserHeader = true, want false (the default: demoUserHeader keeps winning, unchanged)")
	}
	if cfg.ReadFlyClientIP {
		t.Fatal("ReadFlyClientIP = true, want false (the default: no vendor header is read, authn's fail-closed shape)")
	}
	if cfg.failSelfServiceProvision != nil {
		t.Fatal("failSelfServiceProvision armed with an unset APP_FAIL_SELF_SERVICE_PROVISION: an absent variable must leave the self-service provisioning untouched (production behaviour unchanged)")
	}
}

// TestConfigFromEnv_FailSelfServiceProvision_ParseAndDisableSemantics pins
// APP_FAIL_SELF_SERVICE_PROVISION's parse contract (see
// failSelfServiceProvisionEnv's own doc comment): absent or "0" leaves the
// self-service provisioning uninjected, a positive integer N arms an
// injection whose first N provisioning attempts of each account fail and
// whose later attempts of the same account succeed, and anything else
// (not a number, or a negative count) refuses boot with the variable
// named. Failing before the switch existed: configFromEnv ignored the
// variable entirely, so no value of it could arm or refuse anything.
func TestConfigFromEnv_FailSelfServiceProvision_ParseAndDisableSemantics(t *testing.T) {
	t.Setenv("APP_DEPLOYMENT_MODE", "")
	t.Setenv("PORT", "")
	t.Setenv("APP_DB_PATH", "")

	t.Run("0 disables the injection", func(t *testing.T) {
		t.Setenv("APP_FAIL_SELF_SERVICE_PROVISION", "0")
		cfg, err := configFromEnv()
		if err != nil {
			t.Fatalf("configFromEnv() error = %v", err)
		}
		if cfg.failSelfServiceProvision != nil {
			t.Fatal("failSelfServiceProvision armed with the variable at 0, want the disabled default")
		}
	})
	t.Run("a positive count arms the injection with exactly that budget", func(t *testing.T) {
		t.Setenv("APP_FAIL_SELF_SERVICE_PROVISION", "2")
		cfg, err := configFromEnv()
		if err != nil {
			t.Fatalf("configFromEnv() error = %v", err)
		}
		if cfg.failSelfServiceProvision == nil {
			t.Fatal("failSelfServiceProvision nil with the variable at 2, want the armed injection")
		}
		// The first two provisioning attempts of one account fail...
		if err := cfg.failSelfServiceProvision("budget-account"); err == nil {
			t.Fatal("the armed injection's first attempt of an account did not fail")
		}
		if err := cfg.failSelfServiceProvision("budget-account"); err == nil {
			t.Fatal("the armed injection's second attempt of an account did not fail")
		}
		// ...and the third succeeds: the budget is per account, so the
		// count is the number of failed attempts, never a permanent block.
		if err := cfg.failSelfServiceProvision("budget-account"); err != nil {
			t.Fatalf("the armed injection failed an attempt past its budget: %v", err)
		}
		// A different account starts its own fresh budget of N.
		if err := cfg.failSelfServiceProvision("another-budget-account"); err == nil {
			t.Fatal("the armed injection's first attempt of a second account did not fail")
		}
	})
	t.Run("not a number refuses boot", func(t *testing.T) {
		t.Setenv("APP_FAIL_SELF_SERVICE_PROVISION", "one")
		_, err := configFromEnv()
		if err == nil {
			t.Fatal("configFromEnv() error = nil, want a parse refusal naming APP_FAIL_SELF_SERVICE_PROVISION")
		}
		if !strings.Contains(err.Error(), "APP_FAIL_SELF_SERVICE_PROVISION") {
			t.Fatalf("parse refusal does not name the variable: %v", err)
		}
	})
	t.Run("a negative count refuses boot", func(t *testing.T) {
		t.Setenv("APP_FAIL_SELF_SERVICE_PROVISION", "-1")
		_, err := configFromEnv()
		if err == nil {
			t.Fatal("configFromEnv() error = nil, want a refusal of a negative count")
		}
		if !strings.Contains(err.Error(), "APP_FAIL_SELF_SERVICE_PROVISION") {
			t.Fatalf("negative-count refusal does not name the variable: %v", err)
		}
	})
}

// TestConfigFromEnv_ReadsOverrides verifies each environment variable
// configFromEnv reads is actually honored.
func TestConfigFromEnv_ReadsOverrides(t *testing.T) {
	t.Setenv("APP_DEPLOYMENT_MODE", string(pkgcore.DeploymentModeDistributed))
	t.Setenv("PORT", "9999")
	t.Setenv("APP_DB_PATH", "/tmp/reference-app-configfromenv-test.db")
	t.Setenv("APP_CONFIG_KEY", "0f0e0d0c0b0a090807060504030201001f1e1d1c1b1a19181716151413121110")
	t.Setenv("APP_REDIS_ADDR", "127.0.0.1:6380")
	t.Setenv("APP_DEMO_USERS_PASSWORD", "env demo seed passphrase")
	t.Setenv("APP_OBJECT_STORE_ROOT", "/var/lib/reference-app/objects")
	t.Setenv("APP_DISABLE_DEMO_USER_HEADER", "1")
	t.Setenv("APP_TRUSTED_PROXIES", " 172.16.0.0/12, 203.0.113.10 , ")
	t.Setenv("APP_READ_FLY_CLIENT_IP", "true")
	t.Setenv("APP_FAIL_SELF_SERVICE_PROVISION", "2")

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
	if cfg.DemoUsersPassword != "env demo seed passphrase" {
		t.Fatalf("DemoUsersPassword = %q, want the APP_DEMO_USERS_PASSWORD value", cfg.DemoUsersPassword)
	}
	if cfg.ObjectStoreRoot != "/var/lib/reference-app/objects" {
		t.Fatalf("ObjectStoreRoot = %q, want the APP_OBJECT_STORE_ROOT value", cfg.ObjectStoreRoot)
	}
	if !cfg.DisableDemoUserHeader {
		t.Fatal("DisableDemoUserHeader = false, want true (APP_DISABLE_DEMO_USER_HEADER set to a non-empty value)")
	}
	wantProxies := []string{"172.16.0.0/12", "203.0.113.10"}
	if len(cfg.TrustedProxies) != len(wantProxies) {
		t.Fatalf("TrustedProxies = %v, want %v", cfg.TrustedProxies, wantProxies)
	}
	for i := range wantProxies {
		if cfg.TrustedProxies[i] != wantProxies[i] {
			t.Errorf("TrustedProxies[%d] = %q, want %q", i, cfg.TrustedProxies[i], wantProxies[i])
		}
	}
	if !cfg.ReadFlyClientIP {
		t.Fatal("ReadFlyClientIP = false, want true (APP_READ_FLY_CLIENT_IP set to 'true' alongside the proxy declaration)")
	}
	wantKey := []byte{
		0x0f, 0x0e, 0x0d, 0x0c, 0x0b, 0x0a, 0x09, 0x08,
		0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01, 0x00,
		0x1f, 0x1e, 0x1d, 0x1c, 0x1b, 0x1a, 0x19, 0x18,
		0x17, 0x16, 0x15, 0x14, 0x13, 0x12, 0x11, 0x10,
	}
	if !bytes.Equal(cfg.ConfigKey, wantKey) {
		t.Fatalf("ConfigKey = %x, want the decoded APP_CONFIG_KEY %x", cfg.ConfigKey, wantKey)
	}
	if cfg.failSelfServiceProvision == nil {
		t.Fatal("failSelfServiceProvision nil with APP_FAIL_SELF_SERVICE_PROVISION=2, want the armed injection")
	}
	// The variable's value is the number of attempts to fail: with N=2 the
	// first two attempts of one account fail and the third succeeds.
	for attempt := 1; attempt <= 2; attempt++ {
		if err := cfg.failSelfServiceProvision("override-account"); err == nil {
			t.Fatalf("the armed injection's attempt %d of an account did not fail (N=2)", attempt)
		}
	}
	if err := cfg.failSelfServiceProvision("override-account"); err != nil {
		t.Fatalf("the armed injection failed an attempt past its two-attempt budget: %v", err)
	}
}

// TestConfigFromEnv_ReadFlyClientIPWithoutTrustedProxies_ReturnsError pins
// the declaration-pair rule readFlyClientIPEnv's own doc comment states:
// reading Fly-Client-IP is authorized only for a deployment whose proxy is
// declared, so 'true' with an empty APP_TRUSTED_PROXIES refuses boot --
// the pair would never read the header and would silently keep recording
// the proxy itself, the defect the declaration exists to fix. A value that
// is not a strict bool is refused the same way.
func TestConfigFromEnv_ReadFlyClientIPWithoutTrustedProxies_ReturnsError(t *testing.T) {
	t.Setenv("APP_DEPLOYMENT_MODE", "")
	t.Setenv("PORT", "")
	t.Setenv("APP_DB_PATH", "")
	t.Setenv("APP_TRUSTED_PROXIES", "")

	t.Run("true with no proxy declared", func(t *testing.T) {
		t.Setenv("APP_READ_FLY_CLIENT_IP", "true")
		if _, err := configFromEnv(); err == nil {
			t.Fatal("configFromEnv() error = nil, want a refusal naming both variables")
		}
	})
	t.Run("not a strict bool", func(t *testing.T) {
		t.Setenv("APP_READ_FLY_CLIENT_IP", "yes")
		if _, err := configFromEnv(); err == nil {
			t.Fatal("configFromEnv() error = nil, want a bool-parse refusal")
		}
	})
	t.Run("false with no proxy declared stays accepted", func(t *testing.T) {
		t.Setenv("APP_READ_FLY_CLIENT_IP", "false")
		cfg, err := configFromEnv()
		if err != nil {
			t.Fatalf("configFromEnv() error = %v", err)
		}
		if cfg.ReadFlyClientIP {
			t.Fatal("ReadFlyClientIP = true, want false")
		}
	})
}

// TestConfigFromEnv_ObjectStoreRootWithS3_ReturnsError proves the
// "objectstore"-seam ambiguity rule objectStoreRootEnv's own doc comment
// states: a complete APP_S3_* composition and APP_OBJECT_STORE_ROOT both
// name a store for the one seam, so configFromEnv refuses the combination
// loudly -- never by silently preferring one -- while each composition on
// its own stays accepted. TestConfigFromEnv_ReadsOverrides already pins
// the root-alone side; the S3-alone control below is the other half,
// proving the refusal is caused by the combination rather than by the S3
// set itself.
func TestConfigFromEnv_ObjectStoreRootWithS3_ReturnsError(t *testing.T) {
	t.Setenv("APP_DEPLOYMENT_MODE", "")
	t.Setenv("PORT", "")
	t.Setenv("APP_DB_PATH", "")

	t.Setenv(s3EndpointEnv, "https://objects.example.test")
	t.Setenv(s3BucketEnv, "bucket")
	t.Setenv(s3AccessKeyEnv, "key")
	t.Setenv(s3SecretKeyEnv, "secret")

	t.Setenv(objectStoreRootEnv, "/var/lib/reference-app/objects")
	if _, err := configFromEnv(); err == nil {
		t.Fatal("configFromEnv with both APP_OBJECT_STORE_ROOT and a complete APP_S3_* composition: want error, got nil")
	} else if !strings.Contains(err.Error(), objectStoreRootEnv) {
		t.Fatalf("configFromEnv error = %v, want it to name %s", err, objectStoreRootEnv)
	}

	t.Setenv(objectStoreRootEnv, "")
	if _, err := configFromEnv(); err != nil {
		t.Fatalf("configFromEnv with the complete S3 composition alone: %v", err)
	}
}

// TestConfigFromEnv_ConfigKeyRejectsMalformedValues proves configFromEnv
// fails configuration loading on a malformed APP_CONFIG_KEY -- too short
// to be a 32-byte key, or not hex at all -- with a precise error, rather
// than letting a subtly wrong key reach dbkit.NewCipher (whose error would
// name only the key size) or, worse, silently sealing values with a key
// the operator did not intend.
func TestConfigFromEnv_ConfigKeyRejectsMalformedValues(t *testing.T) {
	t.Setenv("APP_DEPLOYMENT_MODE", "")
	t.Setenv("PORT", "")
	t.Setenv("APP_DB_PATH", "")

	for name, encoded := range map[string]string{
		"too short": "00ff",                                                             // 1 byte, not 32
		"not hex":   "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", // 64 chars, not hex
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("APP_CONFIG_KEY", encoded)
			if _, err := configFromEnv(); err == nil {
				t.Fatalf("configFromEnv with APP_CONFIG_KEY=%q: want error, got nil", encoded)
			}
		})
	}
}

// TestConfigFromEnv_InvalidDeploymentMode_ReturnsError proves the
// pkgcore.ParseDeploymentMode error path actually propagates out of
// configFromEnv: an invalid APP_DEPLOYMENT_MODE value must fail
// configuration loading -- and therefore run() in main.go -- rather than
// silently fall back to the standalone default or panic.
func TestConfigFromEnv_InvalidDeploymentMode_ReturnsError(t *testing.T) {
	t.Setenv("APP_DEPLOYMENT_MODE", "not-a-real-deployment-mode")
	t.Setenv("PORT", "")
	t.Setenv("APP_DB_PATH", "")

	_, err := configFromEnv()
	if err == nil {
		t.Fatal("configFromEnv with APP_DEPLOYMENT_MODE=not-a-real-deployment-mode: want error, got nil")
	}
	if !errors.Is(err, pkgcore.ErrInvalidDeploymentMode) {
		t.Fatalf("configFromEnv error = %v, want it to wrap %v", err, pkgcore.ErrInvalidDeploymentMode)
	}
}

// rootKeyEnvVars lists every environment variable an explicit individual
// key can be set through, in the same order rootKeyEnv's own doc comment
// lists the six key materials. The root-key tests below clear all six
// before setting APP_ROOT_KEY, so every key is proven to resolve through
// the derivation path with nothing left over from the ambient environment.
var rootKeyEnvVars = []string{
	configKeyEnv, orgIndexKeyEnv, notificationIndexKeyEnv,
	pkiLocalKeyCipherKeyEnv, authnBlindIndexKeyEnv, authnPIICipherKeyEnv,
}

// clearRootKeyOverrides sets every one of rootKeyEnvVars to "" via
// t.Setenv, so a root-key test's outcome depends only on APP_ROOT_KEY
// and never on an individual override left set by a previous test or the
// ambient shell.
func clearRootKeyOverrides(t *testing.T) {
	t.Helper()
	for _, env := range rootKeyEnvVars {
		t.Setenv(env, "")
	}
}

// TestConfigFromEnv_RootKey_DerivesAllSixKeys proves APP_ROOT_KEY alone
// -- no individual key env var set -- derives every one of the six key
// materials rootKeyEnv's own doc comment lists, and that configFromEnv's
// derivation matches dbkit.DeriveKey called directly with this file's own
// rootKeyPurpose* constants: not merely "some non-default bytes landed in
// cfg", but the exact key a caller who knew the root and the purpose
// string could reproduce independently.
func TestConfigFromEnv_RootKey_DerivesAllSixKeys(t *testing.T) {
	t.Setenv("APP_DEPLOYMENT_MODE", "")
	t.Setenv("PORT", "")
	t.Setenv("APP_DB_PATH", "")
	clearRootKeyOverrides(t)

	rootKey := sha256.Sum256([]byte("TestConfigFromEnv_RootKey_DerivesAllSixKeys root secret"))
	t.Setenv(rootKeyEnv, hex.EncodeToString(rootKey[:]))

	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}

	for _, tt := range []struct {
		name    string
		got     []byte
		purpose string
	}{
		{"ConfigKey", cfg.ConfigKey, rootKeyPurposeConfigCipher},
		{"OrgIndexKey", cfg.OrgIndexKey, rootKeyPurposeOrgIndex},
		{"NotificationIndexKey", cfg.NotificationIndexKey, rootKeyPurposeNotificationIndex},
		{"PKILocalKeyCipherKey", cfg.PKILocalKeyCipherKey, rootKeyPurposePKILocalKeyCipher},
		{"AuthnBlindIndexKey", cfg.AuthnBlindIndexKey, rootKeyPurposeAuthnBlindIndex},
		{"AuthnPIICipherKey", cfg.AuthnPIICipherKey, rootKeyPurposeAuthnPIICipher},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want, deriveErr := dbkit.DeriveKey(rootKey[:], tt.purpose)
			if deriveErr != nil {
				t.Fatalf("dbkit.DeriveKey(rootKey, %q): %v", tt.purpose, deriveErr)
			}
			if !bytes.Equal(tt.got, want) {
				t.Fatalf("cfg.%s = %x, want dbkit.DeriveKey(rootKey, %q) = %x", tt.name, tt.got, tt.purpose, want)
			}
		})
	}

	// The six derived keys must also be pairwise distinct -- a purpose
	// string collision (or a resolveKey wiring mistake reusing one
	// derived value for two fields) would silently reintroduce the exact
	// key-material reuse dbkit's key-separation rule forbids.
	keys := map[string][]byte{
		"ConfigKey": cfg.ConfigKey, "OrgIndexKey": cfg.OrgIndexKey,
		"NotificationIndexKey": cfg.NotificationIndexKey, "PKILocalKeyCipherKey": cfg.PKILocalKeyCipherKey,
		"AuthnBlindIndexKey": cfg.AuthnBlindIndexKey, "AuthnPIICipherKey": cfg.AuthnPIICipherKey,
	}
	seen := make(map[string]string, len(keys))
	for name, key := range keys {
		digest := hex.EncodeToString(key)
		if other, ok := seen[digest]; ok {
			t.Fatalf("%s and %s derived to the identical key %s", name, other, digest)
		}
		seen[digest] = name
	}
}

// TestConfigFromEnv_RootKey_IndividualOverrideWins proves the precedence
// order rootKeyEnv's own doc comment states: with both APP_ROOT_KEY and
// one individual key env var (APP_CONFIG_KEY) set, the explicit
// individual value wins for that one key, while every other key still
// resolves through the root-key derivation -- the "power users can still
// override any single one" half of the design.
func TestConfigFromEnv_RootKey_IndividualOverrideWins(t *testing.T) {
	t.Setenv("APP_DEPLOYMENT_MODE", "")
	t.Setenv("PORT", "")
	t.Setenv("APP_DB_PATH", "")
	clearRootKeyOverrides(t)

	rootKey := sha256.Sum256([]byte("TestConfigFromEnv_RootKey_IndividualOverrideWins root secret"))
	t.Setenv(rootKeyEnv, hex.EncodeToString(rootKey[:]))

	explicitConfigKey := sha256.Sum256([]byte("TestConfigFromEnv_RootKey_IndividualOverrideWins explicit APP_CONFIG_KEY"))
	t.Setenv(configKeyEnv, hex.EncodeToString(explicitConfigKey[:]))

	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}

	if !bytes.Equal(cfg.ConfigKey, explicitConfigKey[:]) {
		t.Fatalf("cfg.ConfigKey = %x, want the explicit %s value %x (it must win over the APP_ROOT_KEY derivation)",
			cfg.ConfigKey, configKeyEnv, explicitConfigKey[:])
	}

	wantOrgIndexKey, err := dbkit.DeriveKey(rootKey[:], rootKeyPurposeOrgIndex)
	if err != nil {
		t.Fatalf("dbkit.DeriveKey: %v", err)
	}
	if !bytes.Equal(cfg.OrgIndexKey, wantOrgIndexKey) {
		t.Fatalf("cfg.OrgIndexKey = %x, want it to still resolve through the APP_ROOT_KEY derivation (%x) since %s was never set",
			cfg.OrgIndexKey, wantOrgIndexKey, orgIndexKeyEnv)
	}
}

// TestBuildServer_RootKeyAlone_AllSixDerivedKeysWorkForTheirRealPurpose is
// this round's own end-to-end proof: APP_ROOT_KEY set alone (every
// individual key env var cleared), configFromEnv resolves all six key
// materials through dbkit.DeriveKey, buildServer boots a real composed
// server from the result, and every one of the six derived keys is
// exercised through the real mechanism it protects -- never merely "no
// error from NewCipher/NewBlindIndexer".
//
// ConfigKey, OrgIndexKey and NotificationIndexKey are proven with a real
// encrypt/decrypt or Index/Equal round trip through the exact dbkit
// primitive (and, for the two blind-index keys, the exact column name and
// normalizer) buildServer itself wires them into -- see server.go's own
// dbkit.NewBlindIndexer("email_index", ...) and
// dbkit.NewBlindIndexer("contact_email_index"/"contact_phone_index", ...)
// call sites. PKILocalKeyCipherKey, AuthnBlindIndexKey and
// AuthnPIICipherKey are proven together by a real register-then-login
// round trip through the actual composed HTTP stack: registration
// encrypts the new user's email under AuthnPIICipherKey and blind-indexes
// it under AuthnBlindIndexKey, and login can only succeed if the very
// same derived AuthnBlindIndexKey both wrote and reads back that index
// value -- while the returned, verified access token proves
// PKILocalKeyCipherKey correctly round-tripped pki's persisted signing
// key well enough to both mint and verify a real EdDSA-signed token.
func TestBuildServer_RootKeyAlone_AllSixDerivedKeysWorkForTheirRealPurpose(t *testing.T) {
	t.Setenv("APP_DEPLOYMENT_MODE", "")
	t.Setenv("PORT", "0")
	t.Setenv("APP_DB_PATH", filepath.Join(t.TempDir(), "reference-app-rootkey-e2e-test.db"))
	t.Setenv("APP_REDIS_ADDR", "")
	t.Setenv("APP_DEMO_USERS_PASSWORD", "")
	clearRootKeyOverrides(t)

	rootKey := sha256.Sum256([]byte("TestBuildServer_RootKeyAlone root secret"))
	t.Setenv(rootKeyEnv, hex.EncodeToString(rootKey[:]))

	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}
	cfg.HostTenants = demoHostTenants
	cfg.Memberships = newSignInMemberships()

	// ConfigKey: the exact mechanism go/config's Sensitive values are
	// sealed with (config.WithCipher over dbkit.NewCipher, per
	// go/config/AGENTS.md) -- a real Encrypt/Decrypt round trip under the
	// derived key.
	configCipher, err := dbkit.NewCipher(cfg.ConfigKey)
	if err != nil {
		t.Fatalf("dbkit.NewCipher(cfg.ConfigKey): %v", err)
	}
	const configPlaintext = "sensitive config value protected by the derived ConfigKey"
	ciphertext, err := configCipher.Encrypt([]byte(configPlaintext))
	if err != nil {
		t.Fatalf("configCipher.Encrypt: %v", err)
	}
	decrypted, err := configCipher.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("configCipher.Decrypt: %v", err)
	}
	if string(decrypted) != configPlaintext {
		t.Fatalf("configCipher round trip = %q, want %q", decrypted, configPlaintext)
	}

	// OrgIndexKey and NotificationIndexKey: the exact BlindIndexer
	// construction (column name and normalizer included) buildServer
	// itself wires org.WithEmailIndexer and the notification contact
	// indexers from -- a real Index/Equal round trip under each derived
	// key.
	orgIndexer, err := dbkit.NewBlindIndexer("email_index", cfg.OrgIndexKey, dbkit.NormalizeEmail)
	if err != nil {
		t.Fatalf("dbkit.NewBlindIndexer(org email_index): %v", err)
	}
	assertBlindIndexRoundTrip(t, orgIndexer, "invitee@example.com")

	contactEmailIndexer, err := dbkit.NewBlindIndexer("contact_email_index", cfg.NotificationIndexKey, dbkit.NormalizeEmail)
	if err != nil {
		t.Fatalf("dbkit.NewBlindIndexer(notification contact_email_index): %v", err)
	}
	assertBlindIndexRoundTrip(t, contactEmailIndexer, "contact@example.com")

	contactPhoneIndexer, err := dbkit.NewBlindIndexer("contact_phone_index", cfg.NotificationIndexKey, dbkit.NormalizePhoneE164)
	if err != nil {
		t.Fatalf("dbkit.NewBlindIndexer(notification contact_phone_index): %v", err)
	}
	assertBlindIndexRoundTrip(t, contactPhoneIndexer, "+15550100")

	// PKILocalKeyCipherKey, AuthnBlindIndexKey and AuthnPIICipherKey,
	// together: boot the real composed server and drive a real
	// register-then-login round trip through it.
	handler, cleanup, _, err := buildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildServer with APP_ROOT_KEY alone: %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			t.Errorf("cleanup: %v", cleanupErr)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	token := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "root-key-proof")
	if token == "" {
		t.Fatal("registerAndAuthenticate returned an empty access token")
	}
}

// assertBlindIndexRoundTrip proves indexer genuinely functions as a blind
// index: Index(raw) is deterministic, and Equal(raw)'s returned condition
// carries exactly the value Index(raw) computed -- the write side and the
// query side agreeing is what makes "WHERE <column> = ?" actually find the
// row Index wrote, the real purpose a blind-index key exists to serve.
func assertBlindIndexRoundTrip(t *testing.T, indexer *dbkit.BlindIndexer, raw string) {
	t.Helper()

	indexed, err := indexer.Index(raw)
	if err != nil {
		t.Fatalf("Index(%q): %v", raw, err)
	}
	if indexed == "" {
		t.Fatalf("Index(%q) returned an empty index", raw)
	}

	cond, err := indexer.Equal(raw)
	if err != nil {
		t.Fatalf("Equal(%q): %v", raw, err)
	}
	if cond.Value != indexed {
		t.Fatalf("Equal(%q) condition value = %v, want it to match Index(%q) = %q", raw, cond.Value, raw, indexed)
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
	srv, cfg, _ := buildTestServer(t)

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
	handler, cleanup, _, err := buildServer(context.Background(), cfg)
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
	// tenant-acme's own audit trail is no longer notes' alone: since the
	// go/billing credit-audit round, seedDemoCredits' own boot-time Grant
	// (demo_credits.go) is itself a real CreditService.Grant call, which
	// now records its own "billing.credit.grant" AuditEvent for this same
	// tenant (credit_service.go's emitCreditAudit) -- see
	// billing_credit_flow_test.go's own audit tests for that surface's
	// dedicated proof. This test's own claim is narrower and unaffected:
	// exactly one "notes.note.create" event exists for the note this test
	// itself created, found among whatever else this tenant's audit trail
	// holds, rather than assuming the trail holds nothing else at all.
	noteEvents := auditEventsWithAction(events, "notes.note.create")
	if len(noteEvents) != 1 {
		t.Fatalf("notes.note.create audit events for tenant %q = %+v (all events = %+v), want exactly 1", tenantID, noteEvents, events)
	}

	got := noteEvents[0]
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
	// The row must be attributed to the creating user -- the value
	// createNoteAs sent as X-Demo-User-Id (demoNotesCreatorUserID), which
	// demoNotesSubjectResolver answers for notes' SubjectResolver seam and
	// recordNoteCreatedAudit now layers as the audit event's Actor (see its
	// doc comment in internal/notes/handler.go). audit.Emit copies the
	// Actor from ctx at emit time, and no middleware in the composed chain
	// populates that carrier, so an empty actor_type/actor_id here means
	// the audit trail cannot answer "who created this note".
	if actor := got.Actor(); actor.Type != pkgcore.ActorTypeUser || actor.ID != demoNotesCreatorUserID {
		t.Fatalf("AuditEvent.Actor() = %+v, want {Type: %q, ID: %q}",
			actor, pkgcore.ActorTypeUser, demoNotesCreatorUserID)
	}
	if got.OccurredAt.IsZero() {
		t.Fatal("AuditEvent.OccurredAt is zero, want a real timestamp")
	}

	// A negative control symmetric with this test's own positive
	// assertions: an unrelated tenant's read must see none of acme's
	// notes.note.create audit trail -- audit_events carries a real
	// tenant_id column precisely so this remains true even though the
	// table is platform data, not dbkit.TenantScoped (see go/dbkit/audit's
	// model.go doc comment). This is deliberately no longer "zero events
	// of any kind": tenant-globex is demo-seeded credits too
	// (demoHostTenants lists it alongside tenant-acme), so it carries its
	// own boot-time "billing.credit.grant" AuditEvent -- exactly the same
	// real, expected row this test's own tenant-acme assertion above now
	// tolerates -- the point being that NONE of it is acme's note.
	globexEvents, err := audit.NewRepository(auditDB).ListByTenant(context.Background(), "tenant-globex")
	if err != nil {
		t.Fatalf("ListByTenant(%q): %v", "tenant-globex", err)
	}
	if globexNoteEvents := auditEventsWithAction(globexEvents, "notes.note.create"); len(globexNoteEvents) != 0 {
		t.Fatalf("notes.note.create audit events for tenant %q = %+v, want none", "tenant-globex", globexNoteEvents)
	}
}

// auditEventsWithAction returns the subset of events whose Action is
// action, preserving order.
func auditEventsWithAction(events []audit.AuditEvent, action string) []audit.AuditEvent {
	var out []audit.AuditEvent
	for _, evt := range events {
		if evt.Action == action {
			out = append(out, evt)
		}
	}
	return out
}
