package observability

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// defaultServiceName is the service.name resource attribute used when the
// caller does not supply WithServiceName.
const defaultServiceName = "speed"

// localStdoutMetricInterval is how often the local exporters' stdout metric
// reader dumps a snapshot. The pull-based Prometheus reader wired alongside
// it (see initLocalExporters) is unaffected by this value: it collects
// synchronously from the SDK on every /metrics scrape, so MetricsHandler's
// own output is never stale by up to this interval the way the stdout
// reader's periodic snapshots are.
const localStdoutMetricInterval = 10 * time.Second

// Config holds Init's tunables, assembled from the Option values passed to
// it. Callers do not construct a Config directly; Init always starts from
// its own documented defaults before applying opts.
type Config struct {
	// ServiceName becomes the service.name resource attribute every span
	// and metric is tagged with. Defaults to "speed".
	ServiceName string

	// OTLPEndpoint is the "host:port" target (see otlptracegrpc's own doc
	// comment for the full accepted syntax) Init dials for both traces and
	// metrics when the host wants its signals pushed to a collector over
	// OTLP/gRPC. A non-empty value -- supplied via WithOTLPEndpoint -- is
	// what switches Init onto the OTLP exporters; empty (the default)
	// keeps Init on the local exporters. Deployment mode plays no role in
	// that decision (docs/internal/03-deployment-modes.md: mode and
	// implementation composition are two orthogonal axes, and exporter
	// choice is an implementation-composition question, not a
	// mode question).
	//
	// This package deliberately does not read it from the environment
	// itself. go/pkgcore/config's bootstrap loader is the natural home
	// for a value like this once a host wires that loader in; until then
	// a host resolves it exactly how it resolves its other bootstrap
	// settings and passes it here via WithOTLPEndpoint. This field is
	// the seam a real host wires up.
	//
	// examples/reference-app does not yet demonstrate that wiring:
	// cmd/server/server.go's configFromEnv reads only
	// SPEED_DEPLOYMENT_MODE, PORT and SPEED_DB_PATH, and
	// cmd/server/main.go's run calls Init with no WithOTLPEndpoint
	// option, so that example's observability stays on the local
	// exporters -- not because its deployment mode requires it, but
	// because no host code resolves an endpoint to hand over. There is
	// no SPEED_OTLP_ENDPOINT (or equivalent) anywhere in this repository
	// today; a host wiring real production OTLP export starts this
	// field's resolution from scratch, not from an existing example to
	// copy.
	OTLPEndpoint string

	// OTLPInsecure disables gRPC transport security for the OTLP
	// connection. Defaults to false (secure), matching
	// otlptracegrpc/otlpmetricgrpc's own default; set it when the
	// configured collector is reached over a private, unencrypted network
	// path, as docs/internal/09-observability.md's plain-container LGTM
	// stack typically is. It has no effect when no OTLP endpoint is
	// configured.
	OTLPInsecure bool
}

// Option customises the Config Init builds providers from.
type Option func(*Config)

// WithServiceName overrides the default "speed" service.name resource
// attribute.
func WithServiceName(name string) Option {
	return func(c *Config) { c.ServiceName = name }
}

// WithOTLPEndpoint sets the OTLP/gRPC endpoint Init dials when the host
// wants traces and metrics pushed to a collector: supplying a non-empty
// endpoint is what wires the OTLP exporters (see Init's own doc comment),
// and an empty endpoint is treated as no endpoint. Deployment mode plays
// no role in the decision. See Config.OTLPEndpoint's doc comment for why
// this package takes it as an explicit option rather than reading it from
// the environment itself.
func WithOTLPEndpoint(endpoint string) Option {
	return func(c *Config) { c.OTLPEndpoint = endpoint }
}

// WithOTLPInsecure disables gRPC transport security for the OTLP
// connection Init dials when an endpoint is configured. See
// Config.OTLPInsecure's doc comment.
func WithOTLPInsecure(insecure bool) Option {
	return func(c *Config) { c.OTLPInsecure = insecure }
}

// metricsHandlerMu guards currentMetricsHandler. Init runs once per
// process in normal operation, but this package's own tests call it
// repeatedly, so the handler is mutex-protected rather than a bare package
// variable.
var (
	metricsHandlerMu      sync.Mutex
	currentMetricsHandler http.Handler = http.HandlerFunc(metricsUnavailable)
)

// MetricsHandler returns the http.Handler the most recent Init call wired
// up for /metrics: a real Prometheus scrape endpoint when Init wired the
// local exporters (no OTLP endpoint configured), and a 404 explaining that
// metrics are pushed via OTLP instead of pulled locally when it wired the
// OTLP exporters -- or before Init has run at all.
//
// See examples/reference-app/cmd/server/server.go for where a host mounts
// this at /metrics.
func MetricsHandler() http.Handler {
	metricsHandlerMu.Lock()
	defer metricsHandlerMu.Unlock()
	return currentMetricsHandler
}

// setMetricsHandler installs h as MetricsHandler's return value.
func setMetricsHandler(h http.Handler) {
	metricsHandlerMu.Lock()
	defer metricsHandlerMu.Unlock()
	currentMetricsHandler = h
}

// metricsUnavailableBody is the response body MetricsHandler serves when
// the OTLP exporters are wired, and before Init has run.
const metricsUnavailableBody = "observability: no local /metrics endpoint; metrics are pushed via OTLP to the configured collector\n"

// metricsUnavailable is the handler installed whenever there is no local
// Prometheus registry to serve -- see metricsUnavailableBody.
func metricsUnavailable(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, metricsUnavailableBody, http.StatusNotFound)
}

// Init wires a TracerProvider and MeterProvider and installs them as
// OpenTelemetry's global providers (otel.SetTracerProvider /
// otel.SetMeterProvider), so that Middleware, FromContext's span lookup,
// and any future module's own otel.Tracer/otel.Meter calls all reach them
// without a provider threaded through every call site. It returns a
// shutdown function the caller must invoke during graceful process
// shutdown to flush and close both providers -- see
// examples/reference-app/cmd/server/main.go's run function for the
// pattern this mirrors, matching the http.Server graceful-shutdown
// sequence already used there.
//
// Which exporters get wired is decided by whether the caller supplied an
// OTLP endpoint -- nothing else. Init takes no deployment mode and never
// branches on one (docs/internal/03-deployment-modes.md: mode and
// implementation composition are two orthogonal axes, and root
// CLAUDE.md's rule confines mode differences to kernel wiring; this
// package is not kernel wiring). A host that wants its telemetry pushed
// to a collector simply supplies the endpoint; a host that does not gets
// the local exporters. Both choices work in a single-process composition
// and in a multi-replica one alike.
//
// With no WithOTLPEndpoint option, Init wires the local exporters. They
// need no configuration and no external process: traces are written to
// stdout (stdouttrace), and metrics are written both to stdout
// (stdoutmetric, periodically, for a developer tailing the process) and to
// an in-process Prometheus registry exposed through MetricsHandler for the
// host to mount at /metrics.
//
// With a non-empty endpoint supplied via WithOTLPEndpoint, Init wires the
// OTLP/gRPC exporters instead: both signals are pushed to that endpoint,
// and MetricsHandler reports 404, since there is no local Prometheus
// registry to scrape.
func Init(ctx context.Context, opts ...Option) (func(context.Context) error, error) {
	cfg := Config{ServiceName: defaultServiceName}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", cfg.ServiceName),
	))
	if err != nil {
		return nil, fmt.Errorf("observability: build resource: %w", err)
	}

	if cfg.OTLPEndpoint != "" {
		return initOTLPExporters(ctx, cfg, res)
	}
	return initLocalExporters(res)
}

// initLocalExporters wires the local exporter set: traces to stdout and
// metrics both to stdout and to an in-process Prometheus registry. See
// Init's own doc comment.
func initLocalExporters(res *resource.Resource) (func(context.Context) error, error) {
	traceExporter, err := stdouttrace.New(stdouttrace.WithWriter(os.Stdout))
	if err != nil {
		return nil, fmt.Errorf("observability: build stdout trace exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		// WithSyncer exports each span synchronously as it ends, instead
		// of batching: this exporter set's whole point is zero-delay
		// visibility for a developer watching stdout, and a low-traffic
		// process with no collector configured has no throughput concern
		// batching would address.
		sdktrace.WithSyncer(traceExporter),
		sdktrace.WithResource(res),
	)

	stdoutExporter, err := stdoutmetric.New(stdoutmetric.WithWriter(os.Stdout))
	if err != nil {
		_ = tp.Shutdown(context.Background())
		return nil, fmt.Errorf("observability: build stdout metric exporter: %w", err)
	}

	// A registry of this Init call's own, rather than
	// prometheus.DefaultRegisterer: the default registerer is a single
	// process-wide global, so a second Init call in the same process --
	// every test in this package that exercises Init's no-endpoint path
	// does exactly this -- would panic with "duplicate metrics collector
	// registration" the moment it tried to register the same instrument
	// names again. A fresh registry per call sidesteps that entirely and
	// keeps repeated Init calls independent.
	registry := prometheus.NewRegistry()
	promExporter, err := otelprom.New(otelprom.WithRegisterer(registry))
	if err != nil {
		_ = tp.Shutdown(context.Background())
		return nil, fmt.Errorf("observability: build prometheus metric exporter: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(stdoutExporter, sdkmetric.WithInterval(localStdoutMetricInterval))),
		// promExporter is itself an sdkmetric.Reader: registering it
		// directly, rather than wrapping it in a PeriodicReader, is what
		// makes it pull-based -- see this function's own doc comment on
		// MetricsHandler's freshness above.
		sdkmetric.WithReader(promExporter),
	)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	setMetricsHandler(promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	shutdown := func(shutdownCtx context.Context) error {
		setMetricsHandler(http.HandlerFunc(metricsUnavailable))
		return errors.Join(tp.Shutdown(shutdownCtx), mp.Shutdown(shutdownCtx))
	}
	return shutdown, nil
}

// initOTLPExporters wires the OTLP/gRPC exporter set: both signals are
// pushed to the configured endpoint. See Init's own doc comment.
func initOTLPExporters(ctx context.Context, cfg Config, res *resource.Resource) (func(context.Context) error, error) {
	traceOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint)}
	metricOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint)}
	if cfg.OTLPInsecure {
		traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
		metricOpts = append(metricOpts, otlpmetricgrpc.WithInsecure())
	}

	traceExporter, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return nil, fmt.Errorf("observability: build OTLP trace exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
	)

	metricExporter, err := otlpmetricgrpc.New(ctx, metricOpts...)
	if err != nil {
		_ = tp.Shutdown(context.Background())
		return nil, fmt.Errorf("observability: build OTLP metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
	)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	setMetricsHandler(http.HandlerFunc(metricsUnavailable))

	shutdown := func(shutdownCtx context.Context) error {
		return errors.Join(tp.Shutdown(shutdownCtx), mp.Shutdown(shutdownCtx))
	}
	return shutdown, nil
}
