package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/jobs"
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
// in-memory answer: the org rows alone answer on restart (the "which
// tenants" question reads them through org's own cross-tenant query --
// the host's self_service_clinics ledger was retired in the same round;
// see the no-ledger regression below), the same two-boot shape the
// demo-users and invitation suites already pin for their own memberships.
//
// Failing before the fix: on the pre-self-service code this suite's first
// test answered the browser-shaped sign-in with the 403 the deleted
// TestDemoUsers_RegisteredButMemberless_BrowserShapedSignInRefused used
// to pin, and the assertions below failed where it passed; the second
// test failed one step earlier (boot one's sign-in never succeeded). On
// the pre-retry code -- the provisioning fix had landed, the retry had
// not -- the suite's last test timed out waiting for a clinic that
// nothing would ever provision: the injected failure was consumed by the
// one synchronous attempt, the authn.user.created event never fires
// again, and no retry existed, so the browser-shaped sign-in stayed
// refused forever.

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
// configured tenant, so a sign-in asking for tenant-acme is refused --
// the unified 401 authn.invalid_credentials answer, identical to a wrong
// password's (the login endpoint no longer names the membership gate).
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
	// back door into someone else's. The refusal is the unified 401
	// authn.invalid_credentials answer a wrong password also gets (this
	// control used to pin the distinguishable 403
	// authn.tenant_membership_required; the specific no-membership reason
	// now lives in the login history, never the response).
	status, code, _ = demoLogin(t, srv, selfServiceFreshEmail, selfServicePassword, "tenant-acme")
	if status != http.StatusUnauthorized || code != "authn.invalid_credentials" {
		t.Fatalf("sign-in of the clinic owner into tenant-acme: status = %d, code = %q, want 401 %q",
			status, code, "authn.invalid_credentials")
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
// the "which tenants" answer reads org's own memberships table directly
// (MemberService.TenantsOf behind the sign-in store -- the
// self_service_clinics ledger this suite's earlier rounds relied on for
// boot-time re-discovery was retired in the same round), so nothing boot
// one held in memory may be load-bearing -- the same two-boot shape the
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
	// clinic -- the org membership row survives, and the sign-in store's
	// answer reads it directly through org's own cross-tenant query.
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
			"(the clinic's org membership row must survive a restart)",
			status, code, http.StatusOK)
	}
	if tenant != clinic {
		t.Fatalf("boot-two sign-in landed the principal in tenant %q, want the boot-one clinic %q", tenant, clinic)
	}
}

// failOnceProvisioning fails exactly the first provisioning attempt it is
// asked about and succeeds afterwards -- the
// serverConfig.failSelfServiceProvision hook shape the failure-half
// regression below arms before building its server.
type failOnceProvisioning struct {
	mu       sync.Mutex
	attempts int
}

// fail implements the failSelfServiceProvision hook: the first attempt
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

// TestSelfServiceSignup_ProvisioningFailure_RetriedUntilTheClinicExists is
// the failure half of the self-service journey: the register route answers
// 201 whether the synchronous provisioning attempt succeeded or failed
// (authn never sees the failure -- self_service.go's # Failure semantics),
// so a failed attempt must recover on its own. It does, through the retry
// job scheduleProvisionRetry enqueues on the app's standalone queue: the
// job re-runs the same idempotent provision until it succeeds, and the
// account that registered into a failed attempt signs in and lands in its
// clinic once the retry converges -- never stranded by a provisioning
// hiccup.
//
// The injection is armed through the server's own config
// (cfg.failSelfServiceProvision) before buildServer captures it into the
// provisioner, so the failure hits the real synchronous delivery inside
// the register request, exactly where a genuine provisioning failure
// would; the rest of the journey -- the register POST, the queue worker
// that picks the retry job up, the browser-shaped sign-in -- is the real
// composed server, nothing mocked.
//
// Failing before the fix (the retry wiring absent): register answered
// 201, the injected failure was consumed by the one synchronous attempt,
// and nothing ever ran provision again -- authn.user.created fires once
// -- so the clinic never gained the registrant's org membership row and
// the browser-shaped sign-in stayed refused. The membership poll below
// therefore timed out and the test failed where it now passes.
func TestSelfServiceSignup_ProvisioningFailure_RetriedUntilTheClinicExists(t *testing.T) {
	inject := &failOnceProvisioning{}
	cfg := testConfig(t)
	cfg.failSelfServiceProvision = inject.fail
	handler, cleanup, _, err := buildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer func() {
		srv.Close()
		if cleanupErr := cleanup(); cleanupErr != nil {
			t.Errorf("cleanup: %v", cleanupErr)
		}
	}()

	// Register answers 201 even though the synchronous provisioning
	// attempt fails: that IS the semantics under test -- the recovery is
	// the retry, never a changed answer.
	userID := registerFreshAccount(t, srv, selfServiceFreshEmail, selfServicePassword)
	if !inject.observed() {
		t.Fatal("the synchronous provisioning attempt never consumed the injected failure, so nothing here exercises the retry")
	}
	clinic := pkgcore.TenantID("tenant-" + userID)

	// The clinic exists only once a provisioning attempt has run far
	// enough to land the registrant's org membership (the host's
	// self_service_clinics ledger was retired in the round that moved the
	// sign-in answer onto org's own memberships table, so the durable
	// record of a completed provision is that row).
	// waitForClinicMembership polls it through a second connection to the
	// server's own database file (the same-shape second connection the
	// audit suite's persister test uses), with the same filter org's own
	// cross-tenant query applies -- status "active", never soft-deleted
	// (go/org/membership.go's MembershipStatusActive and
	// go/org/membership_tenants.go's membershipTenantRow) -- a read, so no
	// authn rate-limit budget is spent waiting for the retry.
	waitForClinicMembership(t, cfg.SQLitePath, clinic, userID)

	// The retry converged the clinic; the browser-shaped sign-in that the
	// pre-self-service dead end used to refuse now lands in it.
	status, code, _, tenant := browserSignIn(t, srv, selfServiceFreshEmail, selfServicePassword)
	if status != http.StatusOK {
		t.Fatalf("sign-in of the clinic owner after the retried provisioning: status = %d, code = %q, want %d",
			status, code, http.StatusOK)
	}
	if tenant != clinic {
		t.Fatalf("sign-in after the retried provisioning landed the principal in tenant %q, want its clinic %q", tenant, clinic)
	}
}

// waitForClinicMembership polls clinic's registrant membership row through a
// second connection to the SQLite file sqlitePath until it appears or the
// deadline passes. The membership row is the durable record of a completed
// provision once the host's self_service_clinics ledger was retired (the
// round that moved the sign-in answer onto org's own memberships table), so
// its appearance means a provisioning attempt ran to completion -- after a
// failed synchronous attempt, the completing attempt can only be the retry
// job's. The poll applies the same filter org's own cross-tenant query
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

const selfServiceEnvDrivenEmail = "env-driven-founder@example.com"

// selfServiceJourneyConfigFromEnv returns the serverConfig a REAL boot
// reads (configFromEnv) under a hermetic environment: every variable
// configFromEnv reads is cleared first, so the journey's outcome cannot
// depend on the ambient environment the test happens to run in (a stray
// APP_REDIS_ADDR or APP_S3_* in the shell would silently rewire a seam or
// refuse the boot outright), with APP_DB_PATH pointed at a fresh per-test
// temp file and APP_FAIL_SELF_SERVICE_PROVISION set to failCount.
func selfServiceJourneyConfigFromEnv(t *testing.T, failCount string) serverConfig {
	t.Helper()
	for _, name := range [...]string{
		"APP_DEPLOYMENT_MODE", "PORT", "APP_REDIS_ADDR", "APP_ROOT_KEY",
		"APP_CONFIG_KEY", "APP_ORG_INDEX_KEY", "APP_NOTIFICATION_INDEX_KEY",
		"APP_PKI_LOCAL_KEY_CIPHER_KEY", "APP_AUTHN_BLIND_INDEX_KEY", "APP_AUTHN_PII_CIPHER_KEY",
		"APP_S3_ENDPOINT", "APP_S3_BUCKET", "APP_S3_ACCESS_KEY", "APP_S3_SECRET_KEY",
		"APP_S3_REGION", "APP_S3_USE_SSL", "APP_OBJECT_STORE_ROOT",
		"APP_SMTP_HOST", "APP_SMTP_PORT", "APP_SMTP_USERNAME", "APP_SMTP_PASSWORD",
		"APP_SMS_GATEWAY_URL", "APP_DISABLE_QUEUE_WORKER", "APP_DISABLE_DEMO_USER_HEADER",
		"APP_TRUSTED_PROXIES", "APP_READ_FLY_CLIENT_IP", "APP_WEB_DIST",
		"APP_DEMO_USERS_PASSWORD", "APP_DEMO_PLATFORM_STAFF_PASSWORD",
		"APP_AI_GATEWAY_IMAGE_BASE_URL", "APP_AI_GATEWAY_IMAGE_API_KEY",
		"APP_FAIL_SELF_SERVICE_PROVISION",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("APP_DB_PATH", filepath.Join(t.TempDir(), "self-service-env-driven.db"))
	t.Setenv("APP_FAIL_SELF_SERVICE_PROVISION", failCount)
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}
	return cfg
}

// TestSelfServiceSignup_EnvDrivenFirstProvisionFailure_RetryConverges_LaterSignInLandsInClinic
// is the env-driven half of the failure journey, driven the way a real
// boot reads it: the server is built from configFromEnv's own output with
// APP_FAIL_SELF_SERVICE_PROVISION=1 in the environment, so a fresh
// self-service register fails its first synchronous provisioning attempt,
// the retry job converges the clinic, and a subsequent browser-shaped
// sign-in lands in it. It is the e2e-drivable shape: N=1 makes the
// recovery converge on the retry's first attempt (its backoff is short),
// so the e2e gate can sign in once after convergence inside authn's
// per-account login budget, with no "retry until it passes" loop.
//
// The armed hook itself is the evidence the synchronous attempt really
// failed -- the account's one-attempt budget is spent by the register's
// own attempt, and an untouched account's first attempt still fails -- so
// the convergence this test then watches (the ledger row, provisioning's
// last step) is provably the retry job's work, never a synchronous
// success.
//
// Failing before the switch existed: configFromEnv ignored the variable,
// cfg.failSelfServiceProvision came back nil, and this test failed at the
// armed-hook assertion before any request was served.
func TestSelfServiceSignup_EnvDrivenFirstProvisionFailure_RetryConverges_LaterSignInLandsInClinic(t *testing.T) {
	cfg := selfServiceJourneyConfigFromEnv(t, "1")
	handler, cleanup, _, err := buildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer func() {
		srv.Close()
		if cleanupErr := cleanup(); cleanupErr != nil {
			t.Errorf("cleanup: %v", cleanupErr)
		}
	}()

	if cfg.failSelfServiceProvision == nil {
		t.Fatal("APP_FAIL_SELF_SERVICE_PROVISION=1 booted a server whose provisioning is not injected -- the env switch is not wired into configFromEnv")
	}

	userID := registerFreshAccount(t, srv, selfServiceEnvDrivenEmail, selfServicePassword)
	clinic := pkgcore.TenantID("tenant-" + userID)

	// The register's synchronous attempt consumed the account's one
	// injected failure -- the account's budget is spent, so the hook now
	// answers nil for it (and still fails an untouched account's first
	// attempt, proving the hook is live and per-account).
	if err := cfg.failSelfServiceProvision(userID); err != nil {
		t.Fatalf("the register's synchronous provisioning attempt never consumed its injected failure (hook answers %v): the convergence this test watches would not be the retry's work", err)
	}
	if err := cfg.failSelfServiceProvision("unrelated-fresh-account"); err == nil {
		t.Fatal("the armed injection did not fail an untouched account's first attempt")
	}

	// The retry job converges the clinic: the ledger row (provisioning's
	// last step) appears only once a full provisioning attempt has run,
	// and after the failed synchronous attempt that attempt is the
	// retry's.
	waitForClinicMembership(t, cfg.SQLitePath, clinic, userID)

	// One sign-in, once the clinic exists: it lands in the account's own
	// clinic. This is the e2e gate's own shape -- register, converge,
	// sign in once -- inside authn's per-account login budget.
	status, code, _, tenant := browserSignIn(t, srv, selfServiceEnvDrivenEmail, selfServicePassword)
	if status != http.StatusOK {
		t.Fatalf("sign-in of the env-driven clinic owner after the retried provisioning: status = %d, code = %q, want %d",
			status, code, http.StatusOK)
	}
	if tenant != clinic {
		t.Fatalf("sign-in after the env-driven retried provisioning landed the principal in tenant %q, want its clinic %q", tenant, clinic)
	}
}

// TestSelfServiceProvisionRetry_DeadLetter_LogsTheTerminalSignalByUserAndTenant
// pins the retry job's terminal signal: when a provisioning retry job
// exhausts its budget and dead-letters, the host must log an Error naming
// the account (user_id), its clinic (tenant_id, carried by the worker
// context the queue rebuilt for the hook) and the consequence -- the
// account cannot sign in until it is provisioned by hand. Before the fix
// the only terminal record was the queue's generic dead-letter log, which
// names the job (job_id/job_type), never the account.
//
// The regression drives the REAL handler and a REAL queue to a genuine
// exhaustion: an always-failing provision injection, a fast retry
// cadence (this host's production cadence of one-second base backoff over
// ten retries would take minutes -- the queue's own option exists for
// tests exactly like this), and the handler's own task payload. The
// worker context carries no attached logger, so obs.FromContext falls
// back to slog.Default(), which the test captures.
//
// Failing before the fix: the handler implemented no FailureHook, so the
// job dead-lettered with only the generic queue line and the
// terminal-signal assertion below never matched.
func TestSelfServiceProvisionRetry_DeadLetter_LogsTheTerminalSignalByUserAndTenant(t *testing.T) {
	var logBuf bytes.Buffer
	previousDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(previousDefault)

	ctx := context.Background()
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     filepath.Join(t.TempDir(), "self-service-terminal.db"),
	})
	if err != nil {
		t.Fatalf("dbkit.Open: %v", err)
	}
	defer func() {
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
	}()

	queue := jobs.NewStandaloneQueue(db,
		jobs.WithPollInterval(15*time.Millisecond),
		jobs.WithBackoff(10*time.Millisecond, 50*time.Millisecond))
	// The provisioner behind the handler needs no booted org/rbac modules
	// here: the injection fails at the top of provision, before any
	// org/rbac step runs, which is exactly the terminal path under test.
	handler := &selfServiceProvisionJobHandler{provisioner: &selfServiceProvisioner{
		failProvision: func(string) error { return errors.New("injected terminal provisioning failure") },
	}}
	if regErr := queue.RegisterHandler(handler); regErr != nil {
		t.Fatalf("RegisterHandler: %v", regErr)
	}
	if startErr := queue.Start(ctx); startErr != nil {
		t.Fatalf("queue Start: %v", startErr)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if closeErr := queue.Close(closeCtx); closeErr != nil {
			t.Errorf("queue Close: %v", closeErr)
		}
	}()

	const userID = "terminal-account"
	clinic := clinicTenantOf(userID)
	payload, err := json.Marshal(selfServiceProvisionTask{UserID: userID})
	if err != nil {
		t.Fatalf("marshal the provisioning retry task: %v", err)
	}
	jobID, err := queue.Enqueue(ctx, jobs.Task{
		Type:     selfServiceProvisionTaskType,
		TenantID: clinic,
		Payload:  payload,
	}, jobs.WithMaxRetries(1))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// The job genuinely exhausts its retries and dead-letters -- the
	// terminal state the signal exists to announce. The poll reads under
	// the job's own clinic tenant: jobs' access rule (CallerMayAccess)
	// refuses a tenant-less Get of a tenant-owned job row.
	tenantCtx := pkgcore.WithTenant(ctx, clinic)
	deadline := time.Now().Add(15 * time.Second)
	for {
		job, getErr := queue.Get(tenantCtx, jobID)
		if getErr != nil {
			t.Fatalf("Get(%s): %v", jobID, getErr)
		}
		if job.Status == jobs.StatusDeadLetter {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the provisioning retry job never dead-lettered (status = %s)", job.Status)
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Stop the queue BEFORE reading the captured log: Close joins the
	// queue's goroutines, and the hook's Error line is written inside the
	// dead-letter attempt's own execute call (go/jobs' worker.go), so once
	// Close returns no goroutine can still write the buffer this test
	// reads -- reading a bytes.Buffer a worker goroutine is still writing
	// would be its own data race.
	closeCtx, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelClose()
	if err := queue.Close(closeCtx); err != nil {
		t.Fatalf("queue Close: %v", err)
	}

	// The terminal signal: the host's Error line, naming the account
	// (user_id), its clinic (tenant_id, carried by the worker context the
	// queue rebuilt for the hook) and the consequence. Failing before the
	// fix, the only terminal record was the queue's generic dead-letter
	// line, which names the job, never the account.
	signal := "reference-app: clinic provisioning exhausted its retries and dead-lettered; the account cannot sign in until it is provisioned by hand"
	out := logBuf.String()
	if !strings.Contains(out, signal) {
		t.Fatalf("the exhausted provisioning never logged its terminal signal naming the account; logs:\n%s", out)
	}
	if !strings.Contains(out, "user_id="+userID) {
		t.Fatalf("the terminal signal does not name the account; logs:\n%s", out)
	}
	if !strings.Contains(out, "tenant_id="+string(clinic)) {
		t.Fatalf("the terminal signal does not name the clinic tenant; logs:\n%s", out)
	}
}
