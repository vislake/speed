package testutil

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/pkgcore"
)

// AssertNoMembershipRefusal reads the account accessToken authenticates
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
func AssertNoMembershipRefusal(t *testing.T, srv *httptest.Server, accessToken, what string) {
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
	defer func() { _ = resp.Body.Close() }()
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

// DemoLogin signs the account identified by email into tenant through
// authn's real login/password surface and returns the answer: the HTTP
// status, the error code (empty on a 200) and, on success, the bearer
// access token. Callers assert on exactly the combination they expect; a
// 200 that carries no access_token fails the test here, before any caller
// could mistake a token-less success for a sign-in.
func DemoLogin(t *testing.T, srv *httptest.Server, email, password string, tenant pkgcore.TenantID) (statusCode int, code, accessToken string) {
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
	defer func() { _ = resp.Body.Close() }()

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

// DemoPlatformStaffSeedPassword is the test passphrase the suites that seed
// the demo platform-staff account (internal/app/demo/demo_admin.go's SeedDemoPlatformStaff)
// set its OWN config field to -- deliberately a DIFFERENT value from
// DemoSeedPassword, mirroring the runtime split between
// APP_DEMO_USERS_PASSWORD and APP_DEMO_PLATFORM_STAFF_PASSWORD
// (internal/app/demo/demo_admin.go). It must satisfy go/authn's password policy for the same
// registration-through-the-real-route reason DemoSeedPassword documents.
const DemoPlatformStaffSeedPassword = "platform staff seed passphrase"

// DemoSeedPassword is what the tests below seed demo accounts with. It must
// satisfy go/authn's password policy (length-based) -- which is exactly the
// point: registration runs through the real register route, so a password
// the policy refused would fail the boot the same way it fails a browser.
const DemoSeedPassword = "demo users seed passphrase" //nolint:gosec // the demo suite's documented seed passphrase, not a credential

// BrowserSignIn signs email in through authn's real login/password
// surface with the BROWSER shape -- a body naming the identifier and the
// password and no tenant_id field at all, the exact shape the web host's
// sign-in form sends. It returns the HTTP status, the error code (empty
// on a 200), and, on success, the access token and the tenant the answer
// landed the principal in. A 200 that carries no access_token fails the
// test here, before any caller could mistake a token-less success for a
// sign-in.
func BrowserSignIn(t *testing.T, srv *httptest.Server, email, password string) (statusCode int, code, accessToken string, tenant pkgcore.TenantID) {
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
	defer func() { _ = resp.Body.Close() }()

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

// RegisterFreshAccount registers email through authn's real register route
// and returns the user id authn assigned, failing the test on anything
// but a 201.
func RegisterFreshAccount(t *testing.T, srv *httptest.Server, email, password string) (userID string) {
	t.Helper()

	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		t.Fatalf("register %s: marshal body: %v", email, err)
	}
	resp, err := srv.Client().Post(srv.URL+"/api/v1/authn/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("register %s: %v", email, err)
	}
	defer func() { _ = resp.Body.Close() }()
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

// SelfServiceFreshEmail is the account every journey below registers. The
// @example.com suffix matches the flow tests' convention; the local part
// is unique to this suite so a fresh database never collides with
// anything.
const SelfServiceFreshEmail = "self-service-founder@example.com"

// SelfServicePassword is what the journey registers and signs in with. It
// satisfies go/authn's password policy (length-based), like every
// password in the suites' flows.
const SelfServicePassword = "a fresh clinic passphrase"

// TestPassword is the passphrase the HTTP-driven fixtures register and
// sign their fresh accounts in with; it satisfies go/authn's password
// policy (length-based) like every other passphrase here.
const TestPassword = "a perfectly fine passphrase"
