package main

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

	"github.com/vislake/speed/go/pkgcore"
)

// self_service_test.go is the regression suite for the acceptance finding
// this round closes: a self-registered account used to hit a dead end --
// registration answered 201, the browser-shaped sign-in that followed
// answered 403 authn.tenant_membership_required ("the account has no
// organization yet"), and nothing the product offered could change that
// state. Under the product decision this host now implements, the
// registration itself provisions the account's clinic (self_service.go),
// so the same journey must now land the account inside its own clinic
// tenant with the org root, the membership and the owner grant it can act
// on -- and that must survive the process restart that kills every
// in-memory answer (the org rows and the durable clinic ledger answer on
// their own, the same two-boot shape the demo-users and invitation suites
// already pin for their own memberships).
//
// Failing before the fix: on the pre-self-service code this suite's first
// test answered the browser-shaped sign-in with the 403 the deleted
// TestDemoUsers_RegisteredButMemberless_BrowserShapedSignInRefused used
// to pin, and the assertions below failed where it passed; the second
// test failed one step earlier (boot one's sign-in never succeeded).

// selfServiceFreshEmail is the account every journey below registers. The
// @example.com suffix matches the flow tests' convention; the local part
// is unique to this suite so a fresh database never collides with
// anything.
const selfServiceFreshEmail = "self-service-founder@example.com"

// selfServicePassword is what the journey registers and signs in with. It
// satisfies go/authn's password policy (length-based), like every
// password in this package's flows.
const selfServicePassword = "a fresh clinic passphrase"

// browserSignIn signs email in through authn's real login/password
// surface with the BROWSER shape -- a body naming the identifier and the
// password and no tenant_id field at all, the exact shape the web host's
// sign-in form sends. It returns the HTTP status, the error code (empty
// on a 200), and, on success, the access token and the tenant the answer
// landed the principal in. A 200 that carries no access_token fails the
// test here, before any caller could mistake a token-less success for a
// sign-in.
func browserSignIn(t *testing.T, srv *httptest.Server, email, password string) (statusCode int, code, accessToken string, tenant pkgcore.TenantID) {
	t.Helper()

	body, err := json.Marshal(map[string]string{
		"identifier": email,
		"password":   password,
	})
	if err != nil {
		t.Fatalf("login %s: marshal body: %v", email, err)
	}
	resp, err := srv.Client().Post(srv.URL+"/api/v1/authn/login/password", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login %s: %v", email, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("login %s: read body: %v", email, err)
	}
	var answer struct {
		AccessToken string `json:"access_token"`
		Code        string `json:"code"`
		Principal   struct {
			TenantID string `json:"tenant_id"`
		} `json:"principal"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatalf("login %s: decoding %s: %v", email, raw, err)
	}
	if resp.StatusCode == http.StatusOK && answer.AccessToken == "" {
		t.Fatalf("login %s: status 200 but no access_token; body = %s", email, raw)
	}
	return resp.StatusCode, answer.Code, answer.AccessToken, pkgcore.TenantID(answer.Principal.TenantID)
}

// registerFreshAccount registers email through authn's real register route
// and returns the user id authn assigned, failing the test on anything
// but a 201.
func registerFreshAccount(t *testing.T, srv *httptest.Server, email, password string) (userID string) {
	t.Helper()

	body, err := json.Marshal(map[string]string{"email": email, "password": password})
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

// TestSelfServiceSignup_RegisterThenSignIn_LandsInTheCreatedClinic drives
// the whole acceptance journey through the real composed HTTP stack: a
// fresh account registers, the browser-shaped sign-in that follows lands
// it inside its OWN clinic tenant (the deterministic clinicTenantOf
// derivation -- never one of the configured demo tenants), the clinic's
// org tree answers the account's bearer token with the root node
// registration provisioned, and the notes gate answers the owner grant
// with a real write. The named-tenant control keeps the journey honest
// about what the clinic is NOT: the account holds no membership in any
// configured tenant, so a sign-in asking for tenant-acme is refused
// exactly as before, with authn.tenant_membership_required.
func TestSelfServiceSignup_RegisterThenSignIn_LandsInTheCreatedClinic(t *testing.T) {
	srv, _, _ := buildTestServer(t)

	// A fresh account registers through authn's real register route.
	userID := registerFreshAccount(t, srv, selfServiceFreshEmail, selfServicePassword)

	// The browser-shaped sign-in that used to answer the dead end now
	// succeeds, and lands the principal in the account's OWN clinic -- the
	// deterministic tenant derived from the registrant's user id
	// (self_service.go's clinicTenantOf), never a configured demo tenant.
	// The derivation is spelled out here rather than reached through the
	// production helper so this suite keeps compiling (and failing with a
	// clean assertion) against the pre-self-service code this regression
	// is measured on.
	status, code, token, tenant := browserSignIn(t, srv, selfServiceFreshEmail, selfServicePassword)
	if status != http.StatusOK {
		t.Fatalf("browser-shaped sign-in of the freshly registered account: status = %d, code = %q, want %d "+
			"(registration must provision the clinic its account can sign into)",
			status, code, http.StatusOK)
	}
	if want := pkgcore.TenantID("tenant-" + userID); tenant != want {
		t.Fatalf("browser-shaped sign-in landed the principal in tenant %q, want its own clinic %q", tenant, want)
	}
	for _, demo := range []pkgcore.TenantID{"tenant-acme", "tenant-globex"} {
		if tenant == demo {
			t.Fatalf("browser-shaped sign-in landed the principal in the configured demo tenant %q, want its own clinic", demo)
		}
	}

	// The control: the account holds no membership in any configured
	// tenant, so a sign-in naming one is refused exactly as a
	// pre-acceptance invitee's is -- the clinic is its own tenant, not a
	// back door into someone else's.
	status, code, _ = demoLogin(t, srv, selfServiceFreshEmail, selfServicePassword, "tenant-acme")
	if status != http.StatusForbidden || code != "authn.tenant_membership_required" {
		t.Fatalf("sign-in of the clinic owner into tenant-acme: status = %d, code = %q, want 403 %q",
			status, code, "authn.tenant_membership_required")
	}

	// The clinic's org tree answers the account's own bearer token: the
	// registration provisioned exactly one root node (org's root of the
	// clinic tenant). No demo header rides along -- the tenant comes from
	// the access token and the acting subject from the verified
	// Principal, the browser's own shape.
	resp, err := srv.Client().Do(func() *http.Request {
		req, reqErr := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/org/nodes", nil)
		if reqErr != nil {
			t.Fatalf("build org nodes request: %v", reqErr)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return req
	}())
	if err != nil {
		t.Fatalf("GET /api/v1/org/nodes: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /api/v1/org/nodes as the clinic owner: status = %d, want %d; body = %s",
			resp.StatusCode, http.StatusOK, raw)
	}
	var tree struct {
		Nodes []struct {
			ID       string `json:"id"`
			ParentID string `json:"parentId"`
		} `json:"nodes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tree); err != nil {
		t.Fatalf("decode org nodes response: %v", err)
	}
	if len(tree.Nodes) != 1 || tree.Nodes[0].ParentID != "" || tree.Nodes[0].ID == "" {
		t.Fatalf("clinic org tree = %+v, want exactly one root node", tree.Nodes)
	}

	// The owner grant answers on the notes surface: a create succeeds and
	// the list reads the row back -- under the clinic's own tenant, never
	// the demo tenants.
	noteResp := notesRequestAs(t, srv, http.MethodPost, token, "", strings.NewReader(`{"text":"the clinic owner's first note"}`))
	func() {
		defer noteResp.Body.Close()
		if noteResp.StatusCode != http.StatusCreated {
			raw, _ := io.ReadAll(noteResp.Body)
			t.Fatalf("POST /api/v1/notes as the clinic owner: status = %d, want %d; body = %s",
				noteResp.StatusCode, http.StatusCreated, raw)
		}
	}()
	listResp := notesRequestAs(t, srv, http.MethodGet, token, "", nil)
	func() {
		defer listResp.Body.Close()
		if listResp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(listResp.Body)
			t.Fatalf("GET /api/v1/notes as the clinic owner: status = %d, want %d; body = %s",
				listResp.StatusCode, http.StatusOK, raw)
		}
		var listed testListNotesResponse
		if err := json.NewDecoder(listResp.Body).Decode(&listed); err != nil {
			t.Fatalf("decode notes list response: %v", err)
		}
		if len(listed.Notes) != 1 || listed.Notes[0].Text != "the clinic owner's first note" {
			t.Fatalf("clinic notes list = %+v, want the note the owner just created", listed.Notes)
		}
	}()
}

// TestSelfServiceSignup_ClinicOwnerSignInSurvivesARestart is the
// restart half of the journey: boot one registers an account and proves
// the browser-shaped sign-in lands in its clinic; the server shuts down
// completely, and boot two against the same database proves the same
// sign-in still lands there. The clinic's membership is an org row and
// the clinic tenant itself is re-discovered from the durable
// self_service_clinics ledger (wireSelfService), so nothing boot one held
// in memory may be load-bearing -- the same two-boot shape the
// demo-users and invitation suites use for their own memberships.
func TestSelfServiceSignup_ClinicOwnerSignInSurvivesARestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reference-app-self-service-restart.db")

	// boot composes a server against the shared dbPath without any test
	// cleanup: the caller closes and cleans up each boot explicitly, in
	// order.
	boot := func() (*httptest.Server, func() error) {
		cfg := testConfig(t)
		cfg.SQLitePath = dbPath
		handler, cleanup, _, err := buildServer(context.Background(), cfg)
		if err != nil {
			t.Fatalf("buildServer: %v", err)
		}
		return httptest.NewServer(handler), cleanup
	}

	// Boot one: register and prove the clinic sign-in works.
	srv1, cleanup1 := boot()
	registerFreshAccount(t, srv1, selfServiceFreshEmail, selfServicePassword)
	status, code, _, tenant := browserSignIn(t, srv1, selfServiceFreshEmail, selfServicePassword)
	if status != http.StatusOK {
		t.Fatalf("boot-one sign-in of the freshly registered account: status = %d, code = %q, want %d",
			status, code, http.StatusOK)
	}
	clinic := tenant
	srv1.Close()
	if err := cleanup1(); err != nil {
		t.Fatalf("boot-one cleanup: %v", err)
	}

	// Boot two: the same browser-shaped sign-in must land in the same
	// clinic -- the org membership row survives, and the ledger row tells
	// this boot's sign-in store where to look for it.
	srv2, cleanup2 := boot()
	defer func() {
		srv2.Close()
		if err := cleanup2(); err != nil {
			t.Errorf("boot-two cleanup: %v", err)
		}
	}()
	status, code, _, tenant = browserSignIn(t, srv2, selfServiceFreshEmail, selfServicePassword)
	if status != http.StatusOK {
		t.Fatalf("boot-two sign-in of the clinic owner: status = %d, code = %q, want %d "+
			"(the clinic's org membership and ledger row must survive a restart)",
			status, code, http.StatusOK)
	}
	if tenant != clinic {
		t.Fatalf("boot-two sign-in landed the principal in tenant %q, want the boot-one clinic %q", tenant, clinic)
	}
}
