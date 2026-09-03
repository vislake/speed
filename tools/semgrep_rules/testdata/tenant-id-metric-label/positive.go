// Fixture for tools/semgrep_rules/tenant-id-metric-label.yml.
// Planted violations: every pattern shape must fire on this file.
// This file is NOT shipped code -- it proves the rule fires.
package fixture

import (
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var (
	perTenantCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "http_requests_total"},
		[]string{"tenant_id"}, // fires: vector label name
	)
)

func badMetrics(ctx interface{ Done() <-chan struct{} }, tenantID string) {
	otelMeter := metric.Meter("fixture")
	requests, _ := otelMeter.Int64Counter("requests")
	requests.Add(ctx, 1, metric.WithAttributes(
		attribute.String("tenant_id", tenantID), // fires: WithAttributes inline literal
		attribute.String("route", "/x"),
	))
	attrs := []attribute.KeyValue{
		attribute.String("tenant_id", tenantID), // fires: KeyValue slice literal
		attribute.String("route", "/x"),
	}
	requests.Add(ctx, 1, metric.WithAttributes(attrs...)) // clean: first option is a bare variable, no label-name text
	prometheus.Labels{"tenant_id": tenantID}.String() // fires: Labels literal
}
