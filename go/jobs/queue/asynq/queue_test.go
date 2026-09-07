package asynq

import (
	"context"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/jobs/internal/testutil"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// TestQueue_RegisterHandler_DuplicateType_Errors mirrors StandaloneQueue's
// own TestRegisterHandler_DuplicateType_Errors: RegisterHandler's
// duplicate-type check is pure map logic with no Redis involved, so it is
// unit-tested directly here rather than only incidentally exercised as
// setup in worker_test.go's other tests. See that file's newTestQueue for
// why a bare *Queue (no NewQueue, no Redis) is sufficient.
func TestQueue_RegisterHandler_DuplicateType_Errors(t *testing.T) {
	q := newTestQueue(t)
	h1 := jobs.NewHandlerFunc("dup", func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) { return jobs.Result{}, nil })
	h2 := jobs.NewHandlerFunc("dup", func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) { return jobs.Result{}, nil })

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

func TestQueue_RegisterHandler_DistinctTypes_BothRegister(t *testing.T) {
	q := newTestQueue(t)
	a := jobs.NewHandlerFunc("a", func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) { return jobs.Result{}, nil })
	b := jobs.NewHandlerFunc("b", func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) { return jobs.Result{}, nil })

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

// TestQueue_DepthGauge_StoppedQueueDoesNotQueryRedis is the Queue half of
// the queue-depth gauge lifecycle regression
// TestStandaloneQueue_DepthGauge_StopsQueryingAfterClose proves for
// jobs.StandaloneQueue (same defect, same fix shape; see that test and
// both registerQueueDepthGauge doc comments for the full story). A queue
// that has been stopped must answer nil -- never touch its data source --
// so the harness simulates the stopped state with a bare *Queue whose
// stopCh is closed and whose inspector is nil: any query would panic on
// the nil *asynqlib.Inspector receiver, which is exactly the sharper
// version of the error the unguarded callback produced. (Registering the
// real NewQueue path and driving Close against a real Redis is the
// integration tier's job, per this module's testing convention -- this
// unit test pins the callback's stopped-answer contract alone.)
func TestQueue_DepthGauge_StoppedQueueDoesNotQueryRedis(t *testing.T) {
	q := &Queue{stopCh: make(chan struct{})}
	close(q.stopCh) // the post-Close state: Close has signaled the stop

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if err := q.registerQueueDepthGauge(mp.Meter(jobs.InstrumentationName)); err != nil {
		t.Fatalf("registerQueueDepthGauge() error = %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v, want nil; a stopped queue's gauge callback must not touch its data source", err)
	}
	if depth := testutil.MetricByName(t, rm, "jobs.queue.depth"); depth != nil {
		t.Errorf("Collect() reports %q for a stopped queue, want it to answer nothing", "jobs.queue.depth")
	}
}

// This file's own option-refusal tests: configuration values that would
// make the queue silently process nothing, or crash a processor goroutine
// later, must be refused at option time with a coded panic (the same
// convention pkgcore's constructors use) -- never accepted and left to
// fail somewhere else.

// assertOptionPanics asserts that fn panics with a coded *apperr.Error
// carrying code.
func assertOptionPanics(t *testing.T, code string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("%s: expected a coded panic %q, got none", t.Name(), code)
		}
		e, ok := r.(*apperr.Error)
		if !ok {
			t.Fatalf("%s: panic value = %T(%v), want a coded *apperr.Error %q", t.Name(), r, r, code)
		}
		if e.Code != code {
			t.Fatalf("%s: panic code = %q, want %q", t.Name(), e.Code, code)
		}
	}()
	fn()
}

// TestWithThrottleRetryDelay_Negative_Refused pins the negative-delay
// refusal: retryDelay (worker.go) feeds the delay through
// math/rand.Int64N's range, which panics on a negative bound inside
// asynq's own processor goroutine -- an unrecovered panic that would crash
// the whole process. Fails on the pre-fix code, where WithThrottleRetryDelay(-1)
// is accepted silently and the crash only happens later, asynchronously.
func TestWithThrottleRetryDelay_Negative_Refused(t *testing.T) {
	assertOptionPanics(t, "jobs.throttle_retry_delay_negative", func() { WithThrottleRetryDelay(-1) })
	assertOptionPanics(t, "jobs.throttle_retry_delay_negative", func() { WithThrottleRetryDelay(-time.Hour) })
}

// TestWithTenantConcurrencyLimit_ZeroOrNegative_Refused pins the zero-limit
// refusal: a limit of zero would bounce every task of every tenant forever
// (errTenantAtCapacity), silently processing nothing.
func TestWithTenantConcurrencyLimit_ZeroOrNegative_Refused(t *testing.T) {
	assertOptionPanics(t, "jobs.tenant_concurrency_limit_zero", func() { WithTenantConcurrencyLimit(0) })
	assertOptionPanics(t, "jobs.tenant_concurrency_limit_zero", func() { WithTenantConcurrencyLimit(-2) })
}
