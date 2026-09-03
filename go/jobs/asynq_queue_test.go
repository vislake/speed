package jobs

import (
	"context"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestAsynqQueue_RegisterHandler_DuplicateType_Errors mirrors standalone_queue_test.go's
// TestRegisterHandler_DuplicateType_Errors: RegisterHandler's duplicate-type
// check is pure map logic with no Redis involved, so it is unit-tested
// directly here rather than only incidentally exercised as setup in
// asynq_worker_test.go's other tests. See that file's newTestAsynqQueue for
// why a bare *AsynqQueue (no NewAsynqQueue, no Redis) is sufficient.
func TestAsynqQueue_RegisterHandler_DuplicateType_Errors(t *testing.T) {
	q := newTestAsynqQueue(t)
	h1 := NewHandlerFunc("dup", func(context.Context, *Job, ProgressFn) (Result, error) { return Result{}, nil })
	h2 := NewHandlerFunc("dup", func(context.Context, *Job, ProgressFn) (Result, error) { return Result{}, nil })

	if err := q.RegisterHandler(h1); err != nil {
		t.Fatalf("first RegisterHandler() error = %v, want nil", err)
	}
	if err := q.RegisterHandler(h2); err == nil {
		t.Fatal("second RegisterHandler() for the same Type error = nil, want ErrDuplicateHandlerType")
	}
	if got := q.handler("dup"); got == nil {
		t.Error("handler(\"dup\") = nil, want the first-registered Handler to remain in effect")
	}
}

func TestAsynqQueue_RegisterHandler_DistinctTypes_BothRegister(t *testing.T) {
	q := newTestAsynqQueue(t)
	a := NewHandlerFunc("a", func(context.Context, *Job, ProgressFn) (Result, error) { return Result{}, nil })
	b := NewHandlerFunc("b", func(context.Context, *Job, ProgressFn) (Result, error) { return Result{}, nil })

	if err := q.RegisterHandler(a); err != nil {
		t.Fatalf("RegisterHandler(a) error = %v", err)
	}
	if err := q.RegisterHandler(b); err != nil {
		t.Fatalf("RegisterHandler(b) error = %v", err)
	}
	if q.handler("a") == nil || q.handler("b") == nil {
		t.Error("both distinct Types should be independently registered")
	}
	if q.handler("no-such-type") != nil {
		t.Error("handler(\"no-such-type\") should be nil")
	}
}

// TestAsynqQueue_DepthGauge_StoppedQueueDoesNotQueryRedis is the AsynqQueue
// half of the queue-depth gauge lifecycle regression
// TestStandaloneQueue_DepthGauge_StopsQueryingAfterClose proves for
// StandaloneQueue (same defect, same fix shape; see that test and both
// registerQueueDepthGauge doc comments for the full story). A queue that
// has been stopped must answer nil -- never touch its data source -- so the
// harness simulates the stopped state with a bare *AsynqQueue whose stopCh
// is closed and whose inspector is nil: any query would panic on the nil
// *asynq.Inspector receiver, which is exactly the sharper version of the
// error the unguarded callback produced. (Registering the real
// NewAsynqQueue path and driving Close against a real Redis is the
// integration tier's job, per this package's testing convention -- this
// unit test pins the callback's stopped-answer contract alone.)
func TestAsynqQueue_DepthGauge_StoppedQueueDoesNotQueryRedis(t *testing.T) {
	q := &AsynqQueue{stopCh: make(chan struct{})}
	close(q.stopCh) // the post-Close state: Close has signaled the stop

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if err := q.registerQueueDepthGauge(mp.Meter(instrumentationName)); err != nil {
		t.Fatalf("registerQueueDepthGauge() error = %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v, want nil; a stopped queue's gauge callback must not touch its data source", err)
	}
	if depth := metricByName(t, rm, "jobs.queue.depth"); depth != nil {
		t.Errorf("Collect() reports %q for a stopped queue, want it to answer nothing", "jobs.queue.depth")
	}
}
