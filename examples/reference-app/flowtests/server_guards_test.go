package flowtests

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/app"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/config"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
)

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
			req, err := http.NewRequest(method, srv.URL+obs.HealthzPath, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Host = host

			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("%s %s (Host=%q): %v", method, obs.HealthzPath, host, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s %s (Host=%q) status = %d, want 200", method, obs.HealthzPath, host, resp.StatusCode)
			}
		}
	}
}

// failingResolver deliberately fails every resolution, standing in for any
// Resolver's failure mode in general -- an invalid or missing bearer token
// under authn.NewPrincipalResolver (internal/app/server.go's middleware-chain doc
// comment). Using a resolver that always fails, rather than driving
// BuildServer's real composed chain with a missing/invalid token, keeps
// this test about the allowlist mechanism in isolation.
type failingResolver struct{}

func (failingResolver) Resolve(r *http.Request) (pkgcore.TenantID, error) {
	return "", errors.New("test: deliberately failing resolver")
}

func TestHealthzAllowlist_ResolutionFailure_StillReturns200(t *testing.T) {
	mux := http.NewServeMux()
	obs.MountLiveness(mux)

	// The same construction BuildServer uses: tenancy.Middleware wrapping
	// the mux, allowlisting both GET and HEAD for HealthzPath -- see
	// internal/app/server.go's own comment on why HEAD needs its own entry too.
	handler := tenancy.Middleware(failingResolver{},
		tenancy.WithAllowlist(http.MethodGet, obs.HealthzPath),
		tenancy.WithAllowlist(http.MethodHead, obs.HealthzPath),
	)(mux)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req := httptest.NewRequest(method, obs.HealthzPath, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s with a failing resolver: status = %d, want 200 (body: %s)", method, obs.HealthzPath, rec.Code, rec.Body.String())
		}
	}

	// The body is checked separately from the status-code loop above:
	// net/http's own HEAD handling correctly omits the response body (per
	// RFC 9110) even though the handler wrote one, so asserting on it only
	// for the GET request keeps this test honest about what HEAD actually
	// guarantees.
	req := httptest.NewRequest(http.MethodGet, obs.HealthzPath, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Body.String() != "ok" {
		t.Fatalf("GET %s body = %q, want %q", obs.HealthzPath, rec.Body.String(), "ok")
	}

	// Sanity check, proving the allowlist -- not general leniency in
	// obs.HealthzHandler or the mux -- is what let the requests above
	// through: the identical failing resolver still fails closed (403) for
	// a path that was never allowlisted.
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
// automatically serves HEAD HealthzPath from a registered "GET
// "+HealthzPath pattern (Go's long-standing GET-implies-HEAD convenience),
// but tenancy.Middleware does NOT extend WithAllowlist's (method, path)
// exemption the same way -- its own doc comment says so explicitly:
// "allowlist http.MethodHead explicitly if a health check needs it too."
// Allowlisting GET alone therefore looks fine under a resolver that never
// fails, while silently leaving HEAD one resolver failure away from a 403
// -- exactly what authn.NewPrincipalResolver does fail with whenever
// no Principal is present (internal/app/server.go's middleware-chain doc comment).
//
// This test reproduces exactly that gap (deliberately allowlisting GET
// only, unlike BuildServer's real wiring) as a permanent canary: if it
// ever starts failing -- HEAD suddenly returning 200 -- either net/http's
// or tenancy.Middleware's GET/HEAD behavior changed, and internal/app/server.go's
// BuildServer may no longer need its explicit HEAD allowlist entry.
func TestHealthzAllowlist_GETOnlyAllowlist_LeavesHEADExposed(t *testing.T) {
	mux := http.NewServeMux()
	obs.MountLiveness(mux)

	handler := tenancy.Middleware(failingResolver{}, tenancy.WithAllowlist(http.MethodGet, obs.HealthzPath))(mux)

	req := httptest.NewRequest(http.MethodHead, obs.HealthzPath, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("HEAD %s with only GET allowlisted and a failing resolver: status = %d, want 403", obs.HealthzPath, rec.Code)
	}
}

// TestBuildServer_Metrics_NoTenantRequired proves /metrics responds through
// the real composed server regardless of Host, mirroring
// TestBuildServer_Healthz_NoTenantRequired above for the other route
// BuildServer allowlists (see obs.MetricsPath's and obs.MountLiveness's own
// doc comments: a scraper, like a liveness probe, has no demo Host to send
// and must not depend on one).
//
// Unlike the healthz version, this does not assert on a literal 200: the
// mounted metrics route serves whatever obs.MetricsHandler() currently
// returns, and -- like every other test that drives BuildServer directly in
// this file -- this test never calls obs.Init, so MetricsHandler answers
// its documented "before Init has run" 404 here, not a real scrape (see
// MetricsHandler's own doc comment in go/observability/init.go). The
// property this test level can honestly verify is narrower, but is the one
// actually in question here: tenancy.Middleware's allowlist let the
// request through to the metrics route at all, for every Host, instead of
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
			req, err := http.NewRequest(method, srv.URL+obs.MetricsPath, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Host = host

			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("%s %s (Host=%q): %v", method, obs.MetricsPath, host, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusForbidden {
				t.Fatalf("%s %s (Host=%q) status = %d, want anything but 403 (tenant resolution must not be required for this route)",
					method, obs.MetricsPath, host, resp.StatusCode)
			}
		}
	}
}

// TestMetricsAllowlist_ResolutionFailure_StillReturns200 is MetricsPath's
// counterpart to TestHealthzAllowlist_ResolutionFailure_StillReturns200
// above, proving the same property tenancy.WithAllowlist gives /healthz --
// the route stays reachable even when the Resolver fails outright -- for
// the other route BuildServer allowlists.
//
// Unlike TestBuildServer_Metrics_NoTenantRequired above, this test calls
// obs.Init() itself first -- no deployment mode argument; Init's
// no-endpoint path wires the local exporters, which is exactly the
// wiring main.go's run() arranges before serving any production traffic
// -- so MetricsHandler answers with a real Prometheus scrape (200)
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
	obs.MountLiveness(mux)

	// The same construction BuildServer uses: tenancy.Middleware wrapping
	// the mux, allowlisting both GET and HEAD for MetricsPath -- see
	// internal/app/server.go's own comment on why HEAD needs its own entry too.
	handler := tenancy.Middleware(failingResolver{},
		tenancy.WithAllowlist(http.MethodGet, obs.MetricsPath),
		tenancy.WithAllowlist(http.MethodHead, obs.MetricsPath),
	)(mux)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req := httptest.NewRequest(method, obs.MetricsPath, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s with a failing resolver: status = %d, want 200 (body: %s)", method, obs.MetricsPath, rec.Code, rec.Body.String())
		}
	}

	// Sanity check, proving the allowlist -- not general leniency in
	// MetricsHandler or the mux -- is what let the requests above through:
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

// TestMetricsAllowlist_GETOnlyAllowlist_LeavesHEADExposed is MetricsPath's
// counterpart to TestHealthzAllowlist_GETOnlyAllowlist_LeavesHEADExposed
// above. BuildServer allowlists MetricsPath the same two-calls-one-per-
// method way it allowlists HealthzPath (internal/app/server.go), so it carries the exact
// same regression risk: net/http's ServeMux auto-serves HEAD from the
// registered "GET "+MetricsPath pattern, but tenancy.Middleware does not
// extend WithAllowlist's exemption from GET to HEAD automatically (its own
// doc comment says so explicitly) -- so forgetting, or later deleting, the
// tenancy.WithAllowlist(http.MethodHead, MetricsPath) call in BuildServer
// would silently leave HEAD /metrics one resolver failure away from a 403.
//
// This test reproduces that gap deliberately (GET allowlisted only) as a
// permanent canary: if it ever starts failing -- HEAD suddenly returning
// 200 -- either net/http's or tenancy.Middleware's GET/HEAD behavior
// changed, or BuildServer may no longer need its explicit HEAD allowlist
// entry for MetricsPath.
func TestMetricsAllowlist_GETOnlyAllowlist_LeavesHEADExposed(t *testing.T) {
	mux := http.NewServeMux()
	obs.MountLiveness(mux)

	handler := tenancy.Middleware(failingResolver{}, tenancy.WithAllowlist(http.MethodGet, obs.MetricsPath))(mux)

	req := httptest.NewRequest(http.MethodHead, obs.MetricsPath, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("HEAD %s with only GET allowlisted and a failing resolver: status = %d, want 403", obs.MetricsPath, rec.Code)
	}
}

// fakeSMSGatewayURL is a never-dialed HTTP endpoint used by every
// distributed-mode test below that needs authn's own wiring-time "SMS
// sender" validation to pass (so the Kernel-level assertion the test
// actually pins is what runs) without touching a network:
// pkgcore.NewHTTPSMSSender's own construction never dials anything, and
// none of these tests exercises the phone-login flow that would actually
// POST to it.
const fakeSMSGatewayURL = "http://127.0.0.1:1/sms"

// TestBuildServer_DistributedDeploymentMode_FailsCapabilityValidation pins
// what requesting the distributed deployment mode means: the composition
// is not rejected up front, it is validated -- and with every seam
// resolved from the Preset, the distributed mode's required capabilities
// cannot be met. Kernel.Bootstrap must fail with ErrCapabilityUnsatisfied,
// naming the first shortfall: the "eventbus" seam's in-process memory
// implementation lacking MultiReplicaSafe while the mode is "distributed".
// Bootstrap performs that validation before any Subscribe or goroutine
// starts, so this test needs no Docker and never touches a network, and it
// guarantees the mode can never silently degrade into a
// SQLite-and-in-memory run under a "distributed" label.
//
// The event bus is constructed by BuildServer itself
// -- before dbkit.Open, so the automatic org audit capture can publish on
// the same bus the Kernel later resolves (see the Open call's own comment)
// -- and injected through WithEventBus in the standalone as well as the
// Redis composition. A distributed boot without cfg.RedisAddr therefore
// fails on the INJECTED memory bus, which pkgcore's capability error names
// as implementation "<injected>" (an injected seam has no registry name to
// report -- the host knows what it injected); the seam, capability and
// mode naming is unchanged, and that is the part that proves the mode can
// never silently degrade.
//
// cfg.SMSGatewayURL is set to fakeSMSGatewayURL so that authn's own
// wiring-time SMS-sender validation, which BuildServer reaches BEFORE
// Kernel.Bootstrap (see BuildServer's authn wiring comment), does not mask
// the Kernel-level failure this test actually pins;
// TestBuildServer_DistributedDeploymentMode_NoSMSGateway_FailsClosed below
// is what proves that earlier validation on its own.
func TestBuildServer_DistributedDeploymentMode_FailsCapabilityValidation(t *testing.T) {
	cfg := testConfig(t)
	cfg.DeploymentMode = pkgcore.DeploymentModeDistributed
	cfg.SMSGatewayURL = fakeSMSGatewayURL

	_, _, _, err := app.BuildServer(context.Background(), cfg)
	if err == nil {
		t.Fatal("BuildServer with DeploymentModeDistributed: want error, got nil")
	}
	if !errors.Is(err, pkgcore.ErrCapabilityUnsatisfied) {
		t.Fatalf("BuildServer with DeploymentModeDistributed: error = %v, want errors.Is(err, pkgcore.ErrCapabilityUnsatisfied)", err)
	}
	for _, want := range []string{`seam "eventbus"`, `"<injected>"`, "MultiReplicaSafe", `"distributed"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("BuildServer with DeploymentModeDistributed: error %q does not mention %s", err, want)
		}
	}
}

// TestBuildServer_DistributedDeploymentMode_RedisConfigured_StillFailsOnMailer
// is the second half of the distributed-mode pin under the env-driven
// wiring: APP_REDIS_ADDR composes BOTH the
// "eventbus" and the "kv" seam onto one shared *redis.Client (BuildServer's
// kernel-options doc comment explains why one Redis instance backs both),
// so a distributed deployment with only cfg.RedisAddr set clears both
// "eventbus" and "kv" and fails capability validation on the NEXT seam
// Kernel.Bootstrap resolves: "mailer", whose Preset default
// ("mailer.console") also lacks MultiReplicaSafe, and this test configures
// no APP_SMTP_* composition to swap it for. This is the "one
// seam wired isn't enough" property of the distributed
// mode, demonstrated at the mailer seam. Validation precedes module registration, so no Subscribe is ever
// reached, and the cleanup BuildServer runs on this error path is equally
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

	_, _, _, err := app.BuildServer(context.Background(), cfg)
	if err == nil {
		t.Fatal("BuildServer with DeploymentModeDistributed and Redis configured: want error, got nil")
	}
	if !errors.Is(err, pkgcore.ErrCapabilityUnsatisfied) {
		t.Fatalf("BuildServer with DeploymentModeDistributed and Redis configured: error = %v, want errors.Is(err, pkgcore.ErrCapabilityUnsatisfied)", err)
	}
	for _, want := range []string{`seam "mailer"`, `"mailer.console"`, "MultiReplicaSafe", `"distributed"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("BuildServer with DeploymentModeDistributed and Redis configured: error %q does not mention %s", err, want)
		}
	}
}

// TestBuildServer_DistributedDeploymentMode_NoSMSGateway_FailsClosed proves
// the negative half of the authn "SMS sender" wiring: a
// distributed composition that forgets APP_SMS_GATEWAY_URL must fail
// closed with authn.ErrMissingDistributedSMSSender, rather than silently
// keeping the console transport nobody in a distributed replica pool is
// reading. Passing WithSMSSender(NewConsoleSMSSender(...))
// unconditionally regardless of deployment mode would hide that gap.
// This is authn's OWN wiring-time validation (authn.NewModule's
// newOptions), which BuildServer reaches before it ever calls
// pkgcore.NewKernel(...).Bootstrap -- so this failure fires regardless of
// whether any other seam (Redis, S3, SMTP) is configured, and this test
// configures none of them, needing no Docker and touching no network.
func TestBuildServer_DistributedDeploymentMode_NoSMSGateway_FailsClosed(t *testing.T) {
	cfg := testConfig(t)
	cfg.DeploymentMode = pkgcore.DeploymentModeDistributed

	_, _, _, err := app.BuildServer(context.Background(), cfg)
	if err == nil {
		t.Fatal("BuildServer with DeploymentModeDistributed and no APP_SMS_GATEWAY_URL: want error, got nil")
	}
	if !errors.Is(err, authn.ErrMissingDistributedSMSSender) {
		t.Fatalf("BuildServer with DeploymentModeDistributed and no APP_SMS_GATEWAY_URL: error = %v, want errors.Is(err, authn.ErrMissingDistributedSMSSender)", err)
	}
}

// TestBuildServer_SMTPAndS3Compositions_ResolveThroughThePresetChannel pins
// the channel this app routes its two string-expressible seam compositions
// through -- the APP_SMTP_* and APP_S3_* groups -- by reading
// Kernel.Bootstrap's own startup composition line. With those groups
// configured, the "mailer" seam must be named as the registered
// "mailer.smtp" implementation it resolves to and the "objectstore" seam as
// "objectstore.s3", not as "<injected>" (a host-built value handed over with
// WithMailer/WithObjectStore). A regression back to constructing
// pkgcore.NewSMTPMailer / objectstore/s3.NewObjectStore in this host would
// boot identically and name "<injected>" for both, so the line is the
// discriminator between composing through the channel and pre-building; the
// same boot must keep naming the eventbus as "<injected>" and the kv seam as
// its in-process default, since those two deliberately stay on the injection
// path.
//
// The slog default logger is process-global, and nothing in this package
// calls t.Parallel, so the swap cannot overlap another test's boot (the same
// ground pkgcore's own TestBootstrap_LogsOneInfoLineNamingEveryResolvedSeam
// stands on). Neither composition dials anything at boot: mailer.smtp dials
// per message and the S3 client is constructed lazily, so this test needs no
// Docker and touches no network.
func TestBuildServer_SMTPAndS3Compositions_ResolveThroughThePresetChannel(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(previous)

	cfg := testConfig(t)
	cfg.SMTPHost = "smtp.example.test"
	cfg.SMTPPort = 587
	cfg.SMTPUsername = "mailer@example.test"
	cfg.SMTPPassword = "smtp-password"
	cfg.S3Endpoint = "objects.example.test:9000"
	cfg.S3Bucket = "objects"
	cfg.S3AccessKey = "access-key"
	cfg.S3SecretKey = "secret-key"

	_, cleanup, _, err := app.BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildServer with SMTP and S3 compositions: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	out := buf.String()
	for _, want := range []string{
		"mailer=mailer.smtp",
		"objectstore=objectstore.s3",
		"eventbus=<injected>",
		"kv=kv.memory",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("bootstrap logged %q, want the composition line to name %s", out, want)
		}
	}
	if strings.Contains(out, "mailer=<injected>") || strings.Contains(out, "objectstore=<injected>") {
		t.Errorf("bootstrap logged %q, want neither the mailer nor the objectstore seam injected", out)
	}
}
