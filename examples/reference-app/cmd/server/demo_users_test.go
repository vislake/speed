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

// demo_users_test.go is the consumer proof of seedDemoUsers: when an
// operator sets SPEED_DEMO_USERS_PASSWORD, the boot registers the three demo
// accounts through authn's real register route, grants each its membership
// and rbac role under the user id authn assigned, and those grants are then
// reachable from a browser-shaped request -- a bearer token and no
// demoUserHeader at all -- because demoSubjectResolver falls back to the
// verified Principal. The second test pins the honest fail-closed half of
// the seed's idempotence story: a second boot against the same database
// finds the accounts already registered, skips them, and the sign-in that
// worked under boot one is refused, because memberships do not survive a
// restart.

// demoSeedPassword is what the tests below seed demo accounts with. It must
// satisfy go/authn's password policy (length-based) -- which is exactly the
// point: registration runs through the real register route, so a password
// the policy refused would fail the boot the same way it fails a browser.
const demoSeedPassword = "demo users seed passphrase"

// buildSeededUsersTestServer composes buildServer's real output the way
// buildTestServer does, with the demo-user seed switched on: the boot runs
// seedDemoUsers, which the plain testConfig's empty password never does.
// (public_config_test.go's own buildSeededTestServer seeds config values
// instead; the two names keep the two different seeds apart.)
func buildSeededUsersTestServer(t *testing.T, password string) (*httptest.Server, serverConfig) {
	t.Helper()

	cfg := testConfig(t)
	cfg.DemoUsersPassword = password
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

// demoLogin signs the account identified by email into tenant through
// authn's real login/password surface and returns the answer: the HTTP
// status, the error code (empty on a 200) and, on success, the bearer
// access token. Callers assert on exactly the combination they expect; a
// 200 that carries no access_token fails the test here, before any caller
// could mistake a token-less success for a sign-in.
func demoLogin(t *testing.T, srv *httptest.Server, email, password string, tenant pkgcore.TenantID) (statusCode int, code, accessToken string) {
	t.Helper()

	body, err := json.Marshal(map[string]string{
		"identifier": email,
		"password":   password,
		"tenant_id":  string(tenant),
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
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatalf("login %s: decoding %s: %v", email, raw, err)
	}
	if resp.StatusCode == http.StatusOK && answer.AccessToken == "" {
		t.Fatalf("login %s: status 200 but no access_token; body = %s", email, raw)
	}
	return resp.StatusCode, answer.Code, answer.AccessToken
}

// TestDemoUsers_SeededAccountsReachTheGateThroughTheirPrincipal signs the
// seeded accounts in and drives the notes gate with NO demo header at all:
// the tenant comes from the access token's claim, the acting user from the
// verified Principal demoSubjectResolver falls back to, and the decision
// falls against the grants the seed attached to the user id authn assigned
// at registration. Each account stands in for one property: the owner for
// full access, the reader for the gate closing on a real, correctly
// identified user, and the acme-only account for a grant being a fact about
// a (tenant, user) pair -- signed in for a tenant it holds no membership
// in, it is refused before any route exists.
func TestDemoUsers_SeededAccountsReachTheGateThroughTheirPrincipal(t *testing.T) {
	srv, _ := buildSeededUsersTestServer(t, demoSeedPassword)

	// The seeded owner may write and read.
	status, code, ownerToken := demoLogin(t, srv, demoOwnerEmail, demoSeedPassword, "tenant-acme")
	if status != http.StatusOK {
		t.Fatalf("login as the seeded owner: status = %d, code = %q, want %d", status, code, http.StatusOK)
	}
	resp := notesRequestAs(t, srv, http.MethodPost, ownerToken, "", strings.NewReader(`{"text":"seeded owner note"}`))
	func() {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("POST as the seeded owner: status = %d, want %d; body = %s", resp.StatusCode, http.StatusCreated, body)
		}
	}()
	resp = notesRequestAs(t, srv, http.MethodGet, ownerToken, "", nil)
	func() {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("GET as the seeded owner: status = %d, want %d; body = %s", resp.StatusCode, http.StatusOK, body)
		}
	}()

	// The seeded reader may list notes...
	status, code, readerToken := demoLogin(t, srv, demoReaderEmail, demoSeedPassword, "tenant-acme")
	if status != http.StatusOK {
		t.Fatalf("login as the seeded reader: status = %d, code = %q, want %d", status, code, http.StatusOK)
	}
	resp = notesRequestAs(t, srv, http.MethodGet, readerToken, "", nil)
	func() {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("GET as the seeded reader: status = %d, want %d; body = %s", resp.StatusCode, http.StatusOK, body)
		}
	}()

	// ...and may not create one: the same gate, decided against the
	// notes:read-only grant the seed made under the reader's real user id.
	assertPermissionDenied(t,
		notesRequestAs(t, srv, http.MethodPost, readerToken, "", strings.NewReader(`{"text":"reader note"}`)),
		"POST as the seeded reader")

	// The acme-only account holds its membership and reader grant in
	// tenant-acme only; signing in for the tenant it has no membership in is
	// refused with authn's membership-required code (authn.tenant_
	// membership_required: the caller asked for a tenant it is not an active
	// member of -- the unavailable code, by contrast, is for a membership
	// question nobody could answer at all), before any route exists.
	status, code, _ = demoLogin(t, srv, demoAcmeOnlyEmail, demoSeedPassword, "tenant-globex")
	if status != http.StatusForbidden || code != "authn.tenant_membership_required" {
		t.Fatalf("login as the acme-only account in tenant-globex: status = %d, code = %q, want 403 %q",
			status, code, "authn.tenant_membership_required")
	}
}

// TestDemoUsers_SecondBootAgainstTheSameDatabaseFailsClosed pins what a
// process restart against the same database actually does to the demo-user
// seed. Boot one registers every account and signs in fine; boot two, with
// the seed switched on again and a fresh, empty demoMemberships (the honest
// image of a restart), finds the registrations already in authn's users
// table and skips them -- and because the memberships lived in the
// in-process store boot one owned, the account's sign-in is now refused
// with authn's membership code even though the password is exactly right.
// That is the fail-closed half of seedDemoUsers' contract: a skip is never
// dressed up as a seed.
func TestDemoUsers_SecondBootAgainstTheSameDatabaseFailsClosed(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reference-app-seed-restart.db")

	// boot composes a server against the shared dbPath with the given
	// membership store, without any test cleanup: the caller closes and
	// cleans up each boot explicitly, in order.
	boot := func(memberships *demoMemberships) (*httptest.Server, func() error) {
		cfg := testConfig(t)
		cfg.SQLitePath = dbPath
		cfg.Memberships = memberships
		cfg.DemoUsersPassword = demoSeedPassword
		handler, cleanup, err := buildServer(context.Background(), cfg)
		if err != nil {
			t.Fatalf("buildServer: %v", err)
		}
		return httptest.NewServer(handler), cleanup
	}

	// Boot one seeds every demo account and proves the seed works.
	srv1, cleanup1 := boot(newDemoMemberships())
	status, code, ownerToken := demoLogin(t, srv1, demoOwnerEmail, demoSeedPassword, "tenant-acme")
	if status != http.StatusOK {
		t.Fatalf("boot-one login as the seeded owner: status = %d, code = %q, want %d", status, code, http.StatusOK)
	}
	resp := notesRequestAs(t, srv1, http.MethodGet, ownerToken, "", nil)
	func() {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("boot-one GET as the seeded owner: status = %d, want %d; body = %s", resp.StatusCode, http.StatusOK, body)
		}
	}()
	// Shut boot one down completely -- its cleanup also releases the
	// database file -- before booting again against the same path.
	srv1.Close()
	if err := cleanup1(); err != nil {
		t.Fatalf("boot-one cleanup: %v", err)
	}

	// Boot two re-runs the seed against the same database. Every account is
	// already registered, so the seed skips them (warn + fail closed); the
	// right password no longer signs the owner in, because membership died
	// with boot one -- the sign-in now asks for a tenant the account is not
	// an active member of, and authn answers with the membership-required
	// code (the reader that answers is present and working; the answer it
	// gives is no).
	srv2, cleanup2 := boot(newDemoMemberships())
	defer func() {
		srv2.Close()
		if err := cleanup2(); err != nil {
			t.Errorf("boot-two cleanup: %v", err)
		}
	}()

	status, code, _ = demoLogin(t, srv2, demoOwnerEmail, demoSeedPassword, "tenant-acme")
	if status != http.StatusForbidden || code != "authn.tenant_membership_required" {
		t.Fatalf("boot-two login as the seeded owner: status = %d, code = %q, want 403 %q (memberships do not survive a restart)",
			status, code, "authn.tenant_membership_required")
	}
}

// TestDemoUsers_RegisteredButMemberless_BrowserShapedSignInRefused pins
// the refusal the web demo rig answers for its registered-but-unseeded
// account: an account the register route just created -- the same route
// the app's register form drives -- holds no membership anywhere
// (registration alone grants none), and the browser-shaped sign-in, a
// body naming an identifier and a password with no tenant_id field at
// all, answers 403 authn.tenant_membership_required. The named-tenant
// refusals the seed tests pin leave this shape open: they prove an
// account without a membership in the tenant it asked for is refused,
// while this one proves the no-tenant form of the same refusal -- the
// tenant a sign-in may act in is derived from the account's own
// memberships, never from a field the caller typed, so an account with
// no membership in any tenant is refused for every tenant at once.
func TestDemoUsers_RegisteredButMemberless_BrowserShapedSignInRefused(t *testing.T) {
	srv, _ := buildTestServer(t)

	// Register a fresh account through authn's real register route. The
	// answer is 201, and a membership nowhere.
	resp, err := srv.Client().Post(srv.URL+"/api/v1/authn/register", "application/json",
		strings.NewReader(`{"email":"fresh@example.test","password":"fresh memberless passphrase"}`))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	func() {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("register: status = %d, want %d; body = %s", resp.StatusCode, http.StatusCreated, body)
		}
	}()

	// The browser-shaped sign-in that follows: identifier and password
	// only. With no tenant_id the membership question is "which tenant
	// may this account act in", answered from the account's memberships;
	// the reader is present and answers none, so this is the
	// required-code refusal, never the unavailable one.
	body, err := json.Marshal(map[string]string{
		"identifier": "fresh@example.test",
		"password":   "fresh memberless passphrase",
	})
	if err != nil {
		t.Fatalf("login: marshal body: %v", err)
	}
	loginResp, err := srv.Client().Post(srv.URL+"/api/v1/authn/login/password", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer loginResp.Body.Close()
	raw, err := io.ReadAll(loginResp.Body)
	if err != nil {
		t.Fatalf("login: read body: %v", err)
	}
	var answer struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatalf("login: decoding %s: %v", raw, err)
	}
	if loginResp.StatusCode != http.StatusForbidden || answer.Code != "authn.tenant_membership_required" {
		t.Fatalf("browser-shaped sign-in of the memberless account: status = %d, code = %q, want 403 %q",
			loginResp.StatusCode, answer.Code, "authn.tenant_membership_required")
	}
}
