package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	obs "github.com/vislake/speed/go/observability"
)

// TestObsRouteSeed_RealRoutesSurviveStartupGarbage is the consumer-side
// regression for go/observability's route-label pre-seed API (P1-obs-8):
// obs.RegisterMountedRoutes exists precisely so a host can hand the route
// label limiter every obs.Middleware constructs its REAL route table
// before any request traffic arrives, and this app is the mandatory first
// consumer of that API -- buildServer's wiring below (server.go) calls
// it with the module route table reg.Routes.Routes() holds plus the two
// host-level routes mounted directly on the mux. Without that call, the
// limiter's distinct-value budget (obs.MaxRouteLabelValues) is
// first-come-first-served: an attacker sending that many distinct garbage
// paths right after startup fills the budget, and every genuine route
// first requested afterwards -- /api/v1/notes and /healthz included -- is
// recorded under obs.RouteLabelOverflowValue for the life of the process,
// per-route metrics gone even though no bound was violated.
//
// The test drives the exact composed stack main.go's run serves --
// buildServer's authn+tenancy chain wrapped in obs.Middleware behind a
// real HTTP server, after obs.Init arms the local /metrics scrape -- and
// replays the attack window: obs.MaxRouteLabelValues+100 distinct
// unauthenticated garbage paths (each a normal pre-auth 403, the shape
// the middleware's own doc comment names as the exploit's vehicle) are
// sent FIRST, before any real route is requested. Only then are a real
// route (/healthz, allowlisted, answering 200 with no tenant) and a real
// module surface (GET /api/v1/notes, answering the app's normal
// fail-closed 403 for an anonymous caller) requested, and the /metrics
// scrape is asserted to carry each under its OWN http.route label, never
// the overflow bucket -- which is asserted to exist with the garbage it
// was created for, proving the budget really was exhausted (without
// which the test could pass vacuously: a flood that failed to fill the
// budget would let an unseeded /healthz claim a slot of its own).
//
// Fails before the wiring lands (verified on the unwired tree): with no
// registration, /healthz and /api/v1/notes are first requested only after
// the budget is exhausted, so no series labeled with either path exists
// in the scrape at all. Passes after: the seeded slots keep the real
// routes' series whatever garbage arrived first, exactly as
// go/observability/middleware_test.go's
// TestMiddleware_RealRoutesSurviveGarbage_WhenSeeded proves for the
// mechanism itself.
func TestObsRouteSeed_RealRoutesSurviveStartupGarbage(t *testing.T) {
	cfg := testConfig(t)
	handler, cleanup, _, err := buildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			t.Errorf("cleanup: %v", cleanupErr)
		}
	})

	// obs.Init arms the local pull-based metrics reader the /metrics
	// route serves; the init() side effect registering it
	// (go/observability/exporter/prometheus) is already in this binary via
	// server.go's blank import. Must run before obs.Middleware below is
	// constructed, so the instruments the middleware builds bind to THIS
	// test's MeterProvider and this test's scrape can see them -- the same
	// buildServer -> Init -> Middleware order main.go's run uses.
	shutdown, err := obs.Init(context.Background())
	if err != nil {
		t.Fatalf("obs.Init: %v", err)
	}
	t.Cleanup(func() {
		if shutdownErr := shutdown(context.Background()); shutdownErr != nil {
			t.Errorf("obs.Init shutdown: %v", shutdownErr)
		}
	})

	// obs.Middleware snapshots the routes buildServer registered (the call
	// that is this test's subject) at construction -- a registration made
	// after this line would not reach this handler, mirroring
	// RegisterMountedRoutes' own "register before constructing the
	// Middleware that serves the traffic" contract.
	instrumented := obs.Middleware(handler)
	srv := httptest.NewServer(instrumented)
	t.Cleanup(srv.Close)
	client := srv.Client()

	// The attacker window: right after startup, before any real route has
	// been requested, MaxRouteLabelValues+100 distinct garbage paths. Each
	// answers the normal pre-auth 403 (tenancy.Middleware's fail-closed
	// refusal -- the allowlist names none of them), which is exactly the
	// traffic shape the route limiter's doc comment identifies as the
	// exploit's vehicle: unauthenticated requests that reach the
	// middleware's metric-recording code with attacker-chosen paths.
	const garbagePaths = obs.MaxRouteLabelValues + 100
	for i := 0; i < garbagePaths; i++ {
		resp, getErr := client.Get(srv.URL + fmt.Sprintf("/attacker-garbage-path-%d", i))
		if getErr != nil {
			t.Fatalf("garbage path %d: %v", i, getErr)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if i == 0 || i == garbagePaths-1 {
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("garbage path %d status = %d, want the pre-auth %d (tenancy fail-closed); a different answer means the attack traffic did not take the path this test models",
					i, resp.StatusCode, http.StatusForbidden)
			}
		}
	}

	// The real routes, first requested only after the budget is exhausted:
	// /healthz is the allowlisted host-level route that must answer 200 to
	// an orchestrator with no tenant at all, and GET /api/v1/notes is a
	// real module surface whose normal anonymous answer is the app's
	// fail-closed 403 (server_test.go's TestBuildServer_Unauthenticated_
	// FailsClosed pins it). Both must still be recorded under their own
	// route labels, not the overflow bucket.
	healthzResp, err := client.Get(srv.URL + healthzPath)
	if err != nil {
		t.Fatalf("GET %s after the garbage flood: %v", healthzPath, err)
	}
	healthzBody, _ := io.ReadAll(healthzResp.Body)
	_ = healthzResp.Body.Close()
	if healthzResp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s after the garbage flood: status = %d, want %d (body: %s)",
			healthzPath, healthzResp.StatusCode, http.StatusOK, healthzBody)
	}

	const notesListPath = "/api/v1/notes"
	notesResp, err := client.Get(srv.URL + notesListPath)
	if err != nil {
		t.Fatalf("GET %s after the garbage flood: %v", notesListPath, err)
	}
	_, _ = io.Copy(io.Discard, notesResp.Body)
	_ = notesResp.Body.Close()
	if notesResp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET %s after the garbage flood: status = %d, want the anonymous caller's normal %d",
			notesListPath, notesResp.StatusCode, http.StatusForbidden)
	}

	metricsResp, err := client.Get(srv.URL + metricsPath)
	if err != nil {
		t.Fatalf("GET %s: %v", metricsPath, err)
	}
	scrapeBody, _ := io.ReadAll(metricsResp.Body)
	_ = metricsResp.Body.Close()
	if metricsResp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want %d (body: %s)", metricsPath, metricsResp.StatusCode, http.StatusOK, scrapeBody)
	}
	series := parseRequestCountSeries(t, scrapeBody)

	// /healthz answered normally (200, asserted above) AND kept its own
	// series: one request, under its own label, not the overflow bucket.
	healthzSeries, ok := series[healthzPath]
	if !ok {
		t.Fatalf("no http.server.request.count series labeled http.route=%q in the scrape after %d garbage paths: the real route collapsed to %q (the unseeded limiter's first-come-first-served budget was exhausted before this route was ever requested). Series present: %v",
			healthzPath, garbagePaths, obs.RouteLabelOverflowValue, series)
	}
	if got := healthzSeries["200"]; got != 1 {
		t.Fatalf("http.route=%q recorded %d successful requests, want exactly 1 (the one GET %s above); series: %v",
			healthzPath, got, healthzPath, healthzSeries)
	}

	// The module-table half of the seed: /api/v1/notes is registered by
	// the notes module on the pkgcore registry (not by this app's own
	// host-level mount), so its survival proves the reg.Routes.Routes()
	// half of the registration reached the limiter too.
	notesSeries, ok := series[notesListPath]
	if !ok {
		t.Fatalf("no http.server.request.count series labeled http.route=%q in the scrape after %d garbage paths: the real module route collapsed to %q. Series present: %v",
			notesListPath, garbagePaths, obs.RouteLabelOverflowValue, series)
	}
	if got := notesSeries["403"]; got != 1 {
		t.Fatalf("http.route=%q recorded %d anonymous refusals, want exactly 1 (the one GET %s above); series: %v",
			notesListPath, got, notesListPath, notesSeries)
	}

	// Test-integrity guard: the overflow bucket must exist and hold the
	// garbage it was created for. garbagePaths - obs.MaxRouteLabelValues
	// paths overflowed in every world (seeded or not -- the seed only
	// reserves the real routes' slots, it never grows the bound), so an
	// overflow at least that large proves the flood genuinely exhausted
	// the distinct-value budget; without this assertion the two checks
	// above could pass vacuously if the budget had never filled (an
	// unseeded real route would then have claimed a slot of its own).
	overflowCount := int64(0)
	for _, count := range series[obs.RouteLabelOverflowValue] {
		overflowCount += count
	}
	if overflowCount < garbagePaths-obs.MaxRouteLabelValues {
		t.Fatalf("overflow bucket recorded %d requests, want at least %d (garbagePaths - obs.MaxRouteLabelValues): the flood did not exhaust the distinct-value budget, so the checks above would not prove the seed did anything",
			overflowCount, garbagePaths-obs.MaxRouteLabelValues)
	}
}

// parseRequestCountSeries parses one /metrics scrape body's
// http_server_request_count_total family (the Prometheus-exporter form of
// go/observability's http.server.request.count counter -- its
// "_total"-suffixed family name pinned by exporter/prometheus' own test)
// into route -> status -> request count, so assertions can address a real
// route's series by its http.route label without depending on the
// exporter's label ordering. Lines outside the family are ignored (the
// HELP/TYPE headers, every other metric family the process emits).
func parseRequestCountSeries(t *testing.T, body []byte) map[string]map[string]int64 {
	t.Helper()
	series := map[string]map[string]int64{}
	for _, line := range strings.Split(string(body), "\n") {
		matches := requestCountFamilyLineRE.FindStringSubmatch(line)
		if matches == nil {
			continue
		}
		route, status := "", ""
		for _, labelMatch := range requestCountLabelRE.FindAllStringSubmatch(matches[1], -1) {
			switch labelMatch[1] {
			case "http_route":
				route = labelMatch[2]
			case "http_response_status_code":
				status = labelMatch[2]
			}
		}
		count, err := strconv.ParseFloat(matches[2], 64)
		if err != nil {
			t.Fatalf("parse request count value %q: %v", matches[2], err)
		}
		if series[route] == nil {
			series[route] = map[string]int64{}
		}
		series[route][status] += int64(count)
	}
	return series
}

// requestCountFamilyLineRE matches one data point of the
// http_server_request_count_total family: the label block between braces
// and the trailing count value. The label block is matched greedily to the
// LAST "} " before the count so a label VALUE containing braces -- the
// overflow bucket's own obs.RouteLabelOverflowValue, "{overflow}" -- does
// not truncate the match; the label values are Prometheus-exposition
// quoted strings and every value this app's routes produce is plain
// ASCII.
var requestCountFamilyLineRE = regexp.MustCompile(`^http_server_request_count_total\{(.*)\} ([-+0-9.eE]+)$`)

// requestCountLabelRE matches one quoted "key=\"value\"" pair inside the
// label block, with the standard backslash escapes tolerated so a value
// containing a quote or backslash cannot break the parse.
var requestCountLabelRE = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)="((?:[^"\\]|\\.)*)"`)
