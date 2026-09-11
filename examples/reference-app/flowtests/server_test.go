package flowtests

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/app"
	"github.com/vislake/speed/examples/reference-app/internal/app/demo"

	"github.com/vislake/speed/go/compliance"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
)

// testPassword is the demo password every account registerAndAuthenticate
// creates uses. It exists as a named constant, not a repeated literal,
// purely so every test in this file that registers a demo account agrees on
// what "a perfectly fine passphrase" means -- go/authn's own password
// policy default (go/authn/password.go) accepts it.
const testPassword = "a perfectly fine passphrase"

// DemoNotesCreatorUserID is declared in internal/app/demo/demo_subject.go, next to the other
// demo identity constants, because the running server's own glue reads it
// too (internal/app/demo/demo_notification.go's demo address table keys on it); the test
// helpers here and in notification_flow_test.go reference the same constant
// so a test's X-Demo-User-Id header always names the user the server's
// subscription will dispatch to. See internal/app/demo/demo_subject.go's comment there for
// what the id means.

// testConfig returns a ServerConfig backed by a fresh, per-test temp-file
// SQLite database, so tests never share state and never touch a real file
// outside t.TempDir(). Memberships is always a fresh, empty
// signInMemberships -- tests that need an account to actually reach a
// tenant grant it explicitly via registerAndAuthenticate below, keeping
// the same reference BuildServer itself wires (and attaches to org) so a
// test's grant is visible to the running server.
//
// The six platform key materials need no field here: every boot resolves them
// from the declaring components' declarations on the loader chain, with
// app.BootstrapDevDefaults as the documented development defaults -- the
// engine's platform cipher, the org, notification and authn blind indexers,
// and the authn PII and pki local-key ciphers are all built from that
// material, and a missing one fails the boot before the first request. The
// environment resolution (each key's own variable, or APP_ROOT_KEY's
// derivation) runs on the same chain, on ConfigFromEnv's path and the
// assembly's alike.
func testConfig(t *testing.T) app.ServerConfig {
	t.Helper()
	return app.ServerConfig{
		DeploymentMode: pkgcore.DeploymentModeStandalone,
		Port:           "0",
		SQLitePath:     filepath.Join(t.TempDir(), "reference-app-test.db"),
		HostTenants:    demo.DemoHostTenants,
		Memberships:    app.NewSignInMemberships(),
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
// the retention/erasure/export services, which compliance_flow_test.go
// drives (every other flow test in this package is HTTP-driven and discards
// it, exactly as BuildServer's own doc comment describes main.go doing).
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
//   - X-Demo-User names WHO is acting for the rbac gate (internal/app/demo/demo_subject.go's
//     DemoUserHeader). DemoOwnerUserID holds every permission, so these two
//     helpers exercise the happy path; the tests that exercise the gate
//     itself send other users, or none.
//   - X-Demo-User-Id names the creating user for notes' own SubjectResolver
//     (DemoNotesSubjectResolver in internal/app/server.go) -- the value that lands in the
//     note's CreatorUserID; see DemoNotesCreatorUserID above.
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
	req.Header.Set(demo.DemoUserHeader, demo.DemoOwnerUserID)
	req.Header.Set(demo.DemoOrgUserHeader, demo.DemoNotesCreatorUserID)

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
	req.Header.Set(demo.DemoUserHeader, demo.DemoOwnerUserID)

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
		req.Header.Set(demo.DemoUserHeader, user)
		req.Header.Set(demo.DemoOrgUserHeader, demo.DemoNotesCreatorUserID)
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
	// against (internal/app/demo/demo_subject.go's SeedDemoGrants).
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "pg-owner")

	// The owner may write.
	resp := notesRequestAs(t, srv, http.MethodPost, acmeToken, demo.DemoOwnerUserID,
		strings.NewReader(`{"text":"owner note"}`))
	func() {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("POST as %s: status = %d, want %d; body = %s", demo.DemoOwnerUserID, resp.StatusCode, http.StatusCreated, body)
		}
	}()

	// The reader may list...
	resp = notesRequestAs(t, srv, http.MethodGet, acmeToken, demo.DemoReaderUserID, nil)
	func() {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("GET as %s: status = %d, want %d; body = %s", demo.DemoReaderUserID, resp.StatusCode, http.StatusOK, body)
		}
	}()

	// ...and may not create. This is the whole point of the gate.
	assertPermissionDenied(t,
		notesRequestAs(t, srv, http.MethodPost, acmeToken, demo.DemoReaderUserID, strings.NewReader(`{"text":"reader note"}`)),
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
// sharp case is DemoSingleTenantUserID, which is granted in tenant-acme
// and nowhere else: the identical user id, acting through a token that
// signs into tenant-globex, must be refused. The tenant the decision is
// made in comes from the bearer token, never from anything the caller
// sent -- the header only names WHO is acting.
func TestBuildServer_PermissionGate_GrantsDoNotCrossTenants(t *testing.T) {
	srv, cfg, _ := buildTestServer(t)
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "pg-acme")
	globexToken := registerAndAuthenticate(t, srv, cfg, "tenant-globex", "pg-globex")

	resp := notesRequestAs(t, srv, http.MethodGet, acmeToken, demo.DemoSingleTenantUserID, nil)
	func() {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("GET in the tenant that granted %s: status = %d, want %d; body = %s",
				demo.DemoSingleTenantUserID, resp.StatusCode, http.StatusOK, body)
		}
	}()

	assertPermissionDenied(t,
		notesRequestAs(t, srv, http.MethodGet, globexToken, demo.DemoSingleTenantUserID, nil),
		"GET as the same user id in the tenant that never granted it")

	// And the refusal is genuinely about the tenant rather than the user
	// being unknown: the SAME tenant grants the same role to demo-reader.
	resp = notesRequestAs(t, srv, http.MethodGet, globexToken, demo.DemoReaderUserID, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET as %s in tenant-globex: status = %d, want %d; body = %s",
			demo.DemoReaderUserID, resp.StatusCode, http.StatusOK, body)
	}
}

// A no-Docker "positive" counterpart to the three tests above -- one that
// composes every seam onto a fake, unreachable address and asserts
// BuildServer succeeds -- was deliberately NOT added here, and this is a
// real gap, not a silent one: this app's own SeedDemoGrants
// (internal/app/demo/demo_subject.go), which every BuildServer call runs unconditionally
// after Bootstrap to seed the demo tenants' built-in roles, makes a
// SYNCHRONOUS rbac.Service call that publishes on whatever EventBus
// Bootstrap resolved -- eventbus/redis.EventBus.Publish genuinely appends
// to the Redis stream inline, unlike Subscribe's fire-and-forget background
// reader. Pointing cfg.RedisAddr at a fake address therefore fails this
// seeding step with a real "connection refused" the moment Redis is
// unreachable, regardless of deployment mode. So the positive half of
// this property
// (assembly succeeds when every seam is genuinely satisfied) cannot be
// proven at the unit tier without either standing up real infrastructure
// (which belongs in a Docker-backed integration tier, not here) or
// special-casing SeedDemoGrants for tests (which would test a different
// wiring than main() runs, the exact anti-pattern BuildServer's own doc
// comment warns against). The genuine, real-infrastructure proof is
// examples/reference-app/integration_test/distributed_mode_test.go.

// notesRequest issues method against /api/v1/notes with the given bearer
// token (empty means no Authorization header at all) and optional body,
// returning the raw response for the caller to assert on -- unlike
// createNoteAs/listNotesAs above, which assert success internally, this is
// mostly for tests that expect the request to be rejected.
//
// It always carries X-Demo-User-Id too (DemoNotesCreatorUserID): one POST
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
	req.Header.Set(demo.DemoUserHeader, demo.DemoOwnerUserID)
	req.Header.Set(demo.DemoOrgUserHeader, demo.DemoNotesCreatorUserID)
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
// counterpart of the Host-based isolation regression: an unrecognized
// Host once resolved to a shared demoDefaultTenant
// bucket any anonymous caller could read from and write to. That bucket no
// longer exists at all -- Host plays no part in the notes API's tenant
// resolution any more (internal/app/server.go's middleware-chain doc comment) -- but
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
	// back, which is exactly what made the unisolated bucket a real leak
	// rather than a per-caller-isolated refusal.
	getResp2 := notesRequest(t, srv, http.MethodGet, "", nil)
	defer getResp2.Body.Close()
	if getResp2.StatusCode != http.StatusForbidden {
		respBody, _ := io.ReadAll(getResp2.Body)
		t.Fatalf("GET /api/v1/notes (second unauthenticated caller) status = %d, want %d; body = %s",
			getResp2.StatusCode, http.StatusForbidden, respBody)
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
	req.Header.Set(demo.DemoUserHeader, demo.DemoOwnerUserID)
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
	req2.Header.Set(demo.DemoUserHeader, demo.DemoOwnerUserID)
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

// TestBuildServer_NoteCreate_PersistsAuditEvent is the proof that
// examples/reference-app -- the mandatory first consumer of
// every module -- is a real consumer of go/dbkit/audit, not merely a
// package that compiles against it. It drives a real POST /api/v1/notes
// request through the full composed stack (tenancy.Middleware,
// notes.Handler, a real SQLite database) exactly as
// TestBuildServer_MultiTenantIsolation_EndToEnd above does, then reads the
// audit_events table back through a second dbkit.Open connection to the
// same SQLite file -- the identical "BuildServer hands out neither its
// *gorm.DB nor a module's own service, so a second connection is the only
// reach a test has into storage" pattern config_public_endpoint_gates_test.go's
// buildSeededTestServer/seedConfigRows already use for the config
// module's own table -- and asserts on a real audit.Repository.ListByTenant
// result, not a mock or an in-memory event assertion (handler_test.go's
// TestHandler_Create_ValidText_RecordsAuditEvent already covers that
// narrower unit-level claim).
func TestBuildServer_NoteCreate_PersistsAuditEvent(t *testing.T) {
	cfg := testConfig(t)
	handler, cleanup, _, err := app.BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
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
	// tenant-acme's own audit trail is not notes' alone: SeedDemoCredits'
	// own boot-time Grant
	// (internal/app/demo/demo_credits.go) is itself a real CreditService.Grant call, which
	// records its own "billing.credit.grant" AuditEvent for this same
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
	// createNoteAs sent as X-Demo-User-Id (DemoNotesCreatorUserID), which
	// DemoNotesSubjectResolver answers for notes' SubjectResolver seam and
	// recordNoteCreatedAudit now layers as the audit event's Actor (see its
	// doc comment in internal/notes/handler.go). audit.Emit copies the
	// Actor from ctx at emit time, and no middleware in the composed chain
	// populates that carrier, so an empty actor_type/actor_id here means
	// the audit trail cannot answer "who created this note".
	if actor := got.Actor(); actor.Type != pkgcore.ActorTypeUser || actor.ID != demo.DemoNotesCreatorUserID {
		t.Fatalf("AuditEvent.Actor() = %+v, want {Type: %q, ID: %q}",
			actor, pkgcore.ActorTypeUser, demo.DemoNotesCreatorUserID)
	}
	if got.OccurredAt.IsZero() {
		t.Fatal("AuditEvent.OccurredAt is zero, want a real timestamp")
	}

	// A negative control symmetric with this test's own positive
	// assertions: an unrelated tenant's read must see none of acme's
	// notes.note.create audit trail -- audit_events carries a real
	// tenant_id column precisely so this remains true even though the
	// table is platform data, not dbkit.TenantScoped (see go/dbkit/audit's
	// model.go doc comment). The control asserts on notes.note.create
	// events specifically, not "zero events of any kind": tenant-globex
	// is demo-seeded credits too (DemoHostTenants lists it alongside
	// tenant-acme), so it carries its own boot-time
	// "billing.credit.grant" AuditEvent -- exactly the same real,
	// expected row this test's own tenant-acme assertion above tolerates
	// -- the point being that NONE of it is acme's note.
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
