// Fixture for tools/semgrep_rules/tenant-id-metric-label.yml.
// Clean control: none of these patterns may fire.
package fixture

import (
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var (
	routeCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "http_requests_total"},
		[]string{"route"}, // cardinality-bounded route label: fine
	)
)

func goodMetrics(ctx interface{ Done() <-chan struct{} }, route string) {
	otelMeter := metric.Meter("fixture")
	requests, _ := otelMeter.Int64Counter("requests")
	requests.Add(ctx, 1, metric.WithAttributes(
		attribute.String("route", route),
	))
	routeCounter.WithLabelValues(route).Inc()
	// The tenant dimension's sanctioned home: span attributes and log
	// fields, never a metric label.
	attribute.String("tenant_id", "t1") // bare construction is inert here
}
