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

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
)

// testConfig returns a serverConfig backed by a fresh, per-test temp-file
// SQLite database, so tests never share state and never touch a real file
// outside t.TempDir().
func testConfig(t *testing.T) serverConfig {
	t.Helper()
	return serverConfig{
		DeploymentMode: pkgcore.DeploymentModeStandalone,
		Port:           "0",
		SQLitePath:     filepath.Join(t.TempDir(), "reference-app-test.db"),
		ConfigKey:      devConfigKey,
		HostTenants:    demoHostTenants,
	}
}

// buildTestServer wires up buildServer's real output behind an
// httptest.Server, so tests exercise the exact composed handler main.go
// itself serves -- tenancy.Middleware, the notes Module's real handler,
// and a real (if temp-file) SQLite database -- not a mock of any of them.
func buildTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	handler, cleanup, err := buildServer(context.Background(), testConfig(t))
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
	return srv
}

type testNote struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type testListNotesResponse struct {
	Notes []testNote `json:"notes"`
}

// createNoteAs POSTs a note with the given text to srv, as the tenant that
// host resolves to -- Host is the ONLY thing that selects the tenant, set
// on the request exactly as a real client's would be, never a header or
// body field the server would have to trust.
func createNoteAs(t *testing.T, srv *httptest.Server, host, text string) {
	t.Helper()

	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/notes", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Host = host
	req.Header.Set("Content-Type", "application/json")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/notes (Host=%s): %v", host, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /api/v1/notes (Host=%s) status = %d, want %d; body = %s",
			host, resp.StatusCode, http.StatusCreated, respBody)
	}
}

// listNotesAs GETs the notes visible to the tenant host resolves to.
func listNotesAs(t *testing.T, srv *httptest.Server, host string) []testNote {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/notes", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Host = host

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/notes (Host=%s): %v", host, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /api/v1/notes (Host=%s) status = %d, want %d; body = %s",
			host, resp.StatusCode, http.StatusOK, respBody)
	}

	var out testListNotesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response (Host=%s): %v", host, err)
	}
	return out.Notes
}

// TestBuildServer_MultiTenantIsolation_EndToEnd is the genuine, automated,
// executable proof the root task asked for: two different demo hostnames,
// mapped to two different tenants by the real strictHostResolver this
// app wires up, creating notes under each and asserting each tenant's list
// contains only its own notes -- through the real middleware + handler +
// dbkit.Repository[Note] stack this app actually serves, not a mocked
// shortcut at any layer.
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
	srv := buildTestServer(t)

	const acmeHost = "acme.demo.localhost"
	const globexHost = "globex.demo.localhost"

	createNoteAs(t, srv, acmeHost, "acme secret 1")
	createNoteAs(t, srv, acmeHost, "acme secret 2")
	createNoteAs(t, srv, globexHost, "globex secret 1")

	acmeNotes := listNotesAs(t, srv, acmeHost)
	if len(acmeNotes) != 2 {
		t.Fatalf("tenant-acme sees %d notes, want 2 (%+v)", len(acmeNotes), acmeNotes)
	}
	for _, n := range acmeNotes {
		if strings.Contains(n.Text, "globex") {
			t.Fatalf("tenant-acme's list leaked a globex note: %+v", n)
		}
	}

	globexNotes := listNotesAs(t, srv, globexHost)
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

// TestBuildServer_Healthz_NoTenantRequired proves /healthz responds 200
// through the real composed server regardless of Host -- including a Host
// strictHostResolver cannot match to any configured tenant, which now
// genuinely fails resolution (see strictHostResolver's own doc comment in
// server.go) -- so a liveness probe never depends on tenant-specific
// resolution succeeding.
func TestBuildServer_Healthz_NoTenantRequired(t *testing.T) {
	srv := buildTestServer(t)

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
// Resolver's failure mode in general -- an unrecognized Host under
// strictHostResolver today (see server.go's own doc comment on it), an
// invalid or missing bearer token once authn supplies an authenticated
// Resolver tomorrow. Using a resolver that always fails, rather than
// driving buildServer's real strictHostResolver with an unrecognized Host,
// keeps this test about the allowlist mechanism in isolation, and keeps it
// valid unchanged once authn's Resolver replaces strictHostResolver here,
// since its failure conditions will look nothing like "Host not in a map".
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
// Allowlisting GET alone therefore looked fine under
// tenancy.NewDomainResolver, the Resolver this app wired up at the time
// (which never failed at all), while silently leaving HEAD one resolver
// swap away from a 403. buildServer now wires strictHostResolver instead
// (see its own doc comment in server.go), which genuinely does fail for
// an unrecognized Host -- exactly the swap this comment originally warned
// about -- so this canary matters for real production wiring today, not
// just hypothetically for a future authn-supplied Resolver.
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
// the real composed server regardless of Host -- including a Host
// strictHostResolver cannot match to any configured tenant -- mirroring
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
	srv := buildTestServer(t)

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
// obs.Init(DeploymentModeStandalone) itself first, so metricsHandler
// answers with a real Prometheus scrape (200) here, exactly as main.go's
// run() arranges before serving any production traffic (see main.go's
// run) -- reproducing, as a permanent automated test, exactly what manual
// verification of this gap found: with Init having actually run, both GET
// and HEAD /metrics return 200 regardless of Host/resolution outcome.
// Init's returned shutdown is registered via t.Cleanup so the
// package-level handler obs.MetricsHandler() returns is restored to its
// unavailable-by-default state before any other test in this binary runs
// -- the same discipline go/observability's own tests use to keep
// repeated Init calls independent.
func TestMetricsAllowlist_ResolutionFailure_StillReturns200(t *testing.T) {
	shutdown, err := obs.Init(context.Background(), pkgcore.DeploymentModeStandalone)
	if err != nil {
		t.Fatalf("obs.Init(DeploymentModeStandalone): %v", err)
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

// TestBuildServer_DistributedDeploymentMode_ReturnsClearError documents
// this example's current, honest limitation: only the standalone
// deployment mode is wired up here today (see buildServer's own doc
// comment and root CLAUDE.md's M0 status) -- requesting distributed must
// fail loudly, never silently fall back to SQLite under a "distributed"
// label.
func TestBuildServer_DistributedDeploymentMode_ReturnsClearError(t *testing.T) {
	cfg := testConfig(t)
	cfg.DeploymentMode = pkgcore.DeploymentModeDistributed

	_, _, err := buildServer(context.Background(), cfg)
	if err == nil {
		t.Fatal("buildServer with DeploymentModeDistributed: want error, got nil")
	}
}

// notesRequest issues method against /api/v1/notes with the given Host and
// optional body, returning the raw response for the caller to assert on --
// unlike createNoteAs/listNotesAs above, which assert success internally,
// this is for tests that expect the request to be rejected.
func notesRequest(t *testing.T, srv *httptest.Server, method, host string, body io.Reader) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, srv.URL+"/api/v1/notes", body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Host = host
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s /api/v1/notes (Host=%s): %v", method, host, err)
	}
	return resp
}

// TestBuildServer_UnrecognizedHost_FailsClosed is the regression test for
// the medium-severity gap round 3's smoke test found by live attack
// against a real, running `go run ./cmd/server`: an unrecognized (or
// entirely absent) Host resolved to a shared demoDefaultTenant bucket that
// any anonymous caller could read from and write to, instead of failing
// the request. tenancy.DomainResolver's own doc comment scopes its
// default-tenant fallback to unauthenticated, pre-auth display decisions
// ("rendering the right brand on a login page... it grants no data
// access"); this app's notes API is real, persisted CRUD data, so
// buildServer must fail closed on an unrecognized Host instead -- see
// strictHostResolver's doc comment in server.go.
//
// Before the fix this test failed exactly as the live attack did: step 1
// (GET) returned 200 with an empty list and step 2 (POST) returned 201,
// both served out of the shared demoDefaultTenant bucket. Step 3 is the
// negative control the live attack also ran: a second, different
// unrecognized Host must be rejected too, not see whatever step 2 would
// have planted -- proving there is no shared bucket left at all, not just
// that this one caller happened to be turned away.
func TestBuildServer_UnrecognizedHost_FailsClosed(t *testing.T) {
	srv := buildTestServer(t)

	const attackerHostA = "totally-unknown-attacker-host.example"
	const attackerHostB = "another-completely-different-unknown-host.example"

	// Step 1: GET must not succeed against an implicit shared tenant.
	getResp := notesRequest(t, srv, http.MethodGet, attackerHostA, nil)
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusForbidden {
		respBody, _ := io.ReadAll(getResp.Body)
		t.Fatalf("GET /api/v1/notes (Host=%s) status = %d, want %d; body = %s",
			attackerHostA, getResp.StatusCode, http.StatusForbidden, respBody)
	}

	// Step 2: POST must not plant a note in a shared bucket either.
	createBody, err := json.Marshal(map[string]string{"text": "planted via unrecognized host A"})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	postResp := notesRequest(t, srv, http.MethodPost, attackerHostA, strings.NewReader(string(createBody)))
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusForbidden {
		respBody, _ := io.ReadAll(postResp.Body)
		t.Fatalf("POST /api/v1/notes (Host=%s) status = %d, want %d; body = %s",
			attackerHostA, postResp.StatusCode, http.StatusForbidden, respBody)
	}

	// Step 3 (negative control): a completely different unrecognized Host
	// is independently rejected too -- there is no shared bucket left for
	// one anonymous caller to plant data into and another to read back,
	// which is exactly what made the original gap a real leak rather than
	// a per-caller-isolated 200.
	getResp2 := notesRequest(t, srv, http.MethodGet, attackerHostB, nil)
	defer getResp2.Body.Close()
	if getResp2.StatusCode != http.StatusForbidden {
		respBody, _ := io.ReadAll(getResp2.Body)
		t.Fatalf("GET /api/v1/notes (Host=%s) status = %d, want %d; body = %s",
			attackerHostB, getResp2.StatusCode, http.StatusForbidden, respBody)
	}

	// The live attack additionally confirmed a request with NO Host header
	// at all (a raw HTTP/1.0 request omitting it entirely) shares the same
	// fate. srv.Client() can't reproduce a genuinely absent Host -- the
	// real transport falls back to the request URL's host whenever
	// (*http.Request).Host is empty -- so this drives buildServer's real
	// composed handler directly instead, the same way
	// TestHealthzAllowlist_ResolutionFailure_StillReturns200 drives a
	// handler directly for its own synthetic resolver. httptest.NewRequest
	// defaults Host to "example.com" when none is given; clearing it back
	// to "" here reproduces a request that arrived with no Host header at
	// all, exactly as a raw HTTP/1.0 request can.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil)
	req.Host = ""
	rec := httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /api/v1/notes with no Host header at all: status = %d, want %d (body: %s)",
			rec.Code, http.StatusForbidden, rec.Body.String())
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
}

// TestConfigFromEnv_ReadsOverrides verifies each environment variable
// configFromEnv reads is actually honored.
func TestConfigFromEnv_ReadsOverrides(t *testing.T) {
	t.Setenv("SPEED_DEPLOYMENT_MODE", string(pkgcore.DeploymentModeDistributed))
	t.Setenv("PORT", "9999")
	t.Setenv("SPEED_DB_PATH", "/tmp/reference-app-configfromenv-test.db")
	t.Setenv("SPEED_CONFIG_KEY", "0f0e0d0c0b0a090807060504030201001f1e1d1c1b1a19181716151413121110")

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
// permanent counterpart to round 3's smoke test, which additionally
// attacked the real running server (`go run -race ./cmd/server`) with a
// forged "X-Tenant-ID" header, a "?tenant_id=" query parameter, and a
// "tenant_id" field smuggled into the JSON create body -- every one set
// alongside a Host that legitimately resolves to a DIFFERENT tenant than
// the one being forged -- and reported PASS: every attempt was silently
// ignored, confirmed under single requests and under 400 concurrent
// racing create/list pairs with the race detector attached.
//
// go/tenancy/middleware_test.go's
// TestMiddleware_IgnoresClientSuppliedTenantHints already proves the same
// property at the unit level, against a stub Resolver with no real
// Host-based lookup or persistence behind it. This test proves it again
// through the actual composed stack this app serves -- strictHostResolver,
// the notes Handler, and a real dbkit.Repository[Note] backed by SQLite --
// which is the level round 3's live attack actually targeted. Host,
// resolved by strictHostResolver, is the only tenant source the composed
// server ever trusts: see strictHostResolver's own doc comment in server.go
// and go/tenancy/resolver.go's Resolver doc comment for the same rule
// stated as a hard requirement on every implementation.
func TestBuildServer_ClientSuppliedTenantHints_Ignored(t *testing.T) {
	srv := buildTestServer(t)

	const acmeHost = "acme.demo.localhost"
	const globexHost = "globex.demo.localhost"

	createNoteAs(t, srv, acmeHost, "ACME-SECRET-forgery-target")

	// Attempt 1: a forged X-Tenant-ID header claiming tenant-acme, sent
	// alongside Host=globex, must not surface acme's note.
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/notes", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Host = globexHost
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
		t.Fatalf("GET (Host=%s, forged header X-Tenant-ID: tenant-acme) leaked %d note(s): %+v",
			globexHost, len(headerAttempt.Notes), headerAttempt.Notes)
	}

	// Attempt 2: the same forged tenant, this time as a "?tenant_id="
	// query parameter instead of a header.
	req2, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/notes?tenant_id=tenant-acme", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req2.Host = globexHost
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
		t.Fatalf("GET (Host=%s, ?tenant_id=tenant-acme) leaked %d note(s): %+v",
			globexHost, len(queryAttempt.Notes), queryAttempt.Notes)
	}

	// Attempt 3: "tenant_id" smuggled into the JSON create body. It must
	// be silently ignored -- the handler decodes into the spec-generated
	// api.NotesCreateNoteRequest (internal/notes/api, derived from the
	// module's api/openapi.yaml fragment), which carries no tenant_id
	// field to decode it into -- and the created note must land under
	// globex (Host's tenant), never acme.
	forgeBody, err := json.Marshal(map[string]string{"text": "globex-body-forge-probe", "tenant_id": "tenant-acme"})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	postResp := notesRequest(t, srv, http.MethodPost, globexHost, strings.NewReader(string(forgeBody)))
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(postResp.Body)
		t.Fatalf("POST (Host=%s, tenant_id forged in body) status = %d, want %d; body = %s",
			globexHost, postResp.StatusCode, http.StatusCreated, respBody)
	}

	// Negative control, symmetric with TestBuildServer_MultiTenantIsolation_EndToEnd
	// above: acme must still see exactly its own note (none of the three
	// forgery attempts sourced from Host=globex ever reached it), and
	// globex must see exactly the note attempt 3 planted under globex --
	// proving attempt 3 actually ran, not merely that it returned 201 --
	// with neither tenant's list containing the other's data.
	acmeNotes := listNotesAs(t, srv, acmeHost)
	if len(acmeNotes) != 1 || acmeNotes[0].Text != "ACME-SECRET-forgery-target" {
		t.Fatalf("acme notes after all forgery attempts = %+v, want exactly one note with text %q",
			acmeNotes, "ACME-SECRET-forgery-target")
	}
	globexNotes := listNotesAs(t, srv, globexHost)
	if len(globexNotes) != 1 || globexNotes[0].Text != "globex-body-forge-probe" {
		t.Fatalf("globex notes after all forgery attempts = %+v, want exactly one note with text %q",
			globexNotes, "globex-body-forge-probe")
	}
}
