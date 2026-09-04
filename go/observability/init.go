package observability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
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
	// cmd/server/server.go's configFromEnv reads the five bootstrap
	// variables SPEED_DEPLOYMENT_MODE, PORT, SPEED_DB_PATH,
	// SPEED_CONFIG_KEY and SPEED_REDIS_ADDR, and cmd/server/main.go's
	// run calls Init with no WithOTLPEndpoint option, so that example's
	// observability stays on the local exporters -- not because its
	// deployment mode requires it, but because no host code resolves an
	// endpoint to hand over. There is no SPEED_OTLP_ENDPOINT (or
	// equivalent) anywhere in this repository today; a host wiring real
	// production OTLP export follows the shape SPEED_REDIS_ADDR already
	// demonstrates (resolve a bootstrap setting in configFromEnv, hand
	// it over through the matching option) but starts this field's
	// resolution from scratch rather than copying an existing example.
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

// ErrOTLPExporterNotRegistered is what Init returns when a caller supplies
// WithOTLPEndpoint but nothing has called RegisterOTLPExporters -- the
// actionable fix it names is exactly what go/observability/exporter/otlp's
// init() function does the moment a host blank-imports that subpackage. It
// is exported (rather than a package-private sentinel) so a caller -- and
// this package's own tests, which must prove this path from a test binary
// that never blank-imports exporter/otlp -- can assert on it with
// errors.Is instead of matching its message text.
var ErrOTLPExporterNotRegistered = errors.New(`observability: OTLP endpoint configured but no OTLP exporter registered -- blank-import "github.com/vislake/speed/go/observability/exporter/otlp"`)

// otlpFactory is the constructor Init calls to build the OTLP/gRPC
// exporter set once a caller supplies WithOTLPEndpoint. It starts out nil
// -- Init's own doc comment on WithOTLPEndpoint's actionable error
// describes the consequence -- and is installed by
// RegisterOTLPExporters, which go/observability/exporter/otlp's init()
// function calls as its own side effect. See RegisterOTLPExporters' doc
// comment for why this is a single registration slot, not a name-keyed
// registry.
var otlpFactory func(ctx context.Context, cfg Config, res *resource.Resource) (func(context.Context) error, error)

// RegisterOTLPExporters installs f as the constructor Init calls when a
// caller supplies a non-empty WithOTLPEndpoint. It mirrors database/sql's
// driver-registration pattern (also the shape go/pkgcore's SeamRegistry
// follows, though this package's needs are simpler: there is exactly one
// OTLP exporter implementation this repository ships, so a single package
// variable, not a name-keyed registry, is enough): exactly one subpackage,
// go/observability/exporter/otlp, calls this from its own init() function
// in any real binary, which is what a host importing that subpackage
// purely for its side effect --
//
//	import _ "github.com/vislake/speed/go/observability/exporter/otlp"
//
// -- actually arranges. Doing so is what keeps otlptracegrpc,
// otlpmetricgrpc, and everything they pull in (gRPC, protobuf, and their
// own transitive trees) out of this root package's own import graph: a
// consumer that never imports that subpackage carries none of it, proven
// by this module's measured indirect-dependency count (see this package's
// AGENTS.md).
//
// Last registration wins, matching database/sql's own documented
// convention for a second driver registered under a name already taken.
// This is deliberately not safe against two different OTLP exporter
// packages both registering in the same binary -- the second call silently
// replaces the first, with no error and no panic -- which is an accepted
// limitation given a real binary is expected to blank-import exactly one
// such subpackage, exactly as database/sql accepts for a duplicate driver
// name.
func RegisterOTLPExporters(f func(ctx context.Context, cfg Config, res *resource.Resource) (func(context.Context) error, error)) {
	otlpFactory = f
}

// metricsReaderFactory is the constructor initLocalExporters calls to
// build a pull-based local metrics reader and its HTTP scrape handler. It
// starts out nil -- in which case initLocalExporters wires no local
// scrape endpoint at all, and MetricsHandler answers the same
// not-configured 404 the OTLP path uses -- and is installed by
// RegisterLocalMetricsReader, which
// go/observability/exporter/prometheus's init() function calls as its own
// side effect.
var metricsReaderFactory func() (sdkmetric.Reader, http.Handler, error)

// RegisterLocalMetricsReader installs f as the constructor
// initLocalExporters calls to build a local, pull-based metrics reader
// (registered on the MeterProvider Init assembles) and the http.Handler
// that serves it -- what MetricsHandler returns once registered. See
// RegisterOTLPExporters' doc comment for the registration convention this
// mirrors (single slot, last registration wins, not safe against two
// different implementations registering at once).
//
// This is the mechanism that makes local Prometheus scraping opt-in
// rather than Init's unconditional default: go/observability/exporter/prometheus
// is the one subpackage that calls this today, from its own init()
// function, so a host that wants a local /metrics endpoint blank-imports
// it --
//
//	import _ "github.com/vislake/speed/go/observability/exporter/prometheus"
//
// -- and a host that does not never pulls github.com/prometheus/client_golang
// into its import graph at all. Without any registration, Init's
// no-endpoint default still wires the stdout trace and metric exporters
// (the OTel SDK's own stdout exporters are not a third-party dependency
// for a package that already requires the OTel SDK core), so a host with
// nothing registered is never left with no local telemetry at all --
// only with no local *pull-based scrape* endpoint.
func RegisterLocalMetricsReader(f func() (sdkmetric.Reader, http.Handler, error)) {
	metricsReaderFactory = f
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
// up for /metrics: a real local scrape endpoint when Init wired the local
// exporters AND a metrics reader was registered via
// RegisterLocalMetricsReader (see go/observability/exporter/prometheus),
// and a 404 explaining why not otherwise -- the OTLP exporters were wired
// instead (metrics are pushed, not pulled), no local reader was
// registered, or Init has not run at all yet.
//
// See examples/reference-app/cmd/server/server.go for where a host mounts
// this at /metrics, and blank-imports go/observability/exporter/prometheus
// to get a real answer from it.
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

// metricsUnavailableBody is the response body MetricsHandler serves
// whenever there is no local scrape endpoint to answer with: the OTLP
// exporters are wired (metrics are pushed, not pulled), the local
// exporters are wired but no metrics reader was registered via
// RegisterLocalMetricsReader, or Init has not run at all yet. It names
// both ways forward rather than picking one, since which applies depends
// on which of those three states the caller is actually in.
const metricsUnavailableBody = "observability: no local /metrics endpoint; either metrics are pushed via OTLP to a configured collector, or no local scrape reader is registered -- blank-import \"github.com/vislake/speed/go/observability/exporter/prometheus\" for a local scrape endpoint, or configure WithOTLPEndpoint for push-based metrics\n"

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
// stdout (stdouttrace), and metrics are written to stdout too
// (stdoutmetric, periodically, for a developer tailing the process) --
// this much is Init's true zero-third-party-dependency default (the OTel
// SDK's own stdout exporters, not a third-party dependency for a package
// that already requires the OTel SDK core). A local, pull-based
// Prometheus scrape endpoint at MetricsHandler is an OPT-IN addition on
// top of that default, wired only when a metrics reader has been
// registered via RegisterLocalMetricsReader -- which
// go/observability/exporter/prometheus's init() function does, so a host
// that wants one blank-imports that subpackage. Without it,
// MetricsHandler answers a 404 explaining both ways forward (see
// metricsUnavailableBody).
//
// With a non-empty endpoint supplied via WithOTLPEndpoint, Init wires the
// OTLP/gRPC exporters instead -- built by the constructor registered
// through RegisterOTLPExporters, which go/observability/exporter/otlp's
// init() function installs, so a host that wants OTLP export
// blank-imports that subpackage. Without it, Init itself fails with an
// actionable error naming the same import. Once wired, both signals are
// pushed to that endpoint, and MetricsHandler reports 404, since there is
// no local registry to scrape.
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
		if otlpFactory == nil {
			return nil, ErrOTLPExporterNotRegistered
		}
		shutdown, err := otlpFactory(ctx, cfg, res)
		if err != nil {
			return nil, err
		}
		setMetricsHandler(http.HandlerFunc(metricsUnavailable))
		return shutdown, nil
	}
	return initLocalExporters(res)
}

// initLocalExporters wires the local exporter set: traces to stdout
// always, metrics to stdout always plus, when metricsReaderFactory has
// been registered, a local pull-based reader as well. See Init's own doc
// comment for why the local scrape endpoint is opt-in rather than
// unconditional.
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

	mpOpts := []sdkmetric.Option{
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(stdoutExporter, sdkmetric.WithInterval(localStdoutMetricInterval))),
	}

	// metricsHandler defaults to the not-configured 404: a host that never
	// registered a local metrics reader (never blank-imported
	// go/observability/exporter/prometheus, or any future alternative
	// implementation) gets exactly that answer from MetricsHandler, per
	// Init's own doc comment. Unlike the OTLP path above -- which fails
	// Init outright the moment WithOTLPEndpoint is set with no registered
	// exporter -- this default-behavior break cannot fail Init itself: the
	// no-local-reader case is Init's own advertised default, not a
	// misconfiguration, so the signal is a startup log line instead, on
	// the same convention pkgcore's warnIfNotDurable uses for its own
	// non-fatal startup banner (root CLAUDE.md's deployment-composition
	// section) -- a constant message plus key-value attributes via
	// log/slog directly, since nothing has called Init yet to give this
	// package a context-scoped logger of its own (obs.FromContext needs a
	// context already carrying one). Without this line, a host that
	// forgot the blank import had no signal at process start; discovery
	// depended entirely on someone eventually probing /metrics.
	metricsHandler := http.Handler(http.HandlerFunc(metricsUnavailable))
	if metricsReaderFactory != nil {
		reader, handler, err := metricsReaderFactory()
		if err != nil {
			_ = tp.Shutdown(context.Background())
			return nil, fmt.Errorf("observability: build local metrics reader: %w", err)
		}
		// reader is itself an sdkmetric.Reader: registering it directly,
		// rather than wrapping it in a PeriodicReader, is what makes it
		// pull-based -- collected synchronously on every scrape, per
		// MetricsHandler's own doc comment on freshness.
		mpOpts = append(mpOpts, sdkmetric.WithReader(reader))
		metricsHandler = handler
	} else {
		slog.Default().Warn("observability: no local metrics reader registered; /metrics will answer 404 until one is",
			"hint", `blank-import "github.com/vislake/speed/go/observability/exporter/prometheus" for a local scrape endpoint, or configure WithOTLPEndpoint for push-based metrics`,
		)
	}

	mp := sdkmetric.NewMeterProvider(mpOpts...)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	setMetricsHandler(metricsHandler)

	shutdown := func(shutdownCtx context.Context) error {
		setMetricsHandler(http.HandlerFunc(metricsUnavailable))
		return errors.Join(tp.Shutdown(shutdownCtx), mp.Shutdown(shutdownCtx))
	}
	return shutdown, nil
}
