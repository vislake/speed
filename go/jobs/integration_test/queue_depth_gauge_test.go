//go:build integration

package jobs_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/jobs/internal/testutil"
)

// TestRedisQueue_DepthGauge_UnregisteredTierDoesNotFailCollect is the
// real-Redis regression for the queue-depth gauge's fresh-deployment
// shape. asynq registers a queue -- SADD to its asynq:queues set -- only
// when the first task lands on it, so a queue that has only ever carried
// default-tier tasks owns exactly one registered tier, and a scrape of a
// fresh deployment must still Collect cleanly while reporting the tier
// that exists. Before registerQueueDepthGauge learned to discover the
// registered tiers from Inspector.Queues, its callback probed the
// fixed critical/default/low set with GetQueueInfo; asynq v0.26's
// GetQueueInfo answers a missing queue with an internal NotFound that --
// unlike every other Inspector method -- does not wrap the exported
// asynqlib.ErrQueueNotFound, so the errors.Is tolerance never matched,
// the callback returned the error, and every Collect failed with
// NOT_FOUND: queue "critical" does not exist until a critical-tier task
// had been enqueued at least once. The unit tier cannot pin this: the
// callback's Redis queries run through the concrete *asynqlib.Inspector
// no test can stub, so the real-Redis tier is the regression's home, per
// this module's own testing convention (queue_test.go's
// TestQueue_DepthGauge_StoppedQueueDoesNotQueryRedis records the same
// division for the stopped-queue contract).
func TestRedisQueue_DepthGauge_UnregisteredTierDoesNotFailCollect(t *testing.T) {
	reader := testutil.SetupTestMeterProvider(t)
	ctx := context.Background()

	q := startTestAsynqQueue(t, ctx)
	if err := q.RegisterHandler(&metricOkHandler{}); err != nil {
		t.Fatalf("RegisterHandler(metric-ok) error = %v", err)
	}
	if _, err := q.Enqueue(ctx, jobs.Task{Type: "metric-ok", TenantID: "tenant-a"}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	// The task is now registered on the default tier; critical and low
	// have never carried a task, so asynq's own registry names default
	// alone. No wait for the terminal state is needed: the tier stays
	// registered whether the worker has drained the task or not, so the
	// gauge has a default-tier row to report at any scrape time.

	// The callback must not consult a never-registered tier: a
	// GetQueueInfo probe of the never-registered critical tier surfaces
	// NOT_FOUND (its missing-queue answer does not wrap
	// asynqlib.ErrQueueNotFound), which the ListQueues-driven discovery
	// avoids -- so this Collect must succeed.
	depth := testutil.CollectMetric(t, reader, "jobs.queue.depth")

	labels := gaugeQueueLabels(t, depth)
	if !labels["default"] {
		t.Errorf("jobs.queue.depth reports no data point for queue=%q, want the registered default tier reported", "default")
	}
	if labels["critical"] {
		t.Errorf("jobs.queue.depth reports a data point for queue=%q, want the never-registered tier silent", "critical")
	}
	if labels["low"] {
		t.Errorf("jobs.queue.depth reports a data point for queue=%q, want the never-registered tier silent", "low")
	}
}

// gaugeQueueLabels returns the set of "queue" attribute values present on
// the jobs.queue.depth gauge's data points, for the unregistered-tier
// assertions above.
func gaugeQueueLabels(t *testing.T, m metricdata.Metrics) map[string]bool {
	t.Helper()
	gauge, ok := m.Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("metric %q Data = %T, want metricdata.Gauge[int64]", m.Name, m.Data)
	}
	labels := make(map[string]bool)
	for _, dp := range gauge.DataPoints {
		labels[testutil.AttrString(dp.Attributes, "queue")] = true
	}
	return labels
}
