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

	"github.com/vislake/speed/go/pkgcore"
)

// defaultServiceName is the service.name resource attribute used when the
// caller does not supply WithServiceName.
const defaultServiceName = "speed"

// standaloneStdoutMetricInterval is how often the standalone deployment
// mode's stdout metric reader dumps a snapshot. The pull-based Prometheus
// reader wired alongside it (see initStandalone) is unaffected by this
// value: it collects synchronously from the SDK on every /metrics scrape,
// so MetricsHandler's own output is never stale by up to this interval the
// way the stdout reader's periodic snapshots are.
const standaloneStdoutMetricInterval = 10 * time.Second

// ErrMissingOTLPEndpoint is returned by Init when mode is
// DeploymentModeDistributed and no OTLP endpoint was supplied via
// WithOTLPEndpoint.
var ErrMissingOTLPEndpoint = errors.New("observability: distributed deployment mode requires an OTLP endpoint (see WithOTLPEndpoint)")

// Config holds Init's tunables, assembled from the Option values passed to
// it. Callers do not construct a Config directly; Init always starts from
// its own documented defaults before applying opts.
type Config struct {
	// ServiceName becomes the service.name resource attribute every span
	// and metric is tagged with. Defaults to "speed".
	ServiceName string

	// OTLPEndpoint is the "host:port" target (see otlptracegrpc's own doc
	// comment for the full accepted syntax) a DeploymentModeDistributed
	// Init dials for both traces and metrics. Required for
	// DeploymentModeDistributed; ignored for DeploymentModeStandalone.
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
	// cmd/server/main.go's run calls Init
	// with no WithOTLPEndpoint option. buildServer rejects every
	// deployment mode except pkgcore.DeploymentModeStandalone before
	// Init ever runs, so that example never reaches the
	// DeploymentModeDistributed path this field configures, and there is
	// no SPEED_OTLP_ENDPOINT (or equivalent) anywhere in this repository
	// today. A host wiring real production OTLP export starts this
	// field's resolution from scratch, not from an existing example to
	// copy.
	OTLPEndpoint string

	// OTLPInsecure disables gRPC transport security for the OTLP
	// connection. Defaults to false (secure), matching
	// otlptracegrpc/otlpmetricgrpc's own default; set it when the
	// configured collector is reached over a private, unencrypted network
	// path, as docs/internal/09-observability.md's plain-container LGTM
	// stack typically is.
	OTLPInsecure bool
}

// Option customises the Config Init builds providers from.
type Option func(*Config)

// WithServiceName overrides the default "speed" service.name resource
// attribute.
func WithServiceName(name string) Option {
	return func(c *Config) { c.ServiceName = name }
}

// WithOTLPEndpoint sets the OTLP/gRPC endpoint a DeploymentModeDistributed
// Init dials. See Config.OTLPEndpoint's doc comment for why this package
// takes it as an explicit option rather than reading it from the
// environment itself.
func WithOTLPEndpoint(endpoint string) Option {
	return func(c *Config) { c.OTLPEndpoint = endpoint }
}

// WithOTLPInsecure disables gRPC transport security for the OTLP
// connection a DeploymentModeDistributed Init dials. See
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
// up for /metrics: a real Prometheus scrape endpoint for
// DeploymentModeStandalone, and a 404 explaining that metrics are pushed
// via OTLP instead of pulled locally for DeploymentModeDistributed, or
// before Init has run at all.
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

// metricsUnavailableBody is the response body MetricsHandler serves for
// DeploymentModeDistributed, and before Init has run.
const metricsUnavailableBody = "observability: no local /metrics endpoint in this deployment mode; metrics are pushed via OTLP to the configured collector\n"

// metricsUnavailable is the handler installed whenever there is no local
// Prometheus registry to serve -- see metricsUnavailableBody.
func metricsUnavailable(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, metricsUnavailableBody, http.StatusNotFound)
}

// Init wires a TracerProvider and MeterProvider for mode and installs
// them as OpenTelemetry's global providers (otel.SetTracerProvider /
// otel.SetMeterProvider), so that Middleware, FromContext's span lookup,
// and any future module's own otel.Tracer/otel.Meter calls all reach them
// without a provider threaded through every call site. It returns a
// shutdown function the caller must invoke during graceful process
// shutdown to flush and close both providers -- see
// examples/reference-app/cmd/server/main.go's run function for the
// pattern this mirrors, matching the http.Server graceful-shutdown
// sequence already used there.
//
// DeploymentModeStandalone needs no configuration and no external
// process: traces are written to stdout (stdouttrace), and metrics are
// written both to stdout (stdoutmetric, periodically, for a developer
// tailing the process) and to an in-process Prometheus registry exposed
// through MetricsHandler for the host to mount at /metrics.
//
// DeploymentModeDistributed requires opts to supply an OTLP endpoint (see
// WithOTLPEndpoint); Init returns an error wrapping
// ErrMissingOTLPEndpoint otherwise. Both signals are pushed over OTLP/gRPC
// to that endpoint; MetricsHandler continues to report 404 in this
// deployment mode since there is no local Prometheus registry to scrape.
//
// An unrecognized deployment mode is reported as an error wrapping
// pkgcore.ErrInvalidDeploymentMode, matching how go/pkgcore's own Kernel
// reports the same condition.
func Init(ctx context.Context, mode pkgcore.DeploymentMode, opts ...Option) (func(context.Context) error, error) {
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

	switch mode {
	case pkgcore.DeploymentModeStandalone:
		return initStandalone(res)
	case pkgcore.DeploymentModeDistributed:
		return initDistributed(ctx, cfg, res)
	default:
		return nil, fmt.Errorf("observability: %w: %q", pkgcore.ErrInvalidDeploymentMode, mode)
	}
}

// initStandalone wires DeploymentModeStandalone's exporters. See Init's
// own doc comment.
func initStandalone(res *resource.Resource) (func(context.Context) error, error) {
	traceExporter, err := stdouttrace.New(stdouttrace.WithWriter(os.Stdout))
	if err != nil {
		return nil, fmt.Errorf("observability: build stdout trace exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		// WithSyncer exports each span synchronously as it ends, instead
		// of batching: the standalone deployment mode's whole point is
		// zero-delay visibility for a developer watching stdout, and a
		// low-traffic standalone process has no throughput concern
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
	// every test in this package that exercises DeploymentModeStandalone
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
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(stdoutExporter, sdkmetric.WithInterval(standaloneStdoutMetricInterval))),
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

// initDistributed wires DeploymentModeDistributed's exporters. See Init's
// own doc comment.
func initDistributed(ctx context.Context, cfg Config, res *resource.Resource) (func(context.Context) error, error) {
	if cfg.OTLPEndpoint == "" {
		return nil, ErrMissingOTLPEndpoint
	}

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
