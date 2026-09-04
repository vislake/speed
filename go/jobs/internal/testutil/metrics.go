// Package testutil holds test helpers shared across this module's own test
// files, per the backend coding standard's "put shared test helpers in a
// dedicated internal/testutil package, never duplicated" rule. It sits at
// go/jobs/internal/testutil so both the root jobs package's own tests and
// the queue/asynq subpackage's tests may import it (Go's internal-package
// visibility rule allows any package rooted under go/jobs to import
// anything under go/jobs/internal), without exporting it to consumers
// outside this module.
package testutil

import (
	"testing"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// MetricByName returns the metric named name within rm, or nil when rm
// carries no such metric. Shared by StandaloneQueue's and the queue/asynq
// subpackage's Queue's identically-shaped "jobs.queue.depth" gauge
// lifecycle tests.
func MetricByName(t *testing.T, rm metricdata.ResourceMetrics, name string) *metricdata.Metrics {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for i := range sm.Metrics {
			if sm.Metrics[i].Name == name {
				return &sm.Metrics[i]
			}
		}
	}
	return nil
}

// MetricNames lists every metric name present in rm, for failure messages.
func MetricNames(rm metricdata.ResourceMetrics) []string {
	var names []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names = append(names, m.Name)
		}
	}
	return names
}
