package asynq

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	asynqlib "github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
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
// the queue-depth gauge lifecycle guarantee
// TestStandaloneQueue_DepthGauge_StopsQueryingAfterClose proves for
// jobs.StandaloneQueue (see that test and both registerQueueDepthGauge doc
// comments for the full story). A queue that has been stopped must answer
// nil -- never touch its data source -- so the harness simulates the
// stopped state with a bare *Queue whose stopCh is closed and whose
// inspector is nil: any query would panic on the nil *asynqlib.Inspector
// receiver, the crash the stopped-answer contract prevents. (Registering
// the real NewQueue path and driving Close against a real Redis is the
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
// standalone_queue_test.go construction-option tests make for
// StandaloneQueue's options -- so a construction option added to queue.go
// without its refusal test here is immediately visible.

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

// These tests hold the distributed queue's fail-open metric-registration
// regressions -- the mirror of the root package's own
// metric_registration_failure_test.go: registerJobMetrics and
// registerQueueDepthGauge per-instrument error returns, and Start's
// documented warn-and-continue when those registrations fail. The real
// SDK's Meter never fails an instrument creation, so every branch is
// driven through testutil.NewFailingMeter, the instrument-creation fault
// seam (its own tests live in internal/testutil).

// TestRegisterJobMetrics_InstrumentCreationFailures_ReturnErrors covers
// registerJobMetrics' three error returns: the duration Histogram, the
// attempts Counter and the dead-letter Counter each failing to create
// must surface as that call's error, with the instruments registered
// before the failure left nil so the record-side nil guards (worker.go)
// keep the queue running.
func TestRegisterJobMetrics_InstrumentCreationFailures_ReturnErrors(t *testing.T) {
	cases := []struct {
		name string
		fail string // the instrument name whose creation fails
	}{
		{name: "duration histogram", fail: jobDurationMetricName},
		{name: "attempts counter", fail: jobAttemptsMetricName},
		{name: "dead-letter counter", fail: jobDeadLetterMetricName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meter := testutil.NewFailingMeter(map[string]error{tc.fail: testutil.ErrFailingMeterInstrument})
			otel.SetMeterProvider(testutil.NewFailingMeterProvider(meter))

			q := newTestQueue(t)
			if err := q.registerJobMetrics(); err == nil {
				t.Fatalf("registerJobMetrics() error = nil, want the %s creation failure to surface", tc.name)
			}
			if q.jobDuration != nil || q.jobAttempts != nil || q.jobDeadLetter != nil {
				t.Error("registerJobMetrics() left instruments set after failing, want them nil so the record-side nil guards hold")
			}
		})
	}
}

// TestRegisterQueueDepthGauge_InstrumentCreationFails_ReturnsError covers
// the depth gauge's registration failure surfacing from the call -- the
// answer Start's warn-and-continue branch is built on.
func TestRegisterQueueDepthGauge_InstrumentCreationFails_ReturnsError(t *testing.T) {
	meter := testutil.NewFailingMeter(map[string]error{"jobs.queue.depth": testutil.ErrFailingMeterInstrument})
	otel.SetMeterProvider(testutil.NewFailingMeterProvider(meter))

	q := newTestQueue(t)
	if err := q.registerQueueDepthGauge(otel.Meter(jobs.InstrumentationName)); err == nil {
		t.Error("registerQueueDepthGauge() error = nil, want the instrument-creation failure to surface")
	}
}

// TestStart_MetricRegistrationsFail_WarnsAndServerStillLaunches covers
// Start's fail-open contract for the distributed queue: with both the
// queue-depth gauge and the job instruments refusing registration, Start
// logs the two warnings and still launches asynq's server (against the
// unreachable backend of the same shape the unreachable-redis tests above
// use, since this test only asserts the launch answer, never a processed
// task). A metrics wiring failure must not prevent the queue from
// running.
func TestStart_MetricRegistrationsFail_WarnsAndServerStillLaunches(t *testing.T) {
	meter := testutil.NewFailingMeter(map[string]error{
		"jobs.queue.depth":    testutil.ErrFailingMeterInstrument,
		jobDurationMetricName: testutil.ErrFailingMeterInstrument,
	})
	otel.SetMeterProvider(testutil.NewFailingMeterProvider(meter))

	q := NewQueue(deadRedisOpt)
	prevDefault := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer func() { slog.SetDefault(prevDefault) }()

	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil despite the metric registrations failing", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := q.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}

	out := buf.String()
	if !strings.Contains(out, "registering queue depth gauge failed") {
		t.Errorf("Start() logged: %s, want the queue-depth-gauge warning", out)
	}
	if !strings.Contains(out, "registering job metrics failed") {
		t.Errorf("Start() logged: %s, want the job-metrics warning", out)
	}
}

// These tests hold the unit-testable half of this subpackage's error
// surface -- what Queue does when its Redis backend cannot be reached --
// driven against a client pointed at a reserved loopback port
// (127.0.0.1:1, which refuses every connection instantly), never a fake:
// the go-redis/asynq clients are not mockable without replacing the very
// seams this package is a thin wrapper around, and faking them would test
// nothing this package ships. Every success path (a real enqueue, a
// real Get, an idempotency dedupe, a real dispatch) requires a live
// Redis and lives in this module's Docker-backed integration tier; what
// the unit suite can honestly exercise is the fail-closed reporting side
// -- every operation answers its wrapped error rather than hanging or
// panicking, and the marker-guarded dispatch logic makes its
// fail-closed-refusal decision from the read's failure without a server.

// deadRedisAddr is the backend every queue in these tests is wired to: a
// reserved port that refuses connections instantly, so every command
// fails fast with a connection error and no test ever waits on a dial
// timeout.
const deadRedisAddr = "127.0.0.1:1"

// deadRedisOpt is the asynqlib.RedisConnOpt form of deadRedisAddr.
var deadRedisOpt = asynqlib.RedisClientOpt{Addr: deadRedisAddr}

// captureLogs swaps slog's default for a buffer for the duration of the
// test, capturing every log line this package emits through
// obs.FromContext on a context that carries no logger (all of these
// tests' code sites log through context.Background()-rooted contexts).
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestNewQueue_WiresSharedClientAndDefaults pins NewQueue's construction
// without any Redis I/O: the shared client, the asynq client/inspector/
// server triad and the documented defaults are all in place, so a queue
// can be built, configured and refused by its backend long before any
// server exists.
func TestNewQueue_WiresSharedClientAndDefaults(t *testing.T) {
	q := NewQueue(deadRedisOpt, WithConcurrency(3))

	if q.rdb == nil || q.client == nil || q.inspector == nil || q.server == nil {
		t.Fatal("NewQueue() left client/inspector/server/rdb nil, want the shared wiring in place")
	}
	if q.tenantConcurrency != DefaultTenantConcurrencyLimit {
		t.Errorf("tenantConcurrency = %d, want %d", q.tenantConcurrency, DefaultTenantConcurrencyLimit)
	}
	if q.defaultTimeout != jobs.DefaultTimeout {
		t.Errorf("defaultTimeout = %v, want %v", q.defaultTimeout, jobs.DefaultTimeout)
	}
	if q.completedRetention != DefaultCompletedRetention {
		t.Errorf("completedRetention = %v, want %v", q.completedRetention, DefaultCompletedRetention)
	}
	if q.cancelledRetention != DefaultCancelledRetention {
		t.Errorf("cancelledRetention = %v, want %v", q.cancelledRetention, DefaultCancelledRetention)
	}
	if q.throttleRetryDelay != DefaultThrottleRetryDelay {
		t.Errorf("throttleRetryDelay = %v, want %v", q.throttleRetryDelay, DefaultThrottleRetryDelay)
	}
	if len(q.queueWeights) != 3 {
		t.Errorf("queueWeights = %v, want the three default tiers", q.queueWeights)
	}
}

// nonUniversalRedisConnOpt is an asynqlib.RedisConnOpt whose
// MakeRedisClient returns something that is not a redis.UniversalClient
// -- the unsupported-conn-opt shape NewQueue's construction panic exists
// to refuse loudly at construction time rather than inside asynq's own
// constructors.
type nonUniversalRedisConnOpt struct{}

func (nonUniversalRedisConnOpt) MakeRedisClient() interface{} { return 42 }

// TestNewQueue_UnsupportedRedisConnOpt_Panics pins that refusal.
func TestNewQueue_UnsupportedRedisConnOpt_Panics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewQueue(unsupported RedisConnOpt) did not panic")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "unsupported asynq.RedisConnOpt type") {
			t.Errorf("panic value = %v, want the unsupported-RedisConnOpt message", r)
		}
	}()
	NewQueue(nonUniversalRedisConnOpt{})
}

// TestStartAndClose_UnreachableBackend_CompleteWithoutHanging pins the
// lifecycle's failure behavior against a backend that refuses every
// connection: Start launches asynq's own processor goroutines (which
// simply fail their polls against the dead backend), returns nil, is a
// no-op on a second call, and Close shuts the goroutines and the shared
// client down, idempotently. A queue whose backend vanished must not
// wedge Start or Close -- the operator restarts the backend, not the
// process.
func TestStartAndClose_UnreachableBackend_CompleteWithoutHanging(t *testing.T) {
	q := NewQueue(deadRedisOpt)

	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil (asynq's server launch does not depend on a reachable Redis)", err)
	}
	if err := q.Start(context.Background()); err != nil {
		t.Errorf("second Start() error = %v, want nil (no-op after a successful Start)", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := q.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	if err := q.Close(ctx); err != nil {
		t.Errorf("second Close() error = %v, want nil (idempotent)", err)
	}
}

// TestEnqueue_InvalidTask_RefusedBeforeAnyBackendTouch pins that Enqueue
// validates the task before the queue's own client is ever consulted:
// an invalid task is refused with the validation error even though the
// backend is unreachable -- validation failure must never be reported as
// a backend failure.
func TestEnqueue_InvalidTask_RefusedBeforeAnyBackendTouch(t *testing.T) {
	q := NewQueue(deadRedisOpt)

	_, err := q.Enqueue(context.Background(), jobs.Task{Type: "", TenantID: "tenant-a"})
	if err == nil {
		t.Fatal("Enqueue() with an empty Type error = nil, want the validation error")
	}
	if _, ok := apperr.As(err); !ok {
		t.Errorf("Enqueue() error = %v, want a coded *apperr.Error", err)
	}
}

// TestEnqueue_UnreachableBackend_ReturnsWrappedError pins the two
// Enqueue paths' failure reporting: a fresh task whose insert cannot
// reach Redis and a keyed task whose idempotency claim cannot reach
// Redis each answer their wrapped error -- never a hang, never a
// fabricated id.
func TestEnqueue_UnreachableBackend_ReturnsWrappedError(t *testing.T) {
	q := NewQueue(deadRedisOpt)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := q.Enqueue(ctx, jobs.Task{Type: "plain", TenantID: "tenant-a"}); err == nil ||
		!strings.Contains(err.Error(), "jobs: enqueue job") {
		t.Errorf("Enqueue(no key) error = %v, want a wrapped enqueue error", err)
	}

	if _, err := q.Enqueue(ctx, jobs.Task{Type: "keyed", TenantID: "tenant-a", IdempotencyKey: "invoice-1"}); err == nil ||
		!strings.Contains(err.Error(), "claim idempotent enqueue") {
		t.Errorf("Enqueue(idempotency key) error = %v, want a wrapped claim error", err)
	}
}

// TestGetCancelAndDeadLetterJobs_UnreachableBackend_ReturnWrappedErrors
// pins the reporting side's failure answers: a Job lookup that cannot
// reach any of the three priority queues, a Cancel built on the same
// lookup, and a dead-letter listing over unreachable queues each answer
// their wrapped error rather than a fabricated not-found or an empty
// list -- an operator must be able to tell "backend down" from "no such
// job".
func TestGetCancelAndDeadLetterJobs_UnreachableBackend_ReturnWrappedErrors(t *testing.T) {
	q := NewQueue(deadRedisOpt)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := q.Get(ctx, "job-1"); err == nil || !strings.Contains(err.Error(), "jobs: look up job") {
		t.Errorf("Get() error = %v, want a wrapped lookup error", err)
	}
	if err := q.Cancel(ctx, "job-1"); err == nil || !strings.Contains(err.Error(), "jobs: look up job") {
		t.Errorf("Cancel() error = %v, want a wrapped lookup error", err)
	}
	if _, err := q.DeadLetterJobs(ctx); err == nil || !strings.Contains(err.Error(), "list archived jobs in queue") {
		t.Errorf("DeadLetterJobs() error = %v, want a wrapped listing error", err)
	}
}

// TestCancelMarkers_UnreachableBackend_ReportTheReadFailure pins the two
// marker primitives' failure reporting on a bare queue whose rdb is an
// unreachable client: both the write (Cancel's only authoritative
// effect) and the read (Get's fail-closed probe) answer the connection
// error -- never redis.Nil's "no marker", which only a live Redis can
// truthfully answer.
func TestCancelMarkers_UnreachableBackend_ReportTheReadFailure(t *testing.T) {
	q := &Queue{rdb: redis.NewClient(&redis.Options{Addr: deadRedisAddr})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := q.writeCancelMarker(ctx, "job-1"); err == nil {
		t.Error("writeCancelMarker() error = nil, want the connection error")
	}
	if _, err := q.readCancelMarker(ctx, "job-1"); err == nil {
		t.Error("readCancelMarker() error = nil, want the connection error")
	}
}

// TestProcessTask_MarkerReadFailure_RefusesTheRun pins processTask's own
// fail-closed decision end to end without a server: with the marker read
// failing (the dead rdb), the dispatched task is refused with
// errCancelMarkerUnreadable -- never executed, never reported as a clean
// skip.
func TestProcessTask_MarkerReadFailure_RefusesTheRun(t *testing.T) {
	q := &Queue{
		rdb:      redis.NewClient(&redis.Options{Addr: deadRedisAddr}),
		handlers: make(map[string]jobs.Handler),
	}
	buf := captureLogs(t)
	task := asynqlib.NewTaskWithHeaders("marker-refused", nil, map[string]string{headerTenantID: "tenant-a"})

	err := q.processTask(context.Background(), task)

	if !errors.Is(err, errCancelMarkerUnreadable) {
		t.Errorf("processTask() error = %v, want errCancelMarkerUnreadable", err)
	}
	if out := buf.String(); !strings.Contains(out, "cancellation marker unreadable") {
		t.Errorf("processTask() logged: %s, want the unreadable-marker warning", out)
	}
}

// TestHandleError_TerminalMarkerReadFailure_LogsAndRunsTheTerminalPath
// pins handleError's own marker half: for an attempt asynq's dispatch
// loop is about to archive (retried >= maxRetry), the cancellation
// marker read runs on a cancellation-stripped context and its failure is
// logged -- the attempt still converges through the ordinary terminal
// machinery, since the archive itself is asynq's own next step. With a
// bare context asynq's own accessors answer zero (retried = maxRetry =
// 0), which is exactly the terminal shape.
func TestHandleError_TerminalMarkerReadFailure_LogsAndRunsTheTerminalPath(t *testing.T) {
	q := &Queue{
		rdb:      redis.NewClient(&redis.Options{Addr: deadRedisAddr}),
		handlers: make(map[string]jobs.Handler),
	}
	h := &recordingFailureHook{jobType: "always-fails"}
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	buf := captureLogs(t)
	task := asynqlib.NewTaskWithHeaders("always-fails", nil, map[string]string{headerTenantID: "tenant-a"})

	q.handleError(context.Background(), task, errors.New("attempt failed"))

	if out := buf.String(); !strings.Contains(out, "reading cancellation marker failed") {
		t.Errorf("handleError() logged: %s, want the marker-read-failure warning", out)
	}
	if len(h.calls) != 1 {
		t.Errorf("OnFailure called %d times, want exactly 1 (the terminal archive must still fire the hook)", len(h.calls))
	}
}

// TestProcessTaskUncancelled_MissingTenantHeader_RefusedBeforeAnyWrite
// pins the missing-tenant defense on the dispatch path: a task whose
// stored headers carry no tenant is refused with the coded
// errTaskMissingTenant before the attempt reaches the tenant slot or the
// ResultWriter (whose envelope writes a synthetic task cannot perform) --
// the one full branch of processTaskUncancelled past the handler lookup
// that needs no real dispatch machinery.
func TestProcessTaskUncancelled_MissingTenantHeader_RefusedBeforeAnyWrite(t *testing.T) {
	q := newTestQueue(t)
	handled := false
	if err := q.RegisterHandler(jobs.NewHandlerFunc("missing-tenant", func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
		handled = true
		return jobs.Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	task := asynqlib.NewTaskWithHeaders("missing-tenant", nil, map[string]string{})
	err := q.processTaskUncancelled(context.Background(), task, "job-1", slog.Default())

	if handled {
		t.Error("Handle ran for a task with no tenant header, want the refusal before any execution")
	}
	if err == nil {
		t.Fatalf("processTaskUncancelled() error = nil, want the coded missing-tenant refusal")
	}
	if !apperr.HasCode(err, errTaskMissingTenant.Code) {
		t.Errorf("processTaskUncancelled() error = %v, want code %q", err, errTaskMissingTenant.Code)
	}
}
