package flowtests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// test_support_test.go mirrors the demo-identity and self-service journey
// helpers whose originals live in cmd/server's staying suites
// (demo_users_test.go and self_service_test.go): the migrated flow suites
// that register accounts, seed the demo users or drive the self-service
// signup journey use them from this package, and Go test helpers cannot be
// imported across packages, so the two packages each carry a copy. Keep
// the mirror in step with the original.

// demoSeedPassword is the passphrase the demo-account suites seed demo accounts with. It must
// satisfy go/authn's password policy (length-based) -- which is exactly the
// point: registration runs through the real register route, so a password
// the policy refused would fail the boot the same way it fails a browser.
const demoSeedPassword = "demo users seed passphrase"

// demoPlatformStaffSeedPassword is the test passphrase the suites that seed
// the demo platform-staff account (internal/app/demo_admin.go's seedDemoPlatformStaff)
// set its OWN config field to -- deliberately a DIFFERENT value from
// demoSeedPassword, mirroring the runtime split between
// APP_DEMO_USERS_PASSWORD and APP_DEMO_PLATFORM_STAFF_PASSWORD
// (internal/app/demo_admin.go). It must satisfy go/authn's password policy for the same
// registration-through-the-real-route reason demoSeedPassword documents.
const demoPlatformStaffSeedPassword = "platform staff seed passphrase"

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
// after the refusal they pin: since the fold of no-membership logins
// into ErrInvalidCredentials, that 401 is byte-identical to a wrong
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

// selfServiceFreshEmail is the account the self-service journeys register. The
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

// failOnceProvisioning fails exactly the first provisioning attempt it is
// asked about and succeeds afterwards -- the
// ServerConfig.FailSelfServiceProvision hook shape the failure-half
// regression below arms before building its server.
type failOnceProvisioning struct {
	mu       sync.Mutex
	attempts int
}

// fail implements the FailSelfServiceProvision hook: the first attempt
// fails, later ones (the retry job's) succeed.
func (f *failOnceProvisioning) fail(userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.attempts == 1 {
		return errors.New("injected provisioning failure")
	}
	return nil
}

// observed reports whether any provisioning attempt consumed the hook --
// the guarantee that the injected failure really fired (and therefore
// that the convergence the test then watches was the retry job's work,
// not a synchronous success).
func (f *failOnceProvisioning) observed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts >= 1
}

// waitForClinicMembership polls clinic's registrant membership row through a
// second connection to the SQLite file sqlitePath until it appears or the
// deadline passes. The membership row is the durable record of a completed
// provision, so its appearance means a provisioning attempt ran to
// completion -- after a failed synchronous attempt, the completing attempt
// can only be the retry job's. The poll applies the same filter org's own cross-tenant query
// applies -- status "active", never soft-deleted (go/org/membership.go's
// MembershipStatusActive and go/org/membership_tenants.go's
// membershipTenantRow). A read, so no authn rate-limit budget is spent
// waiting; a transient busy on the shared SQLite file while the retry's own
// writes land is a reason to poll again, never a failure.
func waitForClinicMembership(t *testing.T, sqlitePath string, clinic pkgcore.TenantID, userID string) {
	t.Helper()

	ctx, cancelPoll := context.WithCancel(context.Background())
	defer cancelPoll()
	pollDB, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: sqlitePath})
	if err != nil {
		t.Fatalf("open the server's database for the membership poll: %v", err)
	}
	defer func() {
		if sqlDB, dbErr := pollDB.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
	}()
	deadline := time.Now().Add(15 * time.Second)
	var lastCountErr error
	for {
		var count int64
		countErr := pollDB.Table("memberships").
			Where("tenant_id = ? AND user_id = ? AND status = ? AND deleted_at IS NULL",
				string(clinic), userID, "active").
			Count(&count).Error
		if countErr != nil {
			// A transient busy on the shared SQLite file while the retry's
			// own writes land is a reason to poll again, not to fail.
			lastCountErr = countErr
		} else {
			lastCountErr = nil
			if count > 0 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the clinic %s never gained the registrant's membership: register answered 201, the synchronous attempt failed, and no retry converged it -- the account is stranded (last membership read error: %v)",
				clinic, lastCountErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
