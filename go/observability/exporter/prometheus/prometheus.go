// Package prometheus registers a local, pull-based Prometheus metrics
// reader and its /metrics scrape handler for go/observability. Importing
// it purely for its init() side effect is what a host wanting a local
// scrape endpoint does:
//
//	import _ "github.com/vislake/speed/go/observability/exporter/prometheus"
//
// after which observability.Init's no-endpoint (local exporters) path
// wires the reader this package builds into the MeterProvider, and
// observability.MetricsHandler serves real Prometheus exposition-format
// output instead of the not-configured 404. Without that import, the
// local exporters still run (traces and metrics both go to stdout), but
// MetricsHandler keeps answering 404 -- see go/observability/init.go's
// own doc comment on Init and MetricsHandler.
//
// This package exists to isolate github.com/prometheus/client_golang (and
// its own transitive dependency tree) out of go/observability's root
// package: a consumer that only imports the root package never sees any
// of it in its own go.mod or go.sum, per go/observability/AGENTS.md's
// measured indirect-dependency count. It has no exported API beyond the
// init() side effect -- there is nothing here for a caller to invoke
// directly.
//
// The package's own declared name, "prometheus", collides with
// github.com/prometheus/client_golang/prometheus; this file aliases that
// import as promclient to keep the two apart.
package prometheus

import (
	"fmt"
	"net/http"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	observability "github.com/vislake/speed/go/observability"
)

func init() {
	observability.RegisterLocalMetricsReader(buildReader)
}

// buildReader constructs a fresh Prometheus registry and an OTel exporter
// registered against it, returning the exporter (itself an
// sdkmetric.Reader, which is what makes it pull-based once
// observability.initLocalExporters registers it directly on the
// MeterProvider, rather than wrapping it in a PeriodicReader) alongside
// the http.Handler that serves that registry's exposition-format output.
//
// A registry of this call's own, rather than
// promclient.DefaultRegisterer: the default registerer is a single
// process-wide global, so a second observability.Init call in the same
// process -- every test that exercises the no-endpoint path repeatedly
// does exactly this -- would panic with "duplicate metrics collector
// registration" the moment it tried to register the same instrument names
// again. A fresh registry per call sidesteps that entirely and keeps
// repeated Init calls independent.
func buildReader() (sdkmetric.Reader, http.Handler, error) {
	registry := promclient.NewRegistry()
	exporter, err := otelprom.New(otelprom.WithRegisterer(registry))
	if err != nil {
		return nil, nil, fmt.Errorf("observability/exporter/prometheus: build exporter: %w", err)
	}
	return exporter, promhttp.HandlerFor(registry, promhttp.HandlerOpts{}), nil
}
