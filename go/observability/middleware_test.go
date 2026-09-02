package observability_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// The Prometheus names the metrics in middleware.go end up under, once the
// exporter appends its unit/counter-suffix translation. Confirmed against
// the pinned go.opentelemetry.io/otel/exporters/prometheus version rather
// than assumed -- see the package's implementation comment on
// requestCountName / requestDurationName for the OTel-side names these
// derive from.
const (
	promRequestCountFamily    = "http_server_request_count_total"
	promRequestDurationFamily = "http_server_request_duration_seconds"
)

// setupMeterProvider installs, as OTel's global MeterProvider for the
// duration of the test, a real SDK MeterProvider backed by a private
// Prometheus registry (never prometheus.DefaultRegisterer, so repeated
// calls across this file's tests never collide on "duplicate metrics
// collector registration"). It returns the registry to Gather() from.
func setupMeterProvider(t *testing.T) *prometheus.Registry {
	t.Helper()
	reg := prometheus.NewRegistry()
	exp, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		t.Fatalf("build prometheus exporter: %v", err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	otel.SetMeterProvider(mp)
	return reg
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

// findFamily returns the metric family named name, failing the test if it
// is missing.
func findFamily(t *testing.T, families []*dto.MetricFamily, name string) *dto.MetricFamily {
	t.Helper()
	for _, mf := range families {
		if mf.GetName() == name {
			return mf
		}
	}
	var got []string
	for _, mf := range families {
		got = append(got, mf.GetName())
	}
	t.Fatalf("metric family %q not found; families present: %v", name, got)
	return nil
}

// labelMap flattens a gathered metric's label pairs into a map, dropping
// the otel_scope_* bookkeeping labels the Prometheus exporter attaches to
// every series (they identify the meter, not this middleware's request
// attributes, and are irrelevant to what this file asserts on).
func labelMap(m *dto.Metric) map[string]string {
	out := make(map[string]string, len(m.GetLabel()))
	for _, lp := range m.GetLabel() {
		if lp.GetName() == "otel_scope_name" || lp.GetName() == "otel_scope_schema_url" || lp.GetName() == "otel_scope_version" {
			continue
		}
		out[lp.GetName()] = lp.GetValue()
	}
	return out
}

// assertNoTenantLabelAnywhere is the blanket negative control this whole
// file is built around: no metric family Middleware feeds, under any
// circumstance, may carry a tenant_id label. This is checked independently
// of, and in addition to, the more specific series-count assertions below,
// so a future attribute this middleware grows cannot reintroduce tenant_id
// through a code path the more targeted assertions do not happen to cover.
func assertNoTenantLabelAnywhere(t *testing.T, families []*dto.MetricFamily) {
	t.Helper()
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == obs.TenantIDKey {
					t.Fatalf("metric family %q carries a %q label (value %q): tenant_id must never be a Prometheus metric label (CLAUDE.md, docs/internal/09-observability.md)",
						mf.GetName(), obs.TenantIDKey, lp.GetValue())
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
	reg := setupMeterProvider(t)
	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, "/api/v1/notes", ""))

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	assertNoTenantLabelAnywhere(t, families)

	counter := findFamily(t, families, promRequestCountFamily)
	if got := len(counter.GetMetric()); got != 1 {
		t.Fatalf("expected exactly 1 counter series, got %d", got)
	}
	wantLabels := map[string]string{
		"http_request_method":       "GET",
		"http_route":                "/api/v1/notes",
		"http_response_status_code": "200",
	}
	if got := labelMap(counter.GetMetric()[0]); !mapsEqual(got, wantLabels) {
		t.Errorf("counter labels = %v, want %v", got, wantLabels)
	}
	if got := counter.GetMetric()[0].GetCounter().GetValue(); got != 1 {
		t.Errorf("counter value = %v, want 1", got)
	}

	duration := findFamily(t, families, promRequestDurationFamily)
	if got := len(duration.GetMetric()); got != 1 {
		t.Fatalf("expected exactly 1 duration series, got %d", got)
	}
	if got := duration.GetMetric()[0].GetHistogram().GetSampleCount(); got != 1 {
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
	reg := setupMeterProvider(t)
	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, tenant := range []pkgcore.TenantID{"tenant-a", "tenant-b"} {
		handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, "/api/v1/notes", tenant))
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	assertNoTenantLabelAnywhere(t, families)

	counter := findFamily(t, families, promRequestCountFamily)
	if got := len(counter.GetMetric()); got != 1 {
		t.Fatalf("expected the two tenants' requests to collapse into exactly 1 series, got %d series: %v",
			got, counter.GetMetric())
	}
	if got := counter.GetMetric()[0].GetCounter().GetValue(); got != 2 {
		t.Errorf("expected the single series to aggregate both tenants' requests (value 2), got %v", got)
	}

	duration := findFamily(t, families, promRequestDurationFamily)
	if got := len(duration.GetMetric()); got != 1 {
		t.Fatalf("expected the two tenants' requests to collapse into exactly 1 duration series, got %d", got)
	}
	if got := duration.GetMetric()[0].GetHistogram().GetSampleCount(); got != 2 {
		t.Errorf("expected the single duration series to aggregate both tenants' requests (2 samples), got %v", got)
	}
}

// TestMiddleware_DifferentRouteOrStatus_ProducesSeparateSeries is the
// positive-control complement to the test above: it proves the low-
// cardinality labels this middleware DOES use are not accidentally
// collapsed to nothing either -- a middleware that dropped every
// attribute would also pass a "no forking" check vacuously.
func TestMiddleware_DifferentRouteOrStatus_ProducesSeparateSeries(t *testing.T) {
	reg := setupMeterProvider(t)
	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, "/api/v1/notes", ""))
	handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, "/missing", ""))

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	counter := findFamily(t, families, promRequestCountFamily)
	if got := len(counter.GetMetric()); got != 2 {
		t.Fatalf("expected 2 distinct series for 2 distinct (route, status) pairs, got %d: %v", got, counter.GetMetric())
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
	reg := setupMeterProvider(t)
	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	handler.ServeHTTP(httptest.NewRecorder(), newTestRequest(http.MethodGet, "/api/v1/notes", ""))

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	counter := findFamily(t, families, promRequestCountFamily)
	got := labelMap(counter.GetMetric()[0])
	if got["http_response_status_code"] != "200" {
		t.Errorf("status_code label = %q, want %q (implicit OK)", got["http_response_status_code"], "200")
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
