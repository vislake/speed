package asynq

import (
	"context"
	"testing"
	"time"

	asynqlib "github.com/hibiken/asynq"
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
// fail somewhere else. Every validated construction option in queue.go is
// pinned below -- the same completeness claim go/jobs' own
// option_validation_test.go makes for StandaloneQueue's options -- so a
// construction option added to queue.go without its refusal test here is
// immediately visible.

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
// the whole process.
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

// TestWithConcurrency_ZeroOrNegative_Refused pins the WithConcurrency
// refusal for an EXPLICIT value below 1 (omitting the option keeps the
// field at zero and asynqlib.NewServer's runtime.NumCPU() resolution):
// asynqlib.NewServer silently replaces a non-positive Config.Concurrency
// with NumCPU, so an explicit zero or negative could never be honoured
// literally -- it would silently mean "however many CPUs this machine
// happens to have", a machine-dependent guess indistinguishable from an
// option never passed.
func TestWithConcurrency_ZeroOrNegative_Refused(t *testing.T) {
	assertOptionPanics(t, "jobs.worker_count_zero", func() { WithConcurrency(0) })
	assertOptionPanics(t, "jobs.worker_count_zero", func() { WithConcurrency(-1) })
}

// TestWithQueueWeights_AnyTierZeroOrNegative_Refused pins the per-tier
// weight refusal on each of the three arguments: asynqlib.NewServer
// silently drops any configured queue whose weight is not positive
// (server.go's own p > 0 filter), while Enqueue keeps routing that
// priority into its fixed tier (queueForPriority), so a zeroed or
// negative tier would silently accumulate Jobs no processor ever
// services.
func TestWithQueueWeights_AnyTierZeroOrNegative_Refused(t *testing.T) {
	for _, weights := range [][3]int{{0, 3, 1}, {6, 0, 1}, {6, 3, 0}, {-1, 3, 1}, {6, -1, 1}, {6, 3, -1}} {
		assertOptionPanics(t, "jobs.queue_weight_zero", func() {
			WithQueueWeights(weights[0], weights[1], weights[2])
		})
	}
}

// TestWithJobTimeout_ZeroOrNegative_Refused pins the WithJobTimeout(0)
// refusal: a non-positive configured default can never be honoured
// literally -- Enqueue hands it to asynqlib.Timeout, where zero is
// asynq's own "no limit" marker (collapse onto asynq's internal fallback
// deadline) and a negative value lands every attempt's deadline in the
// past, and handleErrorAttempt additionally builds the FailureHook's
// OnFailure context from it (worker.go), which a non-positive value makes
// expire instantly.
func TestWithJobTimeout_ZeroOrNegative_Refused(t *testing.T) {
	assertOptionPanics(t, "jobs.job_timeout_zero", func() { WithJobTimeout(0) })
	assertOptionPanics(t, "jobs.job_timeout_zero", func() { WithJobTimeout(-time.Second) })
}

// TestWithCompletedRetention_ZeroOrNegative_Refused pins the
// WithCompletedRetention refusal: enqueueNew passes this value as
// asynqlib.Retention on every Enqueue, and asynq only keeps a completed
// task's record while that retention is strictly positive (processor.go's
// own msg.Retention > 0 gate) -- a zero or negative value would silently
// fall back to asynq's delete-on-success behavior, and Get() for a
// succeeded Job would answer ErrJobNotFound almost immediately, breaking
// the exact contract DefaultCompletedRetention exists to serve.
func TestWithCompletedRetention_ZeroOrNegative_Refused(t *testing.T) {
	assertOptionPanics(t, "jobs.completed_retention_zero", func() { WithCompletedRetention(0) })
	assertOptionPanics(t, "jobs.completed_retention_zero", func() { WithCompletedRetention(-time.Hour) })
}

// TestWithCancelledRetention_ZeroOrNegative_Refused pins the
// WithCancelledRetention refusal: writeCancelMarker writes each
// cancellation marker with this value as the Redis TTL, and go-redis only
// attaches a TTL for a positive expiration -- a zero or negative retention
// would silently leave every marker without an expiry, reporting
// StatusCancelled forever and growing without bound, the bounded-survival
// design DefaultCancelledRetention's own doc comment states.
func TestWithCancelledRetention_ZeroOrNegative_Refused(t *testing.T) {
	assertOptionPanics(t, "jobs.cancelled_retention_zero", func() { WithCancelledRetention(0) })
	assertOptionPanics(t, "jobs.cancelled_retention_zero", func() { WithCancelledRetention(-24 * time.Hour) })
}

// TestWithRetryDelayFunc_Nil_Refused pins the WithRetryDelayFunc(nil)
// refusal: businessRetryDelayFunc is invoked from q.retryDelay -- asynq's
// own processor and recoverer goroutines call it for every genuine Handler
// failure, both outside any recover -- so a nil override would nil-deref
// panic there, an unrecovered panic crashing the whole process, the
// identical late-crash shape WithThrottleRetryDelay's refusal exists for.
// Omitting the option keeps asynqlib.DefaultRetryDelayFunc.
func TestWithRetryDelayFunc_Nil_Refused(t *testing.T) {
	assertOptionPanics(t, "jobs.retry_delay_func_nil", func() { WithRetryDelayFunc(nil) })
}

// TestWithTaskCheckInterval_Negative_RefusedAndZeroDelegates pins the
// negative-interval refusal (asynqlib.NewServer's own defaulting would
// silently substitute its 1-second default for any non-positive value, so
// a negative explicit value could never be honoured literally) and pins
// that zero -- the sanctioned "asynq's own default" delegation marker --
// is still accepted.
func TestWithTaskCheckInterval_Negative_RefusedAndZeroDelegates(t *testing.T) {
	assertOptionPanics(t, "jobs.task_check_interval_negative", func() { WithTaskCheckInterval(-time.Second) })
	q := &Queue{}
	WithTaskCheckInterval(0)(q)
	if q.taskCheckInterval != 0 {
		t.Errorf("WithTaskCheckInterval(0) stored %v, want the zero delegation marker", q.taskCheckInterval)
	}
}

// TestWithDelayedTaskCheckInterval_Negative_RefusedAndZeroDelegates pins
// the negative-interval refusal (asynqlib.NewServer's defaulting catches
// only exactly zero, so a negative value reaches asynq's forwarder
// goroutine, whose timer busy-loops on the negative interval after Start)
// and pins that zero -- the sanctioned "asynq's own default" delegation
// marker -- is still accepted.
func TestWithDelayedTaskCheckInterval_Negative_RefusedAndZeroDelegates(t *testing.T) {
	assertOptionPanics(t, "jobs.delayed_task_check_interval_negative", func() { WithDelayedTaskCheckInterval(-time.Second) })
	q := &Queue{}
	WithDelayedTaskCheckInterval(0)(q)
	if q.delayedTaskCheckInterval != 0 {
		t.Errorf("WithDelayedTaskCheckInterval(0) stored %v, want the zero delegation marker", q.delayedTaskCheckInterval)
	}
}

// TestConstructionOptions_ValidValuesStillApply guards the other side of
// every refusal above: a valid value must still be accepted and applied,
// because the refusals only ever fire below each option's documented
// threshold. One application per construction option, read back through
// the fields NewQueue's option loop would set.
func TestConstructionOptions_ValidValuesStillApply(t *testing.T) {
	q := &Queue{}
	WithConcurrency(8)(q)
	WithTenantConcurrencyLimit(3)(q)
	WithQueueWeights(5, 2, 1)(q)
	WithJobTimeout(90 * time.Second)(q)
	WithCompletedRetention(48 * time.Hour)(q)
	WithCancelledRetention(7 * 24 * time.Hour)(q)
	WithThrottleRetryDelay(300 * time.Millisecond)(q)
	WithRetryDelayFunc(asynqlib.DefaultRetryDelayFunc)(q)

	if q.concurrency != 8 {
		t.Errorf("concurrency = %d, want 8", q.concurrency)
	}
	if q.tenantConcurrency != 3 {
		t.Errorf("tenantConcurrency = %d, want 3", q.tenantConcurrency)
	}
	if q.queueWeights[queueCritical] != 5 || q.queueWeights[queueDefault] != 2 || q.queueWeights[queueLow] != 1 {
		t.Errorf("queueWeights = %v, want {critical:5 default:2 low:1}", q.queueWeights)
	}
	if q.defaultTimeout != 90*time.Second {
		t.Errorf("defaultTimeout = %v, want 90s", q.defaultTimeout)
	}
	if q.completedRetention != 48*time.Hour {
		t.Errorf("completedRetention = %v, want 48h", q.completedRetention)
	}
	if q.cancelledRetention != 7*24*time.Hour {
		t.Errorf("cancelledRetention = %v, want 168h", q.cancelledRetention)
	}
	if q.throttleRetryDelay != 300*time.Millisecond {
		t.Errorf("throttleRetryDelay = %v, want 300ms", q.throttleRetryDelay)
	}
	if q.businessRetryDelayFunc == nil {
		t.Error("businessRetryDelayFunc = nil, want the applied function")
	}
}
