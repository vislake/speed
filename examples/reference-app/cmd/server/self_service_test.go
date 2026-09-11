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
	"testing"
	"time"

	"github.com/vislake/speed/examples/reference-app/internal/apptest"

	"github.com/vislake/speed/examples/reference-app/internal/app"
	"github.com/vislake/speed/examples/reference-app/internal/testutil"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// self_service_test.go is the regression suite for the self-service
// signup journey: registration provisions the account's clinic
// (internal/app/self_service.go), so the journey must land the account inside its own
// clinic tenant with the org root, the membership and the owner grant it
// can act on -- and that must survive the process restart that kills
// every in-memory answer: the org rows alone answer on restart (the
// "which tenants" question reads them through org's own cross-tenant
// query; see the no-ledger regression below), the same two-boot shape
// the demo-users and invitation suites already pin for their own
// memberships. The failure-injection/retry half is pinned too: an
// injected synchronous failure must converge through the retry job --
// without a retry, the authn.user.created event never fires again and
// the browser-shaped sign-in stays refused forever.

// TestSelfServiceSignup_RegisterThenSignIn_LandsInTheCreatedClinic drives
// the whole acceptance journey through the real composed HTTP stack: a
// fresh account registers, the browser-shaped sign-in that follows lands
// it inside its OWN clinic tenant (the deterministic ClinicTenantOf
// derivation -- never one of the configured demo tenants), the clinic's
// org tree answers the account's bearer token with the root node
// registration provisioned, and the notes gate answers the owner grant
// with a real write. The named-tenant control keeps the journey honest
// about what the clinic is NOT: the account holds no membership in any
// configured tenant, so a sign-in asking for tenant-acme is refused --
// the unified 401 authn.invalid_credentials answer, identical to a wrong
// password's (the login endpoint never distinguishes the membership gate
// from a bad password).
func TestSelfServiceSignup_RegisterThenSignIn_LandsInTheCreatedClinic(t *testing.T) {
	srv, _, _ := apptest.BuildServer(t)

	// A fresh account registers through authn's real register route.
	userID := testutil.RegisterFreshAccount(t, srv, testutil.SelfServiceFreshEmail, testutil.SelfServicePassword)

	// The browser-shaped sign-in succeeds and lands the principal in the
	// account's OWN clinic -- a sign-in a memberless account cannot take --
	// the deterministic tenant derived from the registrant's user id
	// (internal/app/self_service.go's ClinicTenantOf), never a configured demo tenant.
	// The derivation is spelled out here rather than reached through the
	// production helper, so the assertion pins the literal tenant id
	// derivation ("tenant-" + the registrant's user id) rather than the
	// helper's own answer.
	status, code, token, tenant := testutil.BrowserSignIn(t, srv, testutil.SelfServiceFreshEmail, testutil.SelfServicePassword)
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
	// control: the specific no-membership reason
	// lives in the login history, never the response).
	status, code, _ = testutil.DemoLogin(t, srv, testutil.SelfServiceFreshEmail, testutil.SelfServicePassword, "tenant-acme")
	if status != http.StatusUnauthorized || code != "authn.invalid_credentials" {
		t.Fatalf("sign-in of the clinic owner into tenant-acme: status = %d, code = %q, want 401 %q",
			status, code, "authn.invalid_credentials")
	}
	// The refusal's real reason: the 401 above is also a wrong password's
	// answer, so the login history -- the one place authn writes the
	// specific reason -- must show this attempt as the no-membership
	// refusal it is (the browser-shaped sign-in into the clinic above
	// proved the password; history proves the tenant-acme refusal was the
	// missing membership).
	testutil.AssertNoMembershipRefusal(t, srv, token, "sign-in of the clinic owner into tenant-acme")

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
	noteResp := testutil.NotesRequestAs(t, srv, http.MethodPost, token, "", strings.NewReader(`{"text":"the clinic owner's first note"}`))
	func() {
		defer noteResp.Body.Close()
		if noteResp.StatusCode != http.StatusCreated {
			raw, _ := io.ReadAll(noteResp.Body)
			t.Fatalf("POST /api/v1/notes as the clinic owner: status = %d, want %d; body = %s",
				noteResp.StatusCode, http.StatusCreated, raw)
		}
	}()
	listResp := testutil.NotesRequestAs(t, srv, http.MethodGet, token, "", nil)
	func() {
		defer listResp.Body.Close()
		if listResp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(listResp.Body)
			t.Fatalf("GET /api/v1/notes as the clinic owner: status = %d, want %d; body = %s",
				listResp.StatusCode, http.StatusOK, raw)
		}
		var listed testutil.TestListNotesResponse
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
// self_service_clinics ledger an earlier shape relied on for
// boot-time re-discovery), so nothing boot
// one held in memory may be load-bearing -- the same two-boot shape the
// demo-users and invitation suites use for their own memberships.
func TestSelfServiceSignup_ClinicOwnerSignInSurvivesARestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reference-app-self-service-restart.db")

	// boot composes a server against the shared dbPath without any test
	// cleanup: the caller closes and cleans up each boot explicitly, in
	// order.
	boot := func() (*httptest.Server, func() error) {
		cfg := apptest.ServerConfig(t)
		cfg.SQLitePath = dbPath
		handler, cleanup, _, err := app.BuildServer(context.Background(), cfg)
		if err != nil {
			t.Fatalf("BuildServer: %v", err)
		}
		return httptest.NewServer(handler), cleanup
	}

	// Boot one: register and prove the clinic sign-in works.
	srv1, cleanup1 := boot()
	testutil.RegisterFreshAccount(t, srv1, testutil.SelfServiceFreshEmail, testutil.SelfServicePassword)
	status, code, _, tenant := testutil.BrowserSignIn(t, srv1, testutil.SelfServiceFreshEmail, testutil.SelfServicePassword)
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
	status, code, _, tenant = testutil.BrowserSignIn(t, srv2, testutil.SelfServiceFreshEmail, testutil.SelfServicePassword)
	if status != http.StatusOK {
		t.Fatalf("boot-two sign-in of the clinic owner: status = %d, code = %q, want %d "+
			"(the clinic's org membership row must survive a restart)",
			status, code, http.StatusOK)
	}
	if tenant != clinic {
		t.Fatalf("boot-two sign-in landed the principal in tenant %q, want the boot-one clinic %q", tenant, clinic)
	}
}

// TestSelfServiceSignup_ProvisioningFailure_RetriedUntilTheClinicExists is
// the failure half of the self-service journey: the register route answers
// 201 whether the synchronous provisioning attempt succeeded or failed
// (authn never sees the failure -- internal/app/self_service.go's # Failure semantics),
// so a failed attempt must recover on its own. It does, through the retry
// job scheduleProvisionRetry enqueues on the app's standalone queue: the
// job re-runs the same idempotent provision until it succeeds, and the
// account that registered into a failed attempt signs in and lands in its
// clinic once the retry converges -- never stranded by a provisioning
// hiccup.
//
// The injection is armed through the server's own config
// (cfg.FailSelfServiceProvision) before BuildServer captures it into the
// provisioner, so the failure hits the real synchronous delivery inside
// the register request, exactly where a genuine provisioning failure
// would; the rest of the journey -- the register POST, the queue worker
// that picks the retry job up, the browser-shaped sign-in -- is the real
// composed server, nothing mocked.
//
// The pinned behavior: register answers 201 and the injected failure is
// consumed by the one synchronous attempt; the retry job is what re-runs
// provision until the clinic exists. Without the retry, nothing would
// ever run provision again -- authn.user.created fires once -- the
// clinic would never gain the registrant's org membership row, and the
// browser-shaped sign-in would stay refused: the membership poll below
// would time out. Its passing is the retry-wiring proof.
func TestSelfServiceSignup_ProvisioningFailure_RetriedUntilTheClinicExists(t *testing.T) {
	inject := &testutil.FailOnceProvisioning{}
	cfg := apptest.ServerConfig(t)
	cfg.FailSelfServiceProvision = inject.Fail
	handler, cleanup, _, err := app.BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
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
	userID := testutil.RegisterFreshAccount(t, srv, testutil.SelfServiceFreshEmail, testutil.SelfServicePassword)
	if !inject.Observed() {
		t.Fatal("the synchronous provisioning attempt never consumed the injected failure, so nothing here exercises the retry")
	}
	clinic := pkgcore.TenantID("tenant-" + userID)

	// The clinic exists only once a provisioning attempt has run far
	// enough to land the registrant's org membership -- the durable
	// record of a completed provision is that row.
	// testutil.WaitForClinicMembership polls it through a second connection to the
	// server's own database file (the same-shape second connection the
	// audit suite's persister test uses), with the same filter org's own
	// cross-tenant query applies -- status "active", never soft-deleted
	// (go/org/membership.go's MembershipStatusActive and
	// go/org/membership_tenants.go's membershipTenantRow) -- a read, so no
	// authn rate-limit budget is spent waiting for the retry.
	testutil.WaitForClinicMembership(t, cfg.SQLitePath, clinic, userID)

	// The retry converged the clinic; the browser-shaped sign-in now lands
	// in it.
	status, code, _, tenant := testutil.BrowserSignIn(t, srv, testutil.SelfServiceFreshEmail, testutil.SelfServicePassword)
	if status != http.StatusOK {
		t.Fatalf("sign-in of the clinic owner after the retried provisioning: status = %d, code = %q, want %d",
			status, code, http.StatusOK)
	}
	if tenant != clinic {
		t.Fatalf("sign-in after the retried provisioning landed the principal in tenant %q, want its clinic %q", tenant, clinic)
	}
}

const selfServiceEnvDrivenEmail = "env-driven-founder@example.com"

// selfServiceJourneyConfigFromEnv returns the ServerConfig a REAL boot
// reads (ConfigFromEnv) under a hermetic environment: every variable
// ConfigFromEnv reads is cleared first, so the journey's outcome cannot
// depend on the ambient environment the test happens to run in (a stray
// APP_REDIS_ADDR or APP_S3_* in the shell would silently rewire a seam or
// refuse the boot outright), with APP_DB_PATH pointed at a fresh per-test
// temp file and APP_FAIL_SELF_SERVICE_PROVISION set to failCount.
func selfServiceJourneyConfigFromEnv(t *testing.T, failCount string) app.ServerConfig {
	t.Helper()
	testutil.ClearBootstrapEnv(t)
	t.Setenv("APP_DB_PATH", filepath.Join(t.TempDir(), "self-service-env-driven.db"))
	t.Setenv("APP_FAIL_SELF_SERVICE_PROVISION", failCount)
	cfg, err := app.ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	return cfg
}

// TestSelfServiceSignup_EnvDrivenFirstProvisionFailure_RetryConverges_LaterSignInLandsInClinic
// is the env-driven half of the failure journey, driven the way a real
// boot reads it: the server is built from ConfigFromEnv's own output with
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
// Failing before the switch existed: ConfigFromEnv ignored the variable,
// cfg.FailSelfServiceProvision came back nil, and this test failed at the
// armed-hook assertion before any request was served.
func TestSelfServiceSignup_EnvDrivenFirstProvisionFailure_RetryConverges_LaterSignInLandsInClinic(t *testing.T) {
	cfg := selfServiceJourneyConfigFromEnv(t, "1")
	handler, cleanup, _, err := app.BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer func() {
		srv.Close()
		if cleanupErr := cleanup(); cleanupErr != nil {
			t.Errorf("cleanup: %v", cleanupErr)
		}
	}()

	if cfg.FailSelfServiceProvision == nil {
		t.Fatal("APP_FAIL_SELF_SERVICE_PROVISION=1 booted a server whose provisioning is not injected -- the env switch is not wired into app.ConfigFromEnv")
	}

	userID := testutil.RegisterFreshAccount(t, srv, selfServiceEnvDrivenEmail, testutil.SelfServicePassword)
	clinic := pkgcore.TenantID("tenant-" + userID)

	// The register's synchronous attempt consumed the account's one
	// injected failure -- the account's budget is spent, so the hook now
	// answers nil for it (and still fails an untouched account's first
	// attempt, proving the hook is live and per-account).
	if err := cfg.FailSelfServiceProvision(userID); err != nil {
		t.Fatalf("the register's synchronous provisioning attempt never consumed its injected failure (hook answers %v): the convergence this test watches would not be the retry's work", err)
	}
	if err := cfg.FailSelfServiceProvision("unrelated-fresh-account"); err == nil {
		t.Fatal("the armed injection did not fail an untouched account's first attempt")
	}

	// The retry job converges the clinic: the ledger row (provisioning's
	// last step) appears only once a full provisioning attempt has run,
	// and after the failed synchronous attempt that attempt is the
	// retry's.
	testutil.WaitForClinicMembership(t, cfg.SQLitePath, clinic, userID)

	// One sign-in, once the clinic exists: it lands in the account's own
	// clinic. This is the e2e gate's own shape -- register, converge,
	// sign in once -- inside authn's per-account login budget.
	status, code, _, tenant := testutil.BrowserSignIn(t, srv, selfServiceEnvDrivenEmail, testutil.SelfServicePassword)
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
// context the queue rebuilt for the hook) and the terminal condition --
// the clinic needs manual provisioning. The queue's own generic
// dead-letter log names the job (job_id/job_type), never the account, so
// this host line is what makes the dead letter actionable.
//
// The regression drives the REAL handler and a REAL queue to a genuine
// exhaustion: an always-failing provision injection, a fast retry
// cadence (this host's production cadence of one-second base backoff over
// ten retries would take minutes -- the queue's own option exists for
// tests exactly like this), and the handler's own task payload. The
// worker context carries no attached logger, so obs.FromContext falls
// back to slog.Default(), which the test captures.
//
// The pinned behavior: a handler with no FailureHook lets the
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
	handler := &app.SelfServiceProvisionJobHandler{Provisioner: &app.SelfServiceProvisioner{
		FailProvision: func(string) error { return errors.New("injected terminal provisioning failure") },
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
	clinic := app.ClinicTenantOf(userID)
	payload, err := json.Marshal(app.SelfServiceProvisionTask{UserID: userID})
	if err != nil {
		t.Fatalf("marshal the provisioning retry task: %v", err)
	}
	jobID, err := queue.Enqueue(ctx, jobs.Task{
		Type:     app.SelfServiceProvisionTaskType,
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
	// queue rebuilt for the hook) and the terminal condition -- the clinic
	// needs manual provisioning. The queue's generic dead-letter line
	// names the job, never the account.
	signal := "reference-app: clinic provisioning exhausted its retries and dead-lettered; the clinic needs manual provisioning"
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
