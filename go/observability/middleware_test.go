package observability_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// The OTel instrument names middleware.go's Middleware records under.
// Mirrored here as literals -- rather than referencing the package's own
// unexported requestCountName / requestDurationName constants, which this
// external observability_test package cannot see -- matching how this
// file has always pinned the metric names it asserts on independently of
// middleware.go's own implementation constants.
const (
	requestCountMetricName    = "http.server.request.count"
	requestDurationMetricName = "http.server.request.duration"
)

// setupMeterProvider installs, as OTel's global MeterProvider for the
// duration of the test, a real SDK MeterProvider backed by a
// sdkmetric.ManualReader -- a pull-on-demand reader the OTel SDK itself
// provides, needing no exporter or third-party dependency of any kind
// (this file deliberately does not import a Prometheus exporter: doing so
// would defeat go/observability's own root-package/exporter-subpackage
// isolation this repository's depguard rules enforce -- see
// .golangci.yml's prometheus-only-in-observability-exporter-prometheus
// rule). It returns the reader to collect() from.
func setupMeterProvider(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	otel.SetMeterProvider(mp)
	return reader
}

// collect pulls a fresh snapshot from reader -- the ManualReader
// equivalent of a Prometheus Gather() call, since nothing pushes to a
// ManualReader on its own.
func collect(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	return rm
}

// findMetric returns the metric named name across every scope in rm,
// failing the test if none matches.
func findMetric(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()
	var got []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
			got = append(got, m.Name)
		}
	}
	t.Fatalf("metric %q not found; metrics present: %v", name, got)
	return metricdata.Metrics{}
}

// findSum type-asserts m.Data as a Sum[int64] (what Middleware's
// Int64Counter always produces), failing the test if m is not one.
func findSum(t *testing.T, m metricdata.Metrics) metricdata.Sum[int64] {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %q data = %T, want metricdata.Sum[int64]", m.Name, m.Data)
	}
	return sum
}

// findHistogram type-asserts m.Data as a Histogram[float64] (what
// Middleware's Float64Histogram always produces), failing the test if m
// is not one.
func findHistogram(t *testing.T, m metricdata.Metrics) metricdata.Histogram[float64] {
	t.Helper()
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("metric %q data = %T, want metricdata.Histogram[float64]", m.Name, m.Data)
	}
	return hist
}

// setupTracerProvider installs, as OTel's global TracerProvider for the
// duration of the test, a real SDK TracerProvider exporting to the SDK's
// own in-memory recorder (not a mock), and returns that recorder.
func setupTracerProvider(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)
	return exp
}

// labelMap flattens a data point's attribute set into a map. Unlike the
// Prometheus exporter this file used to gather through, a ManualReader's
// metricdata carries no exporter-specific bookkeeping labels (no
// otel_scope_* noise to filter out): every attribute here is one
// Middleware itself attached.
func labelMap(attrs attribute.Set) map[string]string {
	kvs := attrs.ToSlice()
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value.String()
	}
	return out
}

// assertNoTenantLabelAnywhere is the blanket negative control this whole
// file is built around: no metric Middleware feeds, under any
// circumstance, may carry a tenant_id attribute. This is checked
// independently of, and in addition to, the more specific series-count
// assertions below, so a future attribute this middleware grows cannot
// reintroduce tenant_id through a code path the more targeted assertions
// do not happen to cover.
func assertNoTenantLabelAnywhere(t *testing.T, rm metricdata.ResourceMetrics) {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			var dataPointAttrs []attribute.Set
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					dataPointAttrs = append(dataPointAttrs, dp.Attributes)
				}
			case metricdata.Histogram[float64]:
				for _, dp := range data.DataPoints {
					dataPointAttrs = append(dataPointAttrs, dp.Attributes)
				}
			}
			for _, attrs := range dataPointAttrs {
				if v, ok := attrs.Value(attribute.Key(obs.TenantIDKey)); ok {
					t.Fatalf("metric %q carries a %q attribute (value %q): tenant_id must never be a metric label (CLAUDE.md, docs/internal/09-observability.md)",
						m.Name, obs.TenantIDKey, v.String())
				}
			}
		}
	}
}

func newTestRequest(method, path string, tenant pkgcore.TenantID) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if tenant != "" {
		req = req.WithContext(pkgcore.WithTenant(req.Context(), tenant))
	}
	return req
}

func TestMiddleware_RecordsRequestCountAndDuration(t *testing.T) {
	reader := setupMeterProvider(t)
	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, "/api/v1/notes", ""))

	rm := collect(t, reader)
	assertNoTenantLabelAnywhere(t, rm)

	counter := findSum(t, findMetric(t, rm, requestCountMetricName))
	if got := len(counter.DataPoints); got != 1 {
		t.Fatalf("expected exactly 1 counter series, got %d", got)
	}
	wantLabels := map[string]string{
		"http.request.method":       "GET",
		"http.route":                "/api/v1/notes",
		"http.response.status_code": "200",
	}
	if got := labelMap(counter.DataPoints[0].Attributes); !mapsEqual(got, wantLabels) {
		t.Errorf("counter labels = %v, want %v", got, wantLabels)
	}
	if got := counter.DataPoints[0].Value; got != 1 {
		t.Errorf("counter value = %v, want 1", got)
	}

	duration := findHistogram(t, findMetric(t, rm, requestDurationMetricName))
	if got := len(duration.DataPoints); got != 1 {
		t.Fatalf("expected exactly 1 duration series, got %d", got)
	}
	if got := duration.DataPoints[0].Count; got != 1 {
		t.Errorf("duration sample count = %v, want 1", got)
	}
}

// TestMiddleware_MetricsExcludeTenant_NoPerTenantSeries is the test the
// package doc comment's "tenant_id is not a metric label" section promises:
// a real negative control, not just an assertion described in a comment.
// Two requests, differing ONLY in which tenant issued them, must produce
// metrics that (a) carry no tenant_id label at all and (b) collapse into
// exactly one series rather than forking into one per tenant. Either
// failure mode reproduces the exact cardinality incident
// docs/internal/09-observability.md warns about: a few thousand tenants
// multiplying every HTTP metric series by a few thousand.
func TestMiddleware_MetricsExcludeTenant_NoPerTenantSeries(t *testing.T) {
	reader := setupMeterProvider(t)
	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, tenant := range []pkgcore.TenantID{"tenant-a", "tenant-b"} {
		handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, "/api/v1/notes", tenant))
	}

	rm := collect(t, reader)
	assertNoTenantLabelAnywhere(t, rm)

	counter := findSum(t, findMetric(t, rm, requestCountMetricName))
	if got := len(counter.DataPoints); got != 1 {
		t.Fatalf("expected the two tenants' requests to collapse into exactly 1 series, got %d series: %v",
			got, counter.DataPoints)
	}
	if got := counter.DataPoints[0].Value; got != 2 {
		t.Errorf("expected the single series to aggregate both tenants' requests (value 2), got %v", got)
	}

	duration := findHistogram(t, findMetric(t, rm, requestDurationMetricName))
	if got := len(duration.DataPoints); got != 1 {
		t.Fatalf("expected the two tenants' requests to collapse into exactly 1 duration series, got %d", got)
	}
	if got := duration.DataPoints[0].Count; got != 2 {
		t.Errorf("expected the single duration series to aggregate both tenants' requests (2 samples), got %v", got)
	}
}

// TestMiddleware_DifferentRouteOrStatus_ProducesSeparateSeries is the
// positive-control complement to the test above: it proves the low-
// cardinality labels this middleware DOES use are not accidentally
// collapsed to nothing either -- a middleware that dropped every
// attribute would also pass a "no forking" check vacuously.
func TestMiddleware_DifferentRouteOrStatus_ProducesSeparateSeries(t *testing.T) {
	reader := setupMeterProvider(t)
	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, "/api/v1/notes", ""))
	handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, "/missing", ""))

	rm := collect(t, reader)
	counter := findSum(t, findMetric(t, rm, requestCountMetricName))
	if got := len(counter.DataPoints); got != 2 {
		t.Fatalf("expected 2 distinct series for 2 distinct (route, status) pairs, got %d: %v", got, counter.DataPoints)
	}
}

// TestMiddleware_UnboundedRoutePaths_CardinalityIsBounded is the negative
// control for the live, unauthenticated exploit described in Middleware's
// "Metric label cardinality caveats" doc comment: neither a mux 404 nor
// tenancy.Middleware's pre-mux 403 requires a valid route or credential, so
// without a bound an attacker can create one new, permanent metric series
// per distinct URL path they send. This drives the handler well past
// obs.MaxRouteLabelValues distinct paths (concurrently, to also exercise
// routeLabelLimiter's mutex under -race) and asserts the resulting series
// count stays fixed at MaxRouteLabelValues+1 -- the tracked values plus the
// one overflow bucket -- rather than growing with the number of distinct
// paths sent.
func TestMiddleware_UnboundedRoutePaths_CardinalityIsBounded(t *testing.T) {
	reader := setupMeterProvider(t)
	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A real mux would 404 an unregistered path; the exact status is
		// irrelevant to what this test checks (route-label cardinality),
		// so a fixed 404 stands in for it.
		w.WriteHeader(http.StatusNotFound)
	}))

	const attackerRequests = obs.MaxRouteLabelValues + 20
	var wg sync.WaitGroup
	for i := 0; i < attackerRequests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("/attacker-garbage-path-%d", i)
			handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, path, ""))
		}(i)
	}
	wg.Wait()

	rm := collect(t, reader)
	assertNoTenantLabelAnywhere(t, rm)

	counter := findSum(t, findMetric(t, rm, requestCountMetricName))
	dataPoints := counter.DataPoints
	wantSeries := obs.MaxRouteLabelValues + 1
	if got := len(dataPoints); got != wantSeries {
		t.Fatalf("got %d distinct http.route series for %d distinct attacker paths, want exactly %d (%d tracked + 1 overflow bucket): route-label cardinality is not bounded",
			got, attackerRequests, wantSeries, obs.MaxRouteLabelValues)
	}

	var overflowFound bool
	var totalRecorded int64
	for _, dp := range dataPoints {
		v := dp.Value
		totalRecorded += v
		if labelMap(dp.Attributes)["http.route"] == obs.RouteLabelOverflowValue {
			overflowFound = true
			if want := int64(attackerRequests - obs.MaxRouteLabelValues); v != want {
				t.Errorf("overflow bucket (http.route=%q) recorded %v requests, want %v (%d total attacker requests - %d tracked distinct paths)",
					obs.RouteLabelOverflowValue, v, want, attackerRequests, obs.MaxRouteLabelValues)
			}
			continue
		}
		if v != 1 {
			t.Errorf("tracked route %q recorded %v requests, want exactly 1 (each attacker path in this test was requested once)", labelMap(dp.Attributes)["http.route"], v)
		}
	}
	if !overflowFound {
		t.Fatalf("expected one series labeled http.route=%q (the overflow bucket) among the %d gathered series, found none", obs.RouteLabelOverflowValue, len(dataPoints))
	}
	if totalRecorded != int64(attackerRequests) {
		t.Errorf("sum of all recorded requests = %v, want %v (total attacker requests actually sent): a request was lost or double-counted", totalRecorded, attackerRequests)
	}
}

// standardHTTPMethods is the known set the http.request.method METRIC label
// is bounded to: the nine net/http method constants Middleware records
// verbatim. It is enumerated here from the same net/http constants
// middleware.go's own known-set switch references, never as literals, so the
// test's expectation cannot drift from the production known set. HTTP method
// tokens are case-sensitive, so "get" is deliberately absent from (and folds
// out of) this list.
var standardHTTPMethods = []string{
	http.MethodGet,
	http.MethodHead,
	http.MethodPost,
	http.MethodPut,
	http.MethodPatch,
	http.MethodDelete,
	http.MethodConnect,
	http.MethodOptions,
	http.MethodTrace,
}

// isStandardMethod reports whether method is one of the nine standard tokens
// standardHTTPMethods enumerates -- the membership check the test below
// applies to every emitted http.request.method metric value.
func isStandardMethod(method string) bool {
	for _, m := range standardHTTPMethods {
		if method == m {
			return true
		}
	}
	return false
}

// TestMiddleware_UnboundedMethodTokens_CardinalityIsBounded is the negative
// control for the method-dimension half of the live, unauthenticated
// exploit described in Middleware's own "Metric label cardinality caveats"
// doc comment: the route label was already bounded, but the
// http.request.method METRIC label is fed by
// (*http.Request).Method -- the raw request-line method token, which
// net/http accepts from any unauthenticated caller with no set constraint,
// no normalization and no truncation. Middleware sits OUTSIDE
// tenancy.Middleware in the fixed chain (its own doc comment), so pre-auth
// 404s and 403s are counted exactly like the route exploit's traffic, and
// without a bound an attacker sending one distinct method token per request
// creates one new, permanent metric series per token -- the identical
// cardinality-explosion failure mode the route limiter already closes, one
// dimension over.
//
// This drives one Middleware instance with (a) every one of the nine
// standard methods once each, (b) far more distinct attacker-chosen method
// tokens than the known set could ever cover, one per request, plus (c) a
// lower-case near-miss ("get") and two non-standard tokens real clients
// legitimately use ("PROPFIND", WebDAV; "PRI", HTTP/2's connection
// preface) -- and asserts, from observable, black-box behavior, that the
// set of distinct http.request.method METRIC values ever emitted stays
// exactly the known set plus the single fixed overflow value: never one
// series per attacker token. On the unfixed code every distinct token
// becomes its own series and the distinct-value set grows without bound --
// this test fails there (verified by hand before the fix landed) and passes
// after.
func TestMiddleware_UnboundedMethodTokens_CardinalityIsBounded(t *testing.T) {
	reader := setupMeterProvider(t)
	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The exact status is irrelevant to what this test checks
		// (method-label cardinality), so a fixed 200 stands in for it,
		// exactly like the route-cardinality test's fixed 404.
		w.WriteHeader(http.StatusOK)
	}))

	// Leg (b): far more distinct attacker tokens than the known set could
	// ever cover -- an order of magnitude past the nine standard methods.
	const attackerMethodCount = 100
	// Leg (c): the near-miss and the non-standard-but-legitimate tokens.
	nonStandard := []string{"get", "PROPFIND", "PRI"}
	totalRequests := len(standardHTTPMethods) + attackerMethodCount + len(nonStandard)

	// All requests share one fixed path and one fixed status, so the only
	// label that varies across the series below is http.request.method --
	// the distinct-series count IS the distinct-method-label count, which is
	// what makes the boundedness assertion below unambiguous.
	const path = "/api/v1/notes"
	send := func(method string) {
		t.Helper()
		handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(method, path, ""))
	}
	for _, m := range standardHTTPMethods {
		send(m)
	}
	for i := 0; i < attackerMethodCount; i++ {
		send(fmt.Sprintf("ATTACKER-METHOD-%03d", i))
	}
	for _, m := range nonStandard {
		send(m)
	}

	rm := collect(t, reader)
	assertNoTenantLabelAnywhere(t, rm)

	counter := findSum(t, findMetric(t, rm, requestCountMetricName))
	dataPoints := counter.DataPoints

	// The boundedness assertion proper: every emitted method label must be
	// one of the nine standard tokens or the single fixed overflow value --
	// pre-fix, the 100 attacker tokens plus "get"/"PROPFIND"/"PRI" each emit
	// their own label and this fails.
	seen := make(map[string]int64, len(dataPoints))
	var overflowFound bool
	var totalRecorded int64
	for _, dp := range dataPoints {
		labels := labelMap(dp.Attributes)
		method := labels["http.request.method"]
		seen[method] += dp.Value
		totalRecorded += dp.Value
		if method == obs.MethodLabelOverflowValue {
			overflowFound = true
			continue
		}
		if !isStandardMethod(method) {
			t.Fatalf("emitted http.request.method metric label %q is neither one of the %d standard methods nor %q: method-label cardinality is not bounded",
				method, len(standardHTTPMethods), obs.MethodLabelOverflowValue)
		}
	}

	// The overflow bucket must exist and must have absorbed every
	// non-standard token sent, exactly: on the unfixed code there is no
	// overflow bucket at all.
	if !overflowFound {
		t.Fatalf("expected one series labeled http.request.method=%q (the overflow bucket) among the %d gathered series, found none",
			obs.MethodLabelOverflowValue, len(dataPoints))
	}
	wantOverflow := int64(attackerMethodCount + len(nonStandard))
	if got := seen[obs.MethodLabelOverflowValue]; got != wantOverflow {
		t.Errorf("overflow bucket (http.request.method=%q) recorded %v requests, want %v (%d attacker tokens + %d near-miss/non-standard tokens)",
			obs.MethodLabelOverflowValue, got, wantOverflow, attackerMethodCount, len(nonStandard))
	}

	// No collateral damage: each of the nine standard methods must still
	// record its exact, case-sensitive token -- including GET (the token
	// every real dashboard's method dimension is built on) -- never the
	// overflow value.
	for _, m := range standardHTTPMethods {
		got, ok := seen[m]
		if !ok {
			t.Errorf("standard method %q recorded no series of its own: known-method tokens must still be recorded verbatim, not collapsed", m)
			continue
		}
		if got != 1 {
			t.Errorf("standard method %q recorded %v requests, want exactly 1 (each standard method in this test was requested once)", m, got)
		}
	}

	// The near-miss and non-standard tokens must NOT have their own series:
	// "get" is a different, case-sensitive token from GET and folds; so do
	// PROPFIND and PRI. (On the unfixed code each of these three is its own
	// series and this check fails at the boundedness assertion above first.)
	for _, m := range nonStandard {
		if _, ok := seen[m]; ok {
			t.Errorf("non-standard method token %q got its own http.request.method series: everything outside the known set must fold to %q",
				m, obs.MethodLabelOverflowValue)
		}
	}

	if totalRecorded != int64(totalRequests) {
		t.Errorf("sum of all recorded requests = %v, want %v (total requests actually sent): a request was lost or double-counted",
			totalRecorded, totalRequests)
	}
	if want := len(dataPoints); want != len(standardHTTPMethods)+1 {
		t.Fatalf("got %d distinct http.request.method series for %d distinct method tokens sent, want exactly %d (%d standard + 1 overflow bucket): method-label cardinality is not bounded",
			want, totalRequests, len(standardHTTPMethods)+1, len(standardHTTPMethods))
	}
}

// longAttackerPathPrefixLen and longAttackerPathTotalLen size the two
// attacker paths TestMiddleware_LongRoutePaths_LabelLengthIsBounded sends.
// longAttackerPathPrefixLen is deliberately > obs.MaxRouteLabelLength (the
// two paths built by longAttackerPath share every byte up to this length,
// then diverge), and longAttackerPathTotalLen is a "few KB" -- realistically
// sized for a test rather than the near-1-MiB path a real attacker could
// send (see obs.MaxRouteLabelLength's own doc comment for that real-world
// sizing) -- while still landing well past the cap, which is all the
// mechanism being tested cares about.
const (
	longAttackerPathPrefixLen = 600
	longAttackerPathTotalLen  = 4096
)

// longAttackerPath returns a longAttackerPathTotalLen-byte absolute path
// whose first longAttackerPathPrefixLen bytes are always the same
// (regardless of suffix), diverging only afterward. Two calls with
// different suffix bytes are therefore identical within
// obs.MaxRouteLabelLength (600 > obs.MaxRouteLabelLength=512) and distinct
// beyond it -- exactly the shape TestMiddleware_LongRoutePaths_LabelLengthIsBounded
// needs to tell "truncated before use as a map key" apart from "used raw."
func longAttackerPath(suffix byte) string {
	prefix := "/" + strings.Repeat("p", longAttackerPathPrefixLen-1)
	rest := strings.Repeat(string(suffix), longAttackerPathTotalLen-longAttackerPathPrefixLen)
	return prefix + rest
}

// TestMiddleware_LongRoutePaths_LabelLengthIsBounded is the regression test
// for the gap TestMiddleware_UnboundedRoutePaths_CardinalityIsBounded above
// does NOT cover: that test bounds the NUMBER of distinct http.route
// values; this one bounds the LENGTH of any single value.
//
// examples/reference-app's http.Server (cmd/server/main.go) sets no
// MaxHeaderBytes, so it inherits net/http.DefaultMaxHeaderBytes (1 MiB) as
// the effective ceiling on a single request's URL path length. Without a
// length bound, an unauthenticated attacker sending up to
// obs.MaxRouteLabelValues requests, each with a distinct, near-1-MiB path,
// could make routeLabelLimiter's internal "seen" map retain up to roughly
// obs.MaxRouteLabelValues x 1 MiB of attacker-controlled string data for
// the life of the process, and could make the actual exported Prometheus
// series carry labels that large -- see obs.MaxRouteLabelLength's own doc
// comment for the full reasoning.
//
// This test sends two longAttackerPathTotalLen-byte (a few KB, not a
// literal ~1 MiB -- unnecessary to actually prove the mechanism) paths
// that share an identical prefix longer than obs.MaxRouteLabelLength and
// diverge only after it. This shape proves BOTH bounds this middleware
// needs to enforce, entirely from observable, black-box behavior (this
// file is package observability_test and routeLabelLimiter's "seen" field
// is unexported, so there is no other way to reach it):
//
//   - The exported metric label itself must come back at exactly
//     obs.MaxRouteLabelLength bytes -- proving the VALUE handed to the
//     Prometheus exporter is bounded, not the full attacker-supplied
//     length.
//   - The two requests must collapse into exactly ONE series, not two.
//     This is the internal-state proof: if routeLabelLimiter's "seen" map
//     used the raw, untruncated path as its key (the pre-fix behavior),
//     these two paths -- which differ after byte longAttackerPathPrefixLen
//     -- would be two distinct map entries and therefore two distinct
//     series. They can collapse into one only if both were truncated to
//     the same obs.MaxRouteLabelLength-byte prefix BEFORE either became a
//     map key, which is exactly what routeLabelLimiter.label must now do.
//
// Negative-control proof (per this repository's established technique --
// see TestMiddleware_UnboundedRoutePaths_CardinalityIsBounded's own use of
// it): temporarily removing the "path = truncateRouteLabel(path)" line
// from routeLabelLimiter.label makes this test fail on both fronts at
// once -- 2 series instead of 1, and a recorded label
// longAttackerPathTotalLen bytes long instead of obs.MaxRouteLabelLength
// -- then restoring it makes the test pass again. This was verified by
// hand while writing this test, not merely asserted here.
func TestMiddleware_LongRoutePaths_LabelLengthIsBounded(t *testing.T) {
	reader := setupMeterProvider(t)
	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A real mux would 404 an unregistered path, exactly like
		// TestMiddleware_UnboundedRoutePaths_CardinalityIsBounded's handler
		// -- the exact status is irrelevant to what this test checks.
		w.WriteHeader(http.StatusNotFound)
	}))

	pathA := longAttackerPath('a')
	pathB := longAttackerPath('b')
	if len(pathA) != longAttackerPathTotalLen || len(pathB) != longAttackerPathTotalLen {
		t.Fatalf("test setup: attacker paths must be %d bytes, got %d and %d", longAttackerPathTotalLen, len(pathA), len(pathB))
	}
	if pathA[:longAttackerPathPrefixLen] != pathB[:longAttackerPathPrefixLen] {
		t.Fatalf("test setup: attacker paths must share an identical %d-byte prefix", longAttackerPathPrefixLen)
	}
	if pathA == pathB {
		t.Fatalf("test setup: attacker paths must diverge after byte %d, got identical paths", longAttackerPathPrefixLen)
	}

	handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, pathA, ""))
	handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, pathB, ""))

	rm := collect(t, reader)
	assertNoTenantLabelAnywhere(t, rm)

	counter := findSum(t, findMetric(t, rm, requestCountMetricName))
	dataPoints := counter.DataPoints
	if got := len(dataPoints); got != 1 {
		t.Fatalf("got %d distinct http.route series for 2 attacker paths sharing an identical %d-byte prefix (past obs.MaxRouteLabelLength=%d bytes), want exactly 1: routeLabelLimiter's internal map key is not bounded to MaxRouteLabelLength",
			got, longAttackerPathPrefixLen, obs.MaxRouteLabelLength)
	}

	dp := dataPoints[0]
	gotLabel := labelMap(dp.Attributes)["http.route"]
	if got := len(gotLabel); got != obs.MaxRouteLabelLength {
		t.Fatalf("recorded http.route label is %d bytes, want exactly %d (obs.MaxRouteLabelLength): a %d-byte attacker path was not truncated",
			got, obs.MaxRouteLabelLength, longAttackerPathTotalLen)
	}
	if want := pathA[:obs.MaxRouteLabelLength]; gotLabel != want {
		t.Errorf("recorded http.route label = %q, want the attacker path's first %d bytes (%q)", gotLabel, obs.MaxRouteLabelLength, want)
	}
	if got := dp.Value; got != 2 {
		t.Errorf("the single collapsed series recorded %v requests, want 2 (both attacker paths counted under the same truncated label)", got)
	}
}

// multiByteRoutePathPrefixLen sizes the ASCII prefix
// TestMiddleware_MultiByteRoutePath_TruncatesOnRuneBoundary places before
// multiByteRune, chosen so the rune's bytes straddle byte index
// obs.MaxRouteLabelLength: the prefix ends 2 bytes before the cut point,
// so the 4-byte rune placed right after it occupies the cut byte itself
// plus one byte on each side, putting a continuation byte -- never a
// rune-start byte -- exactly at index obs.MaxRouteLabelLength. Derived
// from obs.MaxRouteLabelLength, rather than hardcoded, so this test keeps
// exercising the same straddle if that constant is ever revisited.
const multiByteRoutePathPrefixLen = obs.MaxRouteLabelLength - 2

// multiByteRune is a single UTF-8 rune (U+1F600, an emoji) encoded as 4
// bytes -- the shape truncateRouteLabel's backward scan exists to protect,
// per its own doc comment in middleware.go: net/http percent-decodes
// (*http.Request).URL.Path before this package ever sees it, so a path
// segment can legally contain multi-byte UTF-8 like this one.
const multiByteRune = "😀"

// TestMiddleware_MultiByteRoutePath_TruncatesOnRuneBoundary is the
// regression test for the one reason truncateRouteLabel (middleware.go) is
// not a plain path[:obs.MaxRouteLabelLength] slice. Every attacker path
// TestMiddleware_LongRoutePaths_LabelLengthIsBounded above sends is built
// entirely from single-byte ASCII, so its byte-obs.MaxRouteLabelLength cut
// point always lands on a rune-start byte and never exercises
// truncateRouteLabel's backward "scan to a rune boundary" loop -- a
// regression there (off-by-one, wrong comparison, wrong scan direction)
// would silently slice a multi-byte rune in half, and no test in this file
// would notice.
//
// This test places multiByteRune so its 4 bytes straddle byte index
// obs.MaxRouteLabelLength: multiByteRoutePathPrefixLen bytes of ASCII,
// then the rune. A naive path[:obs.MaxRouteLabelLength] slice would keep
// only the rune's first two bytes -- invalid, truncated UTF-8.
// truncateRouteLabel must instead back up to the rune's start and return
// exactly the ASCII prefix: valid UTF-8, one rune short of the cut point.
//
// The request target below percent-encodes the rune's bytes
// (%F0%9F%98%80) rather than embedding them raw, matching how an actual
// multi-byte path segment reaches this package: net/http percent-decodes
// (*http.Request).URL.Path before Middleware ever sees it. Verified by
// hand while writing this test: req.URL.Path below decodes back to
// exactly prefix+multiByteRune, byte obs.MaxRouteLabelLength of that path
// is confirmed NOT a rune-start byte, and a naive path[:obs.MaxRouteLabelLength]
// slice of it is confirmed invalid UTF-8 -- so this test genuinely
// exercises the backward scan rather than landing on a boundary by luck.
func TestMiddleware_MultiByteRoutePath_TruncatesOnRuneBoundary(t *testing.T) {
	reader := setupMeterProvider(t)
	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A real mux would 404 an unregistered path, exactly like
		// TestMiddleware_LongRoutePaths_LabelLengthIsBounded's handler --
		// the exact status is irrelevant to what this test checks.
		w.WriteHeader(http.StatusNotFound)
	}))

	prefix := "/" + strings.Repeat("p", multiByteRoutePathPrefixLen-1)
	var percentEncodedRune strings.Builder
	for i := 0; i < len(multiByteRune); i++ {
		fmt.Fprintf(&percentEncodedRune, "%%%02X", multiByteRune[i])
	}
	path := prefix + multiByteRune                 // what (*http.Request).URL.Path must decode to
	target := prefix + percentEncodedRune.String() // what actually goes on the request line

	if len(multiByteRune) != 4 {
		t.Fatalf("test setup: multiByteRune must be a 4-byte UTF-8 rune, got %d bytes", len(multiByteRune))
	}
	if len(path) <= obs.MaxRouteLabelLength {
		t.Fatalf("test setup: path must exceed obs.MaxRouteLabelLength (%d) to trigger truncation, got %d bytes", obs.MaxRouteLabelLength, len(path))
	}
	if utf8.RuneStart(path[obs.MaxRouteLabelLength]) {
		t.Fatalf("test setup: byte at index obs.MaxRouteLabelLength (%d) must NOT be a UTF-8 rune-start byte, or this test never actually reaches truncateRouteLabel's backward scan", obs.MaxRouteLabelLength)
	}
	if utf8.ValidString(path[:obs.MaxRouteLabelLength]) {
		t.Fatalf("test setup: a naive path[:obs.MaxRouteLabelLength] slice must be invalid UTF-8, or this test does not prove truncateRouteLabel's backward scan does anything")
	}

	req := newTestRequest(http.MethodGet, target, "")
	if req.URL.Path != path {
		t.Fatalf("test setup: request URL.Path = %q after percent-decoding, want %q: the request target was not built the way this test assumes", req.URL.Path, path)
	}
	handler.ServeHTTP(httptest.NewRecorder(), req)

	rm := collect(t, reader)
	assertNoTenantLabelAnywhere(t, rm)

	counter := findSum(t, findMetric(t, rm, requestCountMetricName))
	dataPoints := counter.DataPoints
	if got := len(dataPoints); got != 1 {
		t.Fatalf("got %d distinct http.route series for a single request, want exactly 1", got)
	}

	gotLabel := labelMap(dataPoints[0].Attributes)["http.route"]
	if !utf8.ValidString(gotLabel) {
		t.Fatalf("recorded http.route label %q is not valid UTF-8: truncateRouteLabel cut the straddling multi-byte rune in half", gotLabel)
	}
	if got := len(gotLabel); got != multiByteRoutePathPrefixLen {
		t.Fatalf("recorded http.route label is %d bytes, want exactly %d: truncateRouteLabel should back up to the byte immediately before the straddling rune, no further and no less", got, multiByteRoutePathPrefixLen)
	}
	if want := path[:multiByteRoutePathPrefixLen]; gotLabel != want {
		t.Errorf("recorded http.route label = %q, want the ASCII prefix before the straddling rune (%q)", gotLabel, want)
	}
}

// TestMiddleware_StartsSpanNamedAfterMethodAndPath confirms Middleware
// actually starts a real span (via otelhttp) per request, independent of
// the metrics assertions above.
func TestMiddleware_StartsSpanNamedAfterMethodAndPath(t *testing.T) {
	exp := setupTracerProvider(t)
	setupMeterProvider(t) // Middleware always records metrics too; give it a live provider.

	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, "/api/v1/notes", ""))

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected exactly 1 span, got %d", len(spans))
	}
	if want := "GET /api/v1/notes"; spans[0].Name != want {
		t.Errorf("span name = %q, want %q", spans[0].Name, want)
	}
}

// TestMiddleware_QueryStringSecrets_NeverReachSpanAttributes is the
// span-channel half of the redaction guarantee whose log-channel half lives
// in redact_test.go. An HTTP request's query string is where credentials
// ride (?access_token=..., ?signature=..., ?api_key=...), so the promise
// "no secret this middleware's instrumentation emits reaches the exported
// span" rests entirely on the query never becoming a span name or a span
// attribute. Today that is guaranteed by two mechanisms: otelhttp's own
// semconv deliberately omits url.query and url.full from the SERVER span
// attributes it attaches, and Middleware's span name formatter and its
// http.route attribute use (*http.Request).URL.Path, which net/http has
// already split from the query before this package ever sees the request.
// Both are exclusions-by-default rather than anything this module actively
// redacts, so they are pinned here by a negative control instead of trusted
// by assumption: if a future otelhttp upgrade starts attaching url.full, or
// a future edit names a span from r.RequestURI or URL.String(), this test
// fails and forces a deliberate decision (redact the span channel in
// redact.go, or accept the regression) rather than leaking silently.
//
// The request below carries a query whose parameter name AND value are both
// secret-shaped -- access_token=<34-char credential> -- plus a benign
// sibling parameter, so a naive "drop the sensitive parameter" filter would
// still have to know the name to drop it; the assertion here is stronger:
// NOTHING about the query, not even its benign parameter, may appear in the
// span. The positive controls alongside it (tenant_id intact, http.route
// intact) confirm the test is looking at a real, fully-populated span and
// not passing vacuously on an empty attribute set.
func TestMiddleware_QueryStringSecrets_NeverReachSpanAttributes(t *testing.T) {
	exp := setupTracerProvider(t)
	setupMeterProvider(t) // Middleware always records metrics too; give it a live provider.

	const credential = "abcDEFgh1234567890XYZmnopQRSTuvWX"
	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(),
		newTestRequest(http.MethodGet, "/api/v1/notes?access_token="+credential+"&expand=charge", "acme"))

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected exactly 1 span, got %d", len(spans))
	}
	span := spans[0]

	if want := "GET /api/v1/notes"; span.Name != want {
		t.Errorf("span name = %q, want %q: the query string must not leak into the span name", span.Name, want)
	}

	// The sensitive stems mirroring redact.go's key-based rule: a span
	// attribute KEY carrying any of them would be a secret-shaped field
	// reaching the exporter regardless of its value.
	sensitiveStems := []string{
		"token", "secret", "password", "passwd", "pwd",
		"authorization", "cookie", "credential", "key",
	}
	for _, kv := range span.Attributes {
		key, val := string(kv.Key), kv.Value.AsString()
		lowerKey := strings.ToLower(key)
		for _, stem := range sensitiveStems {
			if strings.Contains(lowerKey, stem) {
				t.Errorf("span attribute key %q contains sensitive stem %q: no secret-shaped field may reach span attributes", key, stem)
			}
		}
		if strings.Contains(val, credential) || strings.Contains(val, "access_token") || strings.Contains(val, "expand=charge") {
			t.Errorf("span attribute %q carries query-string material (value %q): the query string must never reach span attributes", key, val)
		}
	}

	// Positive controls: the same request's legitimate span signal survives
	// -- the tenant correlation field untouched, and http.route carrying the
	// path half of the request the way Middleware documents.
	if got, ok := findAttr(span.Attributes, obs.TenantIDKey); !ok || got.AsString() != "acme" {
		t.Errorf("expected tenant_id=acme to survive on the span, got attributes: %v", span.Attributes)
	}
	if got, ok := findAttr(span.Attributes, "http.route"); !ok || got.AsString() != "/api/v1/notes" {
		t.Errorf("expected http.route=/api/v1/notes to survive on the span, got attributes: %v", span.Attributes)
	}
}

// TestMiddleware_ServerError_SetsSpanErrorStatus confirms a 5xx response
// is reflected as an error span status, not just as a metric label.
func TestMiddleware_ServerError_SetsSpanErrorStatus(t *testing.T) {
	exp := setupTracerProvider(t)
	setupMeterProvider(t)

	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, "/api/v1/notes", ""))

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected exactly 1 span, got %d", len(spans))
	}
	if got := spans[0].Status.Code; got != codes.Error {
		t.Errorf("span status code = %v, want %v", got, codes.Error)
	}
}

// TestMiddleware_ImplicitOK_WhenHandlerNeverCallsWriteHeader confirms the
// statusRecorder records the implicit 200 net/http itself sends when a
// handler calls Write without ever calling WriteHeader.
func TestMiddleware_ImplicitOK_WhenHandlerNeverCallsWriteHeader(t *testing.T) {
	reader := setupMeterProvider(t)
	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, "/api/v1/notes", ""))

	rm := collect(t, reader)
	counter := findSum(t, findMetric(t, rm, requestCountMetricName))
	got := labelMap(counter.DataPoints[0].Attributes)
	if got["http.response.status_code"] != "200" {
		t.Errorf("status_code label = %q, want %q (implicit OK)", got["http.response.status_code"], "200")
	}
}

// TestMiddleware_AnnotatesSpanWithTenant_WhenTenantAlreadyOnEntryContext
// exercises Middleware's own defensive tenant check directly: per its doc
// comment, Middleware is documented to be mounted OUTSIDE
// tenancy.Middleware, where a tenant is not normally present yet -- but
// the check it makes on whatever context it IS given is real code, not
// aspirational, and this proves it fires correctly when a tenant happens
// to already be there (exactly what would be true if Middleware were ever
// mounted downstream of tenant resolution instead, or -- as here -- called
// directly against a context a test built by hand).
func TestMiddleware_AnnotatesSpanWithTenant_WhenTenantAlreadyOnEntryContext(t *testing.T) {
	exp := setupTracerProvider(t)
	setupMeterProvider(t)

	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, "/api/v1/notes", "acme"))

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected exactly 1 span, got %d", len(spans))
	}
	got, ok := findAttr(spans[0].Attributes, obs.TenantIDKey)
	if !ok {
		t.Fatalf("expected span to carry a %s attribute; attributes: %v", obs.TenantIDKey, spans[0].Attributes)
	}
	if got.AsString() != "acme" {
		t.Errorf("%s attribute = %q, want %q", obs.TenantIDKey, got.AsString(), "acme")
	}
}

func TestAnnotateTenant_NoSpanNoTenant_NoPanic(t *testing.T) {
	// No TracerProvider, no tenant: trace.SpanFromContext falls back to a
	// no-op span, and pkgcore.TenantFromContext reports false. Neither
	// should panic.
	obs.AnnotateTenant(context.Background())
}

func TestAnnotateTenant_SetsAttributeOnActiveSpan(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	ctx, span := tp.Tracer("observability_test").Start(context.Background(), "op")
	ctx = pkgcore.WithTenant(ctx, pkgcore.TenantID("acme"))

	obs.AnnotateTenant(ctx)
	span.End()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected exactly 1 span, got %d", len(spans))
	}
	got, ok := findAttr(spans[0].Attributes, obs.TenantIDKey)
	if !ok || got.AsString() != "acme" {
		t.Errorf("expected span to carry %s=acme, got attributes: %v", obs.TenantIDKey, spans[0].Attributes)
	}
}

func TestAnnotateTenant_NoTenant_LeavesSpanUnmodified(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	ctx, span := tp.Tracer("observability_test").Start(context.Background(), "op")
	obs.AnnotateTenant(ctx) // no tenant on ctx: must be a no-op
	span.End()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected exactly 1 span, got %d", len(spans))
	}
	if _, ok := findAttr(spans[0].Attributes, obs.TenantIDKey); ok {
		t.Errorf("expected no %s attribute with no tenant on context, got attributes: %v", obs.TenantIDKey, spans[0].Attributes)
	}
}

// TestMiddleware_PanickingHandler_StillRecordsMetricsAndErrorSpan is the
// regression for a hole in Middleware's "Every request gets counted here"
// contract: the metric-recording and span-enriching block used to run
// after next.ServeHTTP returned, with no defer, so a handler that panicked
// unwound straight past this middleware -- the reference app's chain has
// no recover middleware above this one, and net/http's own per-connection
// recovery is the first thing a panic reaches -- and the request vanished
// from both the metrics and the span status entirely (0 metrics collected,
// and a span left with an unset status). The recording block now runs in a
// defer, so a panic still produces the request's count/duration data point
// -- labeled with the 500 this middleware records as its stand-in for a
// response that never reached the client (see the defer's own comment) --
// and an error-status span. The panic itself is deliberately NOT recovered
// here: it keeps propagating to net/http's recovery above, exactly as it
// would without this middleware in the chain. Fails before the fix
// (verified): the collect below finds no counter series at all, and the
// span's status code is Unset.
func TestMiddleware_PanickingHandler_StillRecordsMetricsAndErrorSpan(t *testing.T) {
	exp := setupTracerProvider(t)
	reader := setupMeterProvider(t)

	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		panic("probe: handler panic")
	}))

	// net/http's per-connection recovery sits ABOVE the whole handler
	// chain; this test stands in for it at the edge so the panic does not
	// fail the test itself.
	func() {
		defer func() { _ = recover() }()
		handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, "/api/v1/notes", ""))
	}()

	rm := collect(t, reader)
	counter := findSum(t, findMetric(t, rm, requestCountMetricName))
	if got := len(counter.DataPoints); got != 1 {
		t.Fatalf("expected exactly 1 counter series for a panicking request, got %d: %v", got, counter.DataPoints)
	}
	labels := labelMap(counter.DataPoints[0].Attributes)
	if labels["http.response.status_code"] != "500" {
		t.Errorf("a panicking handler that wrote no response must be recorded with status 500, got labels: %v", labels)
	}
	if got := counter.DataPoints[0].Value; got != 1 {
		t.Errorf("counter value = %v, want 1", got)
	}

	duration := findHistogram(t, findMetric(t, rm, requestDurationMetricName))
	if got := duration.DataPoints[0].Count; got != 1 {
		t.Errorf("duration sample count = %v, want 1 (the panicking request must still be timed)", got)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected exactly 1 span, got %d", len(spans))
	}
	if got := spans[0].Status.Code; got != codes.Error {
		t.Errorf("span status code for a panicking handler = %v, want %v (an error-status span)", got, codes.Error)
	}
}

// TestMiddleware_InvalidUTF8Path_SanitizesBeforeTheLabel is the
// label-formation-side regression for the invalid-UTF-8 route-label class
// (the exporter-side, scrape-level regression lives in
// exporter/prometheus/prometheus_test.go's
// TestBuildReader_InvalidUTF8Path_NeverVoidsTheScrape): net/http
// percent-decodes a request target byte-wise, so a %FF in the path
// reaches Middleware as a raw invalid byte, and the http.route metric
// label must never be formed from it -- the Prometheus exporter rejects an
// invalid-UTF-8 label value on every Gather, which would void the whole
// /metrics scrape (an unauthenticated DoS the route label's count and
// length bounds never covered). The label must instead carry the Unicode
// replacement rune in the invalid byte's place -- the request is still
// counted, under a valid label -- while a valid path is never touched.
// Fails before the fix (verified): the recorded route label is "/api/..."'
// with the raw 0xFF byte in it and utf8.ValidString reports false.
func TestMiddleware_InvalidUTF8Path_SanitizesBeforeTheLabel(t *testing.T) {
	reader := setupMeterProvider(t)
	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// A valid control request first, so the sanitizing path has a sibling
	// to compare against in the same snapshot.
	handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, "/api/v1/notes", ""))

	bad := httptest.NewRequest(http.MethodGet, "/api/%FFjunk", nil)
	if utf8.ValidString(bad.URL.Path) {
		t.Fatalf("test setup: URL.Path %q must not be valid UTF-8 for this regression to be exercised", bad.URL.Path)
	}
	handler.ServeHTTP(httptest.NewRecorder(), bad)

	rm := collect(t, reader)
	counter := findSum(t, findMetric(t, rm, requestCountMetricName))
	if got := len(counter.DataPoints); got != 2 {
		t.Fatalf("expected 2 distinct counter series (the valid request and the sanitized invalid one), got %d: %v", got, counter.DataPoints)
	}
	for _, dp := range counter.DataPoints {
		route := labelMap(dp.Attributes)["http.route"]
		if !utf8.ValidString(route) {
			t.Fatalf("recorded http.route label %q is not valid UTF-8: an invalid path byte reached the label value", route)
		}
	}
	// The invalid request is still counted, under the sanitized label.
	var sanitizedFound bool
	for _, dp := range counter.DataPoints {
		route := labelMap(dp.Attributes)["http.route"]
		if route == "/api/"+string(utf8.RuneError)+"junk" {
			sanitizedFound = true
			if dp.Value != 1 {
				t.Errorf("sanitized series recorded %v requests, want 1", dp.Value)
			}
		}
	}
	if !sanitizedFound {
		t.Fatalf("expected a series labeled http.route=\"/api/<U+FFFD>junk\" (the invalid path sanitized); got: %v", counter.DataPoints)
	}
}

// TestMiddleware_RealRoutesSurviveGarbage_WhenSeeded is the regression
// for the route limiter's first-come-first-served pre-emption: before a
// host can register its real route table (obs.RegisterMountedRoutes), the
// limiter's distinct-value budget was filled by whatever request traffic
// arrived first, so 256 distinct garbage paths sent right after startup
// collapsed every genuine route -- /api/v1/notes included -- to
// RouteLabelOverflowValue for the process lifetime, with no bound
// violated. With the route table seeded at construction (the register call
// below, which Middleware snapshots when it builds the limiter), a real
// route keeps its own series whatever garbage arrives. Fails before the
// seeding lands (verified by removing the seeding loop in Middleware):
// the series labeled http.route="/api/v1/notes" does not exist -- the real
// route's request lands in the overflow bucket instead.
func TestMiddleware_RealRoutesSurviveGarbage_WhenSeeded(t *testing.T) {
	reader := setupMeterProvider(t)

	obs.RegisterMountedRoutes([]pkgcore.MountedRoute{
		{Path: "/api/v1/notes"},
		{Path: "/healthz"},
	})
	t.Cleanup(func() { obs.RegisterMountedRoutes(nil) })

	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	// Saturate the distinct-value budget with attacker garbage -- exactly
	// obs.MaxRouteLabelValues distinct paths, which without the seed
	// exhaust the limiter before the real route below is ever requested.
	for i := 0; i < obs.MaxRouteLabelValues; i++ {
		path := fmt.Sprintf("/attacker-garbage-path-%d", i)
		handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, path, ""))
	}

	// The real route, requested only after the budget is exhausted: it
	// must still record its own series, not the overflow bucket.
	handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, "/api/v1/notes", ""))

	rm := collect(t, reader)
	counter := findSum(t, findMetric(t, rm, requestCountMetricName))

	var realRouteFound, overflowFound bool
	var totalRecorded int64
	for _, dp := range counter.DataPoints {
		labels := labelMap(dp.Attributes)
		totalRecorded += dp.Value
		switch route := labels["http.route"]; route {
		case "/api/v1/notes":
			realRouteFound = true
			if dp.Value != 1 {
				t.Errorf("the real route's series recorded %v requests, want 1", dp.Value)
			}
		case obs.RouteLabelOverflowValue:
			overflowFound = true
			if want := int64(2); dp.Value != want {
				t.Errorf("overflow bucket recorded %v requests, want %v (the two garbage paths the two seeded slots displaced)", dp.Value, want)
			}
		}
	}
	if !realRouteFound {
		t.Fatalf("expected a series labeled http.route=\"/api/v1/notes\" after %d garbage paths: real routes must survive the budget being exhausted (pre-seed behavior collapses them to %q)",
			obs.MaxRouteLabelValues, obs.RouteLabelOverflowValue)
	}
	if !overflowFound {
		t.Errorf("expected the overflow bucket to still exist (2 garbage paths beyond the seeded budget); series: %v", counter.DataPoints)
	}
	if totalRecorded != int64(obs.MaxRouteLabelValues+1) {
		t.Errorf("sum of all recorded requests = %v, want %v: a request was lost or double-counted",
			totalRecorded, obs.MaxRouteLabelValues+1)
	}

	// Boundedness holds all the same: the budget is 256 slots of which the
	// two seeds reserve two, so 254 garbage paths are tracked and the
	// remaining 2 collapse to the single overflow bucket -- and the real
	// route's request lands on its own pre-seeded series. A data point
	// only materializes for a slot some request actually used, so the
	// series count is 254 tracked garbage + 1 real route + 1 overflow
	// bucket = obs.MaxRouteLabelValues, not one per budget slot.
	const seededRoutes = 2
	if got := len(counter.DataPoints); got != obs.MaxRouteLabelValues {
		t.Fatalf("got %d distinct series, want exactly %d (%d budget slots - %d seeded routes that reserve but do not emit + 1 real route + 1 overflow bucket): the seed must not grow the bound",
			got, obs.MaxRouteLabelValues, obs.MaxRouteLabelValues, seededRoutes)
	}
}

// TestMiddleware_RealRoutesBelowSeededPrefixes_SurviveStartupGarbage is
// the regression for the P1-obs-8 gap in the seed's PREFIX semantics:
// pkgcore.MountedRoute.Path is the PREFIX a module's handler was mounted
// at (pkgcore/registry.go), while the route limiter's runtime input is
// the request's full URL.Path and its seen-set is an exact-string map --
// so the exact-match seed shipped by the earlier round protected only
// the request whose whole path WAS a mount prefix. Every genuine
// operation path deeper than its mount -- /api/v1/authn/login/password
// under the /api/v1/authn prefix, and with it authn's twenty, org's
// eleven, notification's eleven, storage's seven and sharing's one
// operations -- was unseeded and still collapsed to
// obs.RouteLabelOverflowValue once startup garbage had exhausted the
// budget: the P1-8 consequence held for almost every real route. The
// seed now treats each entry as covering its whole mount subtree -- a
// request at or below a seeded path is labeled with the seeded path
// itself, the closest this limiter can get to route-template folding
// without a real route-capture mechanism -- so a deep operation
// requested only after the budget is exhausted still records a
// measurable series of its own (under its mount's label), never the
// overflow bucket. The two prefix==path baselines below (the shape the
// pre-fix regression TestMiddleware_RealRoutesSurviveGarbage_WhenSeeded
// already pinned) must keep passing before and after, so a failure of
// the deep-route assertion cannot be blamed on the flood not reaching
// the limiter. Fails before the fix (verified): no series labeled
// http.route="/api/v1/authn" exists -- the deep operation's request
// lands in the overflow bucket instead, whose count is one higher than
// the 104 the garbage alone produces.
func TestMiddleware_RealRoutesBelowSeededPrefixes_SurviveStartupGarbage(t *testing.T) {
	reader := setupMeterProvider(t)

	// The shape a real host registers: host-level leaf routes plus the
	// module mount PREFIXES its pkgcore registry carries (mirrors
	// examples/reference-app/cmd/server/server.go's own
	// obs.RegisterMountedRoutes call).
	obs.RegisterMountedRoutes([]pkgcore.MountedRoute{
		{Path: "/healthz"},
		{Path: "/metrics"},
		{Path: "/api/v1/notes"},
		{Path: "/api/v1/authn"},
	})
	t.Cleanup(func() { obs.RegisterMountedRoutes(nil) })

	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Saturate the distinct-value budget with attacker garbage OUTSIDE
	// every seeded prefix. With 4 seeds reserving 4 of the 256 budget
	// slots, 356 garbage paths leave 252 tracked verbatim and 104
	// overflowing; both numbers are deterministic and the assertions
	// below rely on them.
	const (
		garbagePaths = obs.MaxRouteLabelValues + 100
		seededRoutes = 4
	)
	for i := 0; i < garbagePaths; i++ {
		path := fmt.Sprintf("/attacker-garbage-path-%d", i)
		handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, path, ""))
	}

	// The real traffic, first requested only after the budget is
	// exhausted: the two prefix==path baselines (a host leaf route and a
	// module mount requested at its own path) and the regression's real
	// case -- a genuine operation path several segments below its
	// mount's prefix, which no exact-string seed could ever have
	// protected.
	handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, "/healthz", ""))
	handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, "/api/v1/notes", ""))
	handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, "/api/v1/authn/login/password", ""))

	rm := collect(t, reader)
	counter := findSum(t, findMetric(t, rm, requestCountMetricName))

	series := map[string]int64{}
	var totalRecorded int64
	for _, dp := range counter.DataPoints {
		totalRecorded += dp.Value
		series[labelMap(dp.Attributes)["http.route"]] += dp.Value
	}

	// Baselines (pass before the fix too): a host leaf route and a module
	// mount requested at its own path keep their seeded series.
	for _, path := range []string{"/healthz", "/api/v1/notes"} {
		if got := series[path]; got != 1 {
			t.Fatalf("baseline http.route=%q recorded %d requests, want exactly 1 (the one real request above); series: %v",
				path, got, series)
		}
	}

	// The regression: the deep operation keeps a measurable series of its
	// own, labeled by its mount's seeded prefix -- never the overflow
	// bucket. Pre-fix the request lands in {overflow} and no
	// /api/v1/authn series exists at all.
	if got := series["/api/v1/authn"]; got != 1 {
		t.Errorf("http.route=%q (the seeded mount prefix the deep operation request /api/v1/authn/login/password must fold onto) recorded %d requests, want exactly 1: the deep operation collapsed to %q despite its mount prefix being seeded",
			"/api/v1/authn", got, obs.RouteLabelOverflowValue)
	}

	// The overflow bucket holds exactly the garbage that overflowed: 356
	// garbage paths minus the 252 the 4-seed-reduced budget tracked.
	// Pre-fix the deep operation's request is in here too, making it one
	// higher.
	const overflowedGarbage = garbagePaths - (obs.MaxRouteLabelValues - seededRoutes)
	if got := series[obs.RouteLabelOverflowValue]; got != overflowedGarbage {
		t.Errorf("overflow bucket recorded %d requests, want %d (the garbage paths beyond the seeded budget; pre-fix the deep operation's request lands here too, making it %d)",
			got, overflowedGarbage, overflowedGarbage+1)
	}

	// No request was lost or double-counted: 356 garbage + 3 real.
	if totalRecorded != int64(garbagePaths+3) {
		t.Errorf("sum of all recorded requests = %d, want %d", totalRecorded, garbagePaths+3)
	}

	// Boundedness holds all the same (folding must not grow the bound):
	// the 4 seeds reserve 4 of the 256 slots, 252 garbage paths are
	// tracked verbatim, the 3 requested real routes emit their pre-seeded
	// series and the remaining 104 garbage paths share the single
	// overflow bucket -- 252 tracked garbage + 3 requested real routes +
	// 1 overflow bucket = obs.MaxRouteLabelValues distinct series, the
	// same ceiling the exact-match seed enforced. Pre-fix the deep
	// operation emits nothing of its own, so only 255 series exist.
	if got := len(counter.DataPoints); got != obs.MaxRouteLabelValues {
		t.Errorf("got %d distinct series, want exactly %d (252 tracked garbage + 3 requested real routes + 1 overflow bucket): folding onto seeded prefixes must not grow the series bound",
			got, obs.MaxRouteLabelValues)
	}
}

// TestMiddleware_SeededMountPrefix_FoldsRequestsAtOrBelowIt pins the
// subtree semantics the seed's PREFIX interpretation implements in the
// quiet case (no garbage, the budget nowhere near exhaustion): a seeded
// entry is the label for every request AT its path or BELOW it, matching
// net/http ServeMux subtree mounting with a "/"-segment boundary (so
// "/api/v1/notesXYZ" is not below a "/api/v1/notes" mount), and when two
// seeds overlap the deeper mount wins for its own subtree (an "/api/v1"
// mount and an "/api/v1/notes" mount nested under it each label their
// own traffic). A request under NO seeded mount keeps the ordinary
// exact-record behavior, minting its own label while the budget allows.
// The fold is unconditional rather than reserved for a full budget,
// which is what keeps labels deterministic -- the same series for the
// same operation whatever order traffic and garbage arrive in -- and
// what keeps attacker garbage below a seeded prefix from ever minting a
// label at all. Fails before the fix (verified): the exact-match seed
// leaves /api/v1/notes/abc, /api/v1/notesXYZ and /api/v1/billing/xyz to
// each mint their own verbatim series, so the /api/v1/notes series
// counts 1, no /api/v1 series exists and a "/api/v1/notes/abc" series
// does.
func TestMiddleware_SeededMountPrefix_FoldsRequestsAtOrBelowIt(t *testing.T) {
	reader := setupMeterProvider(t)

	// Two overlapping seeds -- a broad "/api/v1" mount and the narrower
	// "/api/v1/notes" module mount registered under it (the registry
	// allows a host to register both; the more specific entry must win
	// for its own subtree) -- plus one host-level leaf route.
	obs.RegisterMountedRoutes([]pkgcore.MountedRoute{
		{Path: "/api/v1"},
		{Path: "/api/v1/notes"},
		{Path: "/healthz"},
	})
	t.Cleanup(func() { obs.RegisterMountedRoutes(nil) })

	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, path := range []string{
		"/healthz",            // the seeded leaf, requested at its own path
		"/api/v1/notes",       // the narrow mount, requested at its own path
		"/api/v1/notes/abc",   // below the narrow mount: folds onto /api/v1/notes
		"/api/v1/notesXYZ",    // not below the notes mount (no "/" boundary), but below /api/v1
		"/api/v1/billing/xyz", // below the broad mount only: folds onto /api/v1
		"/outside/garbage-1",  // under no seeded mount: ordinary exact-record
	} {
		handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, path, ""))
	}

	rm := collect(t, reader)
	counter := findSum(t, findMetric(t, rm, requestCountMetricName))

	series := map[string]int64{}
	for _, dp := range counter.DataPoints {
		series[labelMap(dp.Attributes)["http.route"]] += dp.Value
	}

	if got := series["/api/v1/notes"]; got != 2 {
		t.Errorf("http.route=%q recorded %d requests, want 2 (the mount's own path and the request below it both fold onto it; pre-fix the below-mount request minted its own verbatim series instead)",
			"/api/v1/notes", got)
	}
	if got := series["/api/v1"]; got != 2 {
		t.Errorf("http.route=%q recorded %d requests, want 2 (the sibling-shaped /api/v1/notesXYZ path and /api/v1/billing/xyz, both below the broad mount but not the narrow one; pre-fix no /api/v1 series existed at all)",
			"/api/v1", got)
	}
	if got := series["/healthz"]; got != 1 {
		t.Errorf("http.route=%q recorded %d requests, want 1 (the seeded leaf requested at its own path)", "/healthz", got)
	}
	if _, ok := series["/api/v1/notes/abc"]; ok {
		t.Errorf("request /api/v1/notes/abc minted its own verbatim series: below a seeded mount it must fold onto the mount's label instead (the pre-fix exact-match behavior); series: %v", series)
	}
	if _, ok := series["/api/v1/notesXYZ"]; ok {
		t.Errorf("request /api/v1/notesXYZ minted its own verbatim series: it lies below the seeded /api/v1 mount and must fold there; only a path below NO seeded mount may mint; series: %v", series)
	}
	if got := series["/outside/garbage-1"]; got != 1 {
		t.Errorf("http.route=%q recorded %d requests, want 1: a request under no seeded mount keeps the ordinary exact-record behavior",
			"/outside/garbage-1", got)
	}
}

// erroringMeterProvider and erroringMeter stand in for a MeterProvider
// whose instrument construction fails, so
// TestMiddleware_InstrumentConstructionError_IsReported can prove the
// construction errors Middleware used to drop (requestCount, _ := ...)
// are now routed to OTel's global error handler. The real SDK only errors
// on genuinely invalid instrument configurations (and validates silently
// where these probes would need it to fail), so the failure has to be
// injected; embedding noop.Meter keeps the rest of the interface live for
// anything Middleware (or a future edit of it) may call that this test
// does not anticipate.
type erroringMeterProvider struct {
	metric.MeterProvider
}

type erroringMeter struct {
	noop.Meter
}

func (erroringMeterProvider) Meter(string, ...metric.MeterOption) metric.Meter {
	return erroringMeter{}
}

func (erroringMeter) Int64Counter(name string, _ ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	// The meter contract hands back a no-op instrument alongside the
	// error, exactly like the real SDK does for an invalid registration;
	// recording into it stays safe, which is what lets Middleware report
	// the error instead of failing.
	return noop.Int64Counter{}, fmt.Errorf("probe: cannot create counter %q", name)
}

func (erroringMeter) Float64Histogram(name string, _ ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	return noop.Float64Histogram{}, fmt.Errorf("probe: cannot create histogram %q", name)
}

// TestMiddleware_InstrumentConstructionError_IsReported is the regression
// for Middleware dropping its instrument-construction errors
// (requestCount, _ := meter.Int64Counter(...)): a construction error means
// the meter handed back a no-op instrument, so requests would go
// uncounted or untimed with no startup signal at all -- the silent-failure
// mode a future rename or collision of these instrument names would
// otherwise produce. Middleware now routes each construction error to
// OTel's global error handler (stderr by default; captured here via
// otel.SetErrorHandler), naming the instrument that failed. Fails before
// the fix (verified): the capturing error handler is never invoked.
func TestMiddleware_InstrumentConstructionError_IsReported(t *testing.T) {
	var mu sync.Mutex
	var reported []error
	previous := otel.GetErrorHandler()
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		mu.Lock()
		defer mu.Unlock()
		reported = append(reported, err)
	}))
	t.Cleanup(func() { otel.SetErrorHandler(previous) })

	otel.SetMeterProvider(erroringMeterProvider{})

	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// Serving still works with the no-op instruments the erroring meter
	// returned -- recording into them is safe, which is why the errors can
	// be reported rather than fatal.
	handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, "/api/v1/notes", ""))

	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 2 {
		t.Fatalf("expected Middleware to report both instrument-construction errors to the OTel error handler, got %d: %v", len(reported), reported)
	}
	var sawCounter, sawDuration bool
	for _, err := range reported {
		msg := err.Error()
		if strings.Contains(msg, requestCountMetricName) {
			sawCounter = true
		}
		if strings.Contains(msg, requestDurationMetricName) {
			sawDuration = true
		}
	}
	if !sawCounter || !sawDuration {
		t.Errorf("reported errors must name the failing instruments (%q and %q); got: %v", requestCountMetricName, requestDurationMetricName, reported)
	}
}

// findAttr looks up key in a span's recorded attributes.
func findAttr(attrs []attribute.KeyValue, key string) (attribute.Value, bool) {
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

// mapsEqual compares two string maps for equality without pulling in
// reflect.DeepEqual's less specific failure output.
func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
