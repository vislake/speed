package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/app"
	"github.com/vislake/speed/examples/reference-app/internal/app/demo"

	speedapp "github.com/vislake/speed/go/app"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/authn/demoseed"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
)

// demo_users_test.go is the consumer proof of SeedDemoUsers: when an
// operator sets APP_DEMO_USERS_PASSWORD, the boot registers the three demo
// accounts through authn's real register route, grants each its membership
// and rbac role under the user id authn assigned, and those grants are then
// reachable from a browser-shaped request -- a bearer token and no
// DemoUserHeader at all -- because DemoSubjectResolver falls back to the
// verified Principal. The second test pins the restart half of the seed's
// idempotence story: the memberships live in org's own memberships table
// (authn's sign-in path reads them through internal/app/sign_in_memberships.go), so a
// second boot against the same database finds the accounts already
// registered, re-asserts their grants under the ids authn assigned the
// first time, and every sign-in that worked under boot one -- the demo
// customer accounts AND the platform-staff account's SystemDomain
// membership -- still works under boot two. The sign-ins survive because
// memberships live in durable org rows and the re-asserted SystemDomain
// grant, never in an in-process roster that dies with the boot that
// granted them.

// demoSeedPassword is what the tests below seed demo accounts with. It must
// satisfy go/authn's password policy (length-based) -- which is exactly the
// point: registration runs through the real register route, so a password
// the policy refused would fail the boot the same way it fails a browser.
const demoSeedPassword = "demo users seed passphrase"

// demoPlatformStaffSeedPassword is the test passphrase the suites that seed
// the demo platform-staff account (internal/app/demo/demo_admin.go's SeedDemoPlatformStaff)
// set its OWN config field to -- deliberately a DIFFERENT value from
// demoSeedPassword, mirroring the runtime split between
// APP_DEMO_USERS_PASSWORD and APP_DEMO_PLATFORM_STAFF_PASSWORD
// (internal/app/demo/demo_admin.go). It must satisfy go/authn's password policy for the same
// registration-through-the-real-route reason demoSeedPassword documents.
const demoPlatformStaffSeedPassword = "platform staff seed passphrase"

// demoSeedDomain is the email domain the demo accounts are declared on
// (internal/app/demo/demo_users.go's own demoSeedDomain, unexported), restated
// here for the seeder this file drives directly.
const demoSeedDomain = "example.com"

// buildSeededUsersTestServer composes BuildServer's real output the way
// buildTestServer does, with the demo-user seed switched on: the boot runs
// SeedDemoUsers, which the plain testConfig's empty password never does.
// (flowtests/config_public_endpoint_gates_test.go's own buildSeededTestServer seeds config values
// instead; the two names keep the two different seeds apart.)
func buildSeededUsersTestServer(t *testing.T, password string) (*httptest.Server, app.ServerConfig) {
	t.Helper()

	cfg := testConfig(t)
	cfg.DemoUsersPassword = password
	handler, cleanup, _, err := app.BuildServer(context.Background(), cfg)
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

// assertNoMembershipRefusal reads the account accessToken authenticates
// its login history through authn's real login-history endpoint and
// asserts that the account's newest FAILED attempt was recorded with
// FailureReasonNoMembership. The unified-401 controls call this right
// after the refusal they pin: that 401 is byte-identical to a wrong
// password's, so a control asserting only the status could not tell "the
// password verified and the account holds no membership in the asked-for
// tenant" from "the test's own credentials broke" -- a defect regression
// answering 401 for another reason would leave the control green. The
// login history is the one place the real reason survives: the login
// response itself never names it, and the history endpoint exists
// exactly to read it back (a wrong password records
// FailureReasonBadPassword, an unknown account FailureReasonUnknownUser,
// and only a membership-less refusal of a verified credential records
// FailureReasonNoMembership).
func assertNoMembershipRefusal(t *testing.T, srv *httptest.Server, accessToken, what string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/authn/login-history", nil)
	if err != nil {
		t.Fatalf("%s: build login-history request: %v", what, err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s: read login history: %v", what, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: login history status = %d, want %d; body = %s", what, resp.StatusCode, http.StatusOK, raw)
	}
	var history struct {
		Attempts []struct {
			Result        string `json:"result"`
			FailureReason string `json:"failure_reason"`
		} `json:"attempts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&history); err != nil {
		t.Fatalf("%s: decode login history: %v", what, err)
	}
	const wantReason = authn.FailureReasonNoMembership
	for _, attempt := range history.Attempts {
		if attempt.Result != authn.LoginResultFailure {
			continue
		}
		if attempt.FailureReason != string(wantReason) {
			t.Fatalf("%s: the refusal's history row records %q, want %q -- the 401 was not the no-membership "+
				"answer (a wrong password would record %q)",
				what, attempt.FailureReason, wantReason, authn.FailureReasonBadPassword)
		}
		return
	}
	t.Fatalf("%s: the account's login history holds no failed attempt; attempts = %+v", what, history.Attempts)
}

// TestDemoUsers_SeededAccountsReachTheGateThroughTheirPrincipal signs the
// seeded accounts in and drives the notes gate with NO demo header at all:
// the tenant comes from the access token's claim, the acting user from the
// verified Principal DemoSubjectResolver falls back to, and the decision
// falls against the grants the seed attached to the user id authn assigned
// at registration. Each account stands in for one property: the owner for
// full access, the reader for the gate closing on a real, correctly
// identified user, and the acme-only account for a grant being a fact about
// a (tenant, user) pair -- signed in for a tenant it holds no membership
// in, it is refused before any route exists.
func TestDemoUsers_SeededAccountsReachTheGateThroughTheirPrincipal(t *testing.T) {
	srv, _ := buildSeededUsersTestServer(t, demoSeedPassword)

	// The seeded owner may write and read.
	status, code, ownerToken := demoLogin(t, srv, demo.DemoOwnerEmail, demoSeedPassword, "tenant-acme")
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
	status, code, readerToken := demoLogin(t, srv, demo.DemoReaderEmail, demoSeedPassword, "tenant-acme")
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

	// The acme-only account signs into its own tenant fine -- its
	// membership and reader grant live in tenant-acme alone -- which both
	// proves the credentials the globex control refuses below are right
	// and yields the bearer the login-history read after the control needs
	// (history is the one place a refusal's real reason survives).
	status, code, acmeOnlyToken := demoLogin(t, srv, demo.DemoAcmeOnlyEmail, demoSeedPassword, "tenant-acme")
	if status != http.StatusOK {
		t.Fatalf("login as the seeded acme-only account in its own tenant: status = %d, code = %q, want %d",
			status, code, http.StatusOK)
	}

	// The acme-only account holds its membership and reader grant in
	// tenant-acme only; signing in for the tenant it has no membership in is
	// refused before any route exists with the unified 401
	// authn.invalid_credentials answer a wrong password also gets -- the
	// account is real and the password right, but the login endpoint must
	// not disclose that to an anonymous caller (the answer was deliberately
	// unified: the specific reason is recorded in the login history, never
	// the response).
	status, code, _ = demoLogin(t, srv, demo.DemoAcmeOnlyEmail, demoSeedPassword, "tenant-globex")
	if status != http.StatusUnauthorized || code != "authn.invalid_credentials" {
		t.Fatalf("login as the acme-only account in tenant-globex: status = %d, code = %q, want 401 %q",
			status, code, "authn.invalid_credentials")
	}
	// The refusal's real reason: the 401 above is also a wrong password's
	// answer, so the login history -- the one place authn writes the
	// specific reason -- must show this attempt as the no-membership
	// refusal it is (the sign-in into tenant-acme just above proved the
	// password; history proves the globex refusal was the missing
	// membership).
	assertNoMembershipRefusal(t, srv, acmeOnlyToken, "login as the acme-only account in tenant-globex")
}

// TestDemoUsers_SecondBootAgainstTheSameDatabase_SignInsSurvive pins what a
// process restart against the same database actually does to the demo-user
// seed. Boot one registers every account, signs in fine, and shuts down.
// Boot two, with the seed switched on again, finds the registrations
// already in authn's users table, re-asserts each account's grants under
// the user id authn assigned on boot one (SeedDemoUsers' own doc comment),
// and every sign-in that worked under boot one still works: the demo
// accounts' memberships are org rows that predate boot two and are read
// straight from the database, and the platform-staff account's
// rbac.SystemDomain membership is re-granted by SeedDemoPlatformStaff on
// the same already-exists path.
//
// Boot two (the honest image of a restart) must answer every account as a
// member; the fold of no-membership logins into
// ErrInvalidCredentials answers a missing membership with the
// unified 401 authn.invalid_credentials a wrong password also gets (the
// reason survives only in the login history, never the response), so no
// distinguishable refusal exists for this test to assert and its proof is
// the 200s themselves -- every sign-in below must succeed, and a
// restarted boot that lost the memberships fails them red. The defect
// was the scale-to-zero restart one that killed every demo account (and
// locked the platform-staff account out of admin's console) whenever an
// idle instance stopped and came back.
func TestDemoUsers_SecondBootAgainstTheSameDatabase_SignInsSurvive(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reference-app-seed-restart.db")

	// boot composes a server against the shared dbPath, without any test
	// cleanup: the caller closes and cleans up each boot explicitly, in
	// order. Each boot carries its own fresh membership store -- the honest
	// image of a restart, where nothing boot one held in memory exists.
	// Both demo seed variables are set, each to its OWN passphrase, exactly
	// as an operator enabling the full demo would (the platform-staff
	// account is seeded from APP_DEMO_PLATFORM_STAFF_PASSWORD alone, never
	// from the demo users' variable -- internal/app/demo/demo_admin.go).
	boot := func() (*httptest.Server, func() error) {
		cfg := testConfig(t)
		cfg.SQLitePath = dbPath
		cfg.DemoUsersPassword = demoSeedPassword
		cfg.DemoPlatformStaffPassword = demoPlatformStaffSeedPassword
		handler, cleanup, _, err := app.BuildServer(context.Background(), cfg)
		if err != nil {
			t.Fatalf("BuildServer: %v", err)
		}
		return httptest.NewServer(handler), cleanup
	}

	// Boot one seeds every demo account and proves the seed works.
	srv1, cleanup1 := boot()
	status, code, ownerToken := demoLogin(t, srv1, demo.DemoOwnerEmail, demoSeedPassword, "tenant-acme")
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
	// already registered, so the seed's register leg reports the conflict
	// and its grant leg re-asserts: the org rows survive boot one, and the
	// platform-staff SystemDomain grant is re-made from the recovered user
	// id. All three accounts' sign-ins must work exactly as they did under
	// boot one.
	srv2, cleanup2 := boot()
	defer func() {
		srv2.Close()
		if err := cleanup2(); err != nil {
			t.Errorf("boot-two cleanup: %v", err)
		}
	}()

	status, code, ownerToken2 := demoLogin(t, srv2, demo.DemoOwnerEmail, demoSeedPassword, "tenant-acme")
	if status != http.StatusOK {
		t.Fatalf("boot-two login as the seeded owner: status = %d, code = %q, want %d "+
			"(a demo account's org membership row must survive a restart)",
			status, code, http.StatusOK)
	}
	resp = notesRequestAs(t, srv2, http.MethodGet, ownerToken2, "", nil)
	func() {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("boot-two GET as the seeded owner: status = %d, want %d; body = %s", resp.StatusCode, http.StatusOK, body)
		}
	}()

	status, code, _ = demoLogin(t, srv2, demo.DemoReaderEmail, demoSeedPassword, "tenant-acme")
	if status != http.StatusOK {
		t.Fatalf("boot-two login as the seeded reader: status = %d, code = %q, want %d", status, code, http.StatusOK)
	}

	status, code, _ = demoLogin(t, srv2, demo.DemoPlatformStaffEmail, demoPlatformStaffSeedPassword, rbac.SystemDomain)
	if status != http.StatusOK {
		t.Fatalf("boot-two login as the platform-staff account: status = %d, code = %q, want %d "+
			"(the staff account's SystemDomain membership must survive a restart)",
			status, code, http.StatusOK)
	}
}

// TestDemoUsers_Seeder_RateLimitAnswerNamedDistinctly pins that the seeding
// helper the boot-time seed drives (authn/demoseed's Register) distinguishes
// authn's register rate-limit answer from the other fatal answers honestly --
// naming the public per-IP register budget (limitRegisterByIP,
// go/authn/ratelimit.go) and its remedy -- rather than folding it into the
// generic "answered HTTP %d with code %q" message that reads like a
// misconfiguration. The budget here is one boot's own in-memory KVStore:
// the test exhausts the register route's no-client-address bucket with 10
// in-process register POSTs (the identical in-process, recorder-based
// shape the seeder itself uses, so every POST lands on the same
// bucket the seed's own POSTs land on), then drives the seeder
// itself and asserts the refusal is named as the rate limit -- and that an
// ordinary policy refusal (the control) never carries that name.
func TestDemoUsers_Seeder_RateLimitAnswerNamedDistinctly(t *testing.T) {
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

	ctx := context.Background()

	// The seeder is the one the seed itself builds
	// (internal/app/demo/demo_users.go's SeedDemoUsers), over the app's declared
	// demo domain. Its lookup answers "no such account" because both
	// addresses below are genuinely absent from this fresh boot's users
	// table, so every seeder.Register call reaches the register POST whose
	// classification this test pins.
	seeder, err := demoseed.NewSeeder(handler, func(context.Context, authn.UserSearchQuery) ([]authn.User, error) {
		return nil, nil
	}, demoSeedDomain)
	if err != nil {
		t.Fatalf("demoseed.NewSeeder: %v", err)
	}

	// postRegister posts one register payload in-process, exactly the
	// shape the seeder uses (no client address, so the POST debits
	// the same no-address bucket the seed's own POSTs debit), and returns
	// the status.
	postRegister := func(email, password string) int {
		payload, err := json.Marshal(map[string]string{"email": email, "password": password})
		if err != nil {
			t.Fatalf("marshal register body: %v", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, speedapp.AuthnAPIPath+"/register", bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("build register request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	// Control first, while the budget still has room: a policy refusal (a
	// password too short for authn's policy) must keep flowing through the
	// generic classification -- never mistaken for the rate limit.
	_, _, policyErr := seeder.Register(ctx, "policy-refused@example.com", "x")
	if policyErr == nil {
		t.Fatal("Register with a policy-refused password: want error, got nil")
	}
	if strings.Contains(policyErr.Error(), "rate limit") {
		t.Fatalf("policy-refusal error names the rate limit: %v", policyErr)
	}

	// Exhaust the register budget: 9 more in-process registrations on top
	// of the debited policy attempt reach the 10-per-hour limit.
	for i := 0; i < 9; i++ {
		if status := postRegister("budget-"+strconv.Itoa(i)+"@example.com", demoSeedPassword); status != http.StatusCreated {
			t.Fatalf("register %d status = %d, want 201", i, status)
		}
	}

	// The 11th register POST (10 already debited) must answer 429, and
	// the seeder must name the answer as the public register rate
	// limit with its remedy, not as the generic fatal refusal.
	_, _, rateErr := seeder.Register(ctx, "rate-limited@example.com", demoSeedPassword)
	if rateErr == nil {
		t.Fatal("Register with an exhausted register budget: want error, got nil")
	}
	for _, want := range []string{"public register rate limit", authn.ErrRateLimited.Code, "retry after the sliding window"} {
		if !strings.Contains(rateErr.Error(), want) {
			t.Fatalf("rate-limit error = %v, want it to name %q", rateErr, want)
		}
	}
}
