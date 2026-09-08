// Package asynq is the distributed deployment mode's Queue implementation:
// backed by Redis via github.com/hibiken/asynq, per docs/internal/07-
// platform-services.md's jobs section ("the distributed deployment mode =
// Redis (hibiken/asynq, mature and ships its own retry/delay/scheduling/Web
// UI, not worth reimplementing ourselves)"). It exists as its own
// subpackage, separate from github.com/vislake/speed/go/jobs's root
// package, so a consumer that only ever runs the standalone deployment
// mode's jobs.StandaloneQueue never pulls in asynq or go-redis at all --
// see AGENTS.md's dependency-cost measurement and root CLAUDE.md's "Do not
// put a backend implementation in the same package as the interface it
// implements" rule.
//
// Queue implements the exact same jobs.Queue interface jobs.StandaloneQueue
// does -- see this module's AGENTS.md "Distributed: asynq.Queue" section
// for the full mapping from every jobs.Task/jobs.Job/jobs.EnqueueOption
// concept onto asynq's own client/server/inspector primitives, including
// the two places (per-tenant concurrency, progress reporting) where asynq's
// own primitives needed a thin layer on top rather than a direct
// configuration.
package asynq

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	asynqlib "github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/jobs"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// Queue is the distributed deployment mode's jobs.Queue implementation --
// see the package doc comment above. Like jobs.StandaloneQueue, Queue is
// returned as its own concrete exported type, not the narrower jobs.Queue
// interface, for the same reason: RegisterHandler/Start/Close are not part
// of jobs.Queue's portable surface (jobs' own queue.go doc comment reserves
// exactly this freedom for the distributed deployment mode's own
// Redis/asynq-backed implementation). Code that only needs the portable
// surface should still depend on jobs.Queue, not this type; see the
// compile-time assertion at the bottom of this file.
type Queue struct {
	client    *asynqlib.Client
	inspector *asynqlib.Inspector
	server    *asynqlib.Server
	rdb       redis.UniversalClient // shared with client/inspector/server; also backs our own cancellation markers (store.go's cancelMarkerKey).

	concurrency              int
	tenantConcurrency        int
	defaultTimeout           time.Duration
	completedRetention       time.Duration
	cancelledRetention       time.Duration
	throttleRetryDelay       time.Duration
	queueWeights             map[string]int
	businessRetryDelayFunc   asynqlib.RetryDelayFunc
	taskCheckInterval        time.Duration
	delayedTaskCheckInterval time.Duration

	handlersMu sync.RWMutex
	handlers   map[string]jobs.Handler

	tenantMu         sync.Mutex
	runningPerTenant map[pkgcore.TenantID]int

	closeRDBOnce sync.Once

	// startMu serializes Start, and started records whether a Start has
	// ever fully succeeded (asynqlib.Server.Start has launched the
	// processor goroutines). Together they replace the old sync.Once gate
	// with the same shape StandaloneQueue.Start uses: a Start that FAILED
	// leaves started false, so a later Start genuinely re-runs
	// asynqlib.Server.Start (which itself only errors when already
	// running), while a Start after a success is a no-op returning nil.
	startMu sync.Mutex
	started bool

	// stopCh is the queue-depth gauge callback's lifecycle signal: it is
	// closed when Close releases the shared Redis client, under
	// depthGaugeMu. Unlike StandaloneQueue, Queue has no dispatcher
	// goroutines of its own to stop -- asynqlib.Server's Shutdown covers
	// those -- so this channel exists solely for the gauge callback's
	// stopped-answer contract (see registerQueueDepthGauge's doc comment).
	// On Close's ctx.Done path the queue keeps running and the channel
	// stays open, which is correct: the data source is still alive there,
	// so the callback may keep answering.
	stopCh chan struct{}

	// depthGaugeMu orders the queue-depth gauge callback against Close,
	// identically to StandaloneQueue's own field: the callback holds the
	// read lock across its stopped-check and its Redis queries, and Close
	// holds the write lock while closing stopCh and closing the shared
	// Redis client, so once Close returns no callback can still be
	// querying a closed client.
	depthGaugeMu sync.RWMutex

	// jobDuration, jobAttempts and jobDeadLetter back the
	// "jobs.job.duration"/"jobs.job.attempts"/"jobs.job.dead_letter"
	// instruments registerJobMetrics wires from Start -- the distributed
	// deployment mode's mirror of StandaloneQueue's identically-named
	// fields (jobs' own standalone_queue.go), so both implementations of
	// the jobs.Queue seam emit the same three metric names under the same
	// jobs.InstrumentationName scope (see AGENTS.md's Observability
	// section). Left at their zero value (nil) until then; worker.go's
	// recordJobMetrics/recordDeadLetter guard against that, mirroring
	// StandaloneQueue's identical fail-open contract -- a metrics wiring
	// failure must not prevent the queue itself from running, nor panic a
	// later job execution.
	jobDuration   metric.Float64Histogram
	jobAttempts   metric.Int64Counter
	jobDeadLetter metric.Int64Counter
}

// Defaults for Queue's construction Options, applied when the corresponding
// With* option is not given. Named package-level constants per the backend
// coding standard's configuration rule (§10), mirroring
// jobs.StandaloneQueue's own Default* constants in spirit.
const (
	// DefaultTenantConcurrencyLimit matches jobs.StandaloneQueue's own
	// DefaultTenantConcurrencyLimit -- see AGENTS.md for why the SAME
	// default is used despite the two deployment modes enforcing it at
	// different points in the pipeline.
	DefaultTenantConcurrencyLimit = 2

	// DefaultCompletedRetention bounds how long a succeeded Job remains
	// visible to Get() after it completes. Unlike StandaloneQueue, whose
	// SQLite row for a succeeded Job is never deleted, asynq deletes a
	// completed task immediately unless told to retain it (asynqlib.
	// Retention) -- Queue always passes this value on every Enqueue call;
	// see AGENTS.md's "Get() after a Job succeeds" section for why
	// omitting it would silently break Get()'s contract for the success
	// path.
	DefaultCompletedRetention = 24 * time.Hour

	// DefaultCancelledRetention bounds how long a cancellation marker
	// (store.go's cancelMarkerKey) survives in Redis, and so how long
	// Get() keeps reporting StatusCancelled for a cancelled Job once its
	// underlying asynq task record has itself expired or been evicted.
	// Unlike StandaloneQueue's SQLite row, this is not forever -- see
	// AGENTS.md's Known limitations.
	DefaultCancelledRetention = 30 * 24 * time.Hour

	// DefaultThrottleRetryDelay is the base of the short, jittered delay
	// used when a Job is bounced back for redelivery because its tenant is
	// at its concurrency limit -- deliberately much shorter than
	// asynqlib.DefaultRetryDelayFunc's exponential business-failure
	// backoff. See errTenantAtCapacity's own doc comment (worker.go).
	DefaultThrottleRetryDelay = 200 * time.Millisecond
)

// defaultQueueWeights is the Config.Queues Queue uses unless overridden by
// WithQueueWeights -- the exact 6/3/1 ratio server.go's own Config.Queues
// doc comment uses as its illustrative example, applied here to our fixed
// critical/default/low tiers (store.go).
var defaultQueueWeights = map[string]int{
	queueCritical: 6,
	queueDefault:  3,
	queueLow:      1,
}

// Option configures a Queue at construction time, mirroring
// jobs.StandaloneQueue's own Option -- a distinct type (not a reuse of
// StandaloneQueue's Option) since the two constructors configure
// structurally different fields, but the same functional-options
// convention throughout this codebase (see observability.Option,
// StandaloneQueue's Option).
//
// Every construction option here follows the same one rule StandaloneQueue's
// Option type states (its doc comment, deliberately kept in step with this
// one): an invalid value -- one this queue cannot honour -- is refused at
// option time with a coded panic (an *apperr.Error carrying the code each
// With* function's own doc comment names), never accepted and silently
// reinterpreted, deferred, or left to fail inside asynq's own background
// goroutines after Start has reported success. Each With* function below
// states its exact rule and why the values it rejects are unhonourable; the
// two server-interval options (WithTaskCheckInterval,
// WithDelayedTaskCheckInterval) state their own rule on top of it -- zero is
// the one sanctioned way to say "asynq's own default", a documented
// pass-through, and a negative value is refused like any other unhonourable
// one. Both implementations of the jobs.Queue seam declare this same
// strategy -- StandaloneQueue's Option type doc comment says so for its own
// construction options -- so the two sides refuse alike by declaration, not
// coincidence, and a future one-sided relaxation is a divergence between two
// implementations of one seam, never a local change.
type Option func(*Queue)

// WithConcurrency sets asynqlib.Config.Concurrency: the maximum number of
// Jobs Queue executes concurrently across all tenants and queues combined,
// the direct analog of StandaloneQueue's WithWorkerCount. Omitted, the
// queue runs at runtime.NumCPU() -- asynqlib.NewServer resolves a zero
// Config.Concurrency that way, which is also this field's construction
// default. An EXPLICIT value below 1 is refused at option time with the
// same coded panic jobs.worker_count_zero StandaloneQueue's WithWorkerCount
// refuses with -- the same invalid concept on the same seam: asynqlib.
// NewServer silently replaces a non-positive Concurrency with NumCPU, so
// an explicit zero or negative could never be honoured literally -- it
// would silently mean "however many CPUs this machine happens to have", a
// machine-dependent guess indistinguishable from an option never passed.
func WithConcurrency(n int) Option {
	if n < 1 {
		panic(apperr.Invalid("jobs.worker_count_zero"))
	}
	return func(q *Queue) { q.concurrency = n }
}

// WithTenantConcurrencyLimit caps how many Jobs belonging to any one tenant
// may be running at once, the direct analog of StandaloneQueue's
// WithTenantConcurrencyLimit -- see AGENTS.md for how this is enforced
// differently (a bounce-and-redeliver inside processTask, not a
// pre-dequeue skip) given what asynq itself offers. Defaults to
// DefaultTenantConcurrencyLimit. A value below 1 is refused at option time
// with a coded panic: a limit of zero would bounce every task of every
// tenant forever (errTenantAtCapacity), silently processing nothing.
func WithTenantConcurrencyLimit(n int) Option {
	if n < 1 {
		panic(apperr.Invalid("jobs.tenant_concurrency_limit_zero"))
	}
	return func(q *Queue) { q.tenantConcurrency = n }
}

// WithQueueWeights overrides the relative weight asynq gives each of the
// three fixed priority queues (store.go's queueForPriority) when choosing
// which to service next -- passed straight through to asynqlib.
// Config.Queues. Defaults to 6/3/1 (critical/default/low), server.go's own
// Config.Queues example ratio.
//
// Every one of the three weights must be at least 1; a weight below 1 is
// refused at option time with a coded panic (jobs.queue_weight_zero):
// asynqlib.NewServer silently DROPS any configured queue whose weight is
// not positive (its own p > 0 filter), while Enqueue keeps routing that
// priority into its fixed tier regardless (queueForPriority) -- so a zero
// or negative weight would not "turn that tier off", it would make the
// tier's Jobs pile up in a queue no processor ever services, silently
// never processed. Every tier this package enqueues into must be one asynq
// actually services.
func WithQueueWeights(critical, normal, low int) Option {
	if critical < 1 || normal < 1 || low < 1 {
		panic(apperr.Invalid("jobs.queue_weight_zero"))
	}
	return func(q *Queue) {
		q.queueWeights = map[string]int{
			queueCritical: critical,
			queueDefault:  normal,
			queueLow:      low,
		}
	}
}

// WithJobTimeout sets the per-attempt timeout applied to an Enqueue call
// that does not use jobs.WithTimeout, the direct analog of StandaloneQueue's
// WithJobTimeout. Defaults to jobs.DefaultTimeout. A value <= 0 is refused
// at option time with the same coded panic jobs.job_timeout_zero
// StandaloneQueue's WithJobTimeout refuses with: a non-positive configured
// default can never be honoured literally here. It would silently break
// the bounded-attempt guarantee two ways. Enqueue hands it to asynqlib.
// Timeout on every Job that did not set its own, and asynq's own
// computeDeadline (processor.go) treats a zero per-task timeout as unset --
// logging an internal error and substituting asynq's own internal 30-minute
// fallback deadline, the configured bound silently replaced; a negative
// value instead lands the deadline in the past, so every attempt's context
// is already expired when Handle runs. And handleErrorAttempt (worker.go)
// builds the FailureHook's OnFailure context with context.WithTimeout(
// q.defaultTimeout), which a non-positive value makes expire instantly --
// the compensation path every failed Job's hook exists to run would never
// get to run.
func WithJobTimeout(d time.Duration) Option {
	if d <= 0 {
		panic(apperr.Invalid("jobs.job_timeout_zero"))
	}
	return func(q *Queue) { q.defaultTimeout = d }
}

// WithCompletedRetention overrides DefaultCompletedRetention. A value <= 0
// is refused at option time with a coded panic
// (jobs.completed_retention_zero): enqueueNew passes this value as
// asynqlib.Retention on every Enqueue (see DefaultCompletedRetention's own
// doc comment for why it is always set), and asynq only keeps a completed
// task's record while that retention is strictly positive (processor.go's
// own msg.Retention > 0 gate) -- a zero or negative value would silently
// fall back to asynq's delete-on-success behavior, and Get() for a
// succeeded Job would answer ErrJobNotFound almost immediately, breaking
// the exact contract this option exists to serve.
func WithCompletedRetention(d time.Duration) Option {
	if d <= 0 {
		panic(apperr.Invalid("jobs.completed_retention_zero"))
	}
	return func(q *Queue) { q.completedRetention = d }
}

// WithCancelledRetention overrides DefaultCancelledRetention. A value <= 0
// is refused at option time with a coded panic
// (jobs.cancelled_retention_zero): writeCancelMarker writes each
// cancellation marker with this value as the Redis TTL, and go-redis only
// attaches a TTL for a positive expiration -- a zero or negative retention
// would silently leave every marker without an expiry, reporting
// StatusCancelled forever and growing without bound, the bounded-survival
// design DefaultCancelledRetention's own doc comment states.
func WithCancelledRetention(d time.Duration) Option {
	if d <= 0 {
		panic(apperr.Invalid("jobs.cancelled_retention_zero"))
	}
	return func(q *Queue) { q.cancelledRetention = d }
}

// WithThrottleRetryDelay overrides DefaultThrottleRetryDelay. A negative
// delay is refused at option time with a coded panic: retryDelay (worker.go)
// feeds the delay through math/rand's jitter, which panics on a negative
// range inside asynq's own processor goroutine -- an unrecovered panic
// would crash the whole process, so the bad value must never reach it.
func WithThrottleRetryDelay(d time.Duration) Option {
	if d < 0 {
		panic(apperr.Invalid("jobs.throttle_retry_delay_negative"))
	}
	return func(q *Queue) { q.throttleRetryDelay = d }
}

// WithRetryDelayFunc overrides the backoff formula applied to a genuine
// Handler failure (never to a tenant-concurrency bounce, which always uses
// WithThrottleRetryDelay's short delay regardless -- see worker.go's
// retryDelay). Defaults to asynqlib.DefaultRetryDelayFunc, letting asynq's
// own exponential-backoff formula stand in for StandaloneQueue's
// hand-rolled backoffDelay rather than reimplementing one; WithBackoff
// (StandaloneQueue's equivalent knob) configures a formula, not a
// pluggable function, only because StandaloneQueue rolls its own -- this
// is the same tuning knob shifted to asynq's own extension point. A nil
// function is refused at option time with a coded panic
// (jobs.retry_delay_func_nil): businessRetryDelayFunc is invoked from
// q.retryDelay -- asynq's own processor and recoverer goroutines call it
// for every genuine Handler failure (processor.go and recoverer.go, both
// outside any recover) -- so a nil override would nil-deref panic there,
// an unrecovered panic crashing the whole process, the identical
// late-crash shape WithThrottleRetryDelay's refusal exists for. Omitting
// the option is the only way to keep asynqlib.DefaultRetryDelayFunc.
func WithRetryDelayFunc(fn asynqlib.RetryDelayFunc) Option {
	if fn == nil {
		panic(apperr.Invalid("jobs.retry_delay_func_nil"))
	}
	return func(q *Queue) { q.businessRetryDelayFunc = fn }
}

// WithTaskCheckInterval sets asynqlib.Config.TaskCheckInterval: how often
// the processor polls a queue it just found empty. Left at zero (the
// default here), asynqlib.NewServer applies its own default of 1 second --
// zero is the sanctioned way to say "asynq's own default", the one
// deliberate delegation this Option family allows. Lowering it
// (integration_test/ uses a few tens of milliseconds) trades Redis polling
// load for faster pickup -- the same trade-off StandaloneQueue's
// WithPollInterval documents for its own dispatcher. A value below zero is
// refused at option time with a coded panic
// (jobs.task_check_interval_negative): asynqlib.NewServer's own defaulting
// (its taskCheckInterval <= 0 check) would silently substitute its
// 1-second default for any negative value too, so a negative explicit
// value could never be honoured literally -- only zero means "the
// default".
func WithTaskCheckInterval(d time.Duration) Option {
	if d < 0 {
		panic(apperr.Invalid("jobs.task_check_interval_negative"))
	}
	return func(q *Queue) { q.taskCheckInterval = d }
}

// WithDelayedTaskCheckInterval sets asynqlib.Config.
// DelayedTaskCheckInterval: how often asynq's forwarder checks scheduled
// and retry tasks for ones now ready to run and moves them to pending.
// Left at zero (the default here), asynqlib.NewServer applies its own
// default of 5 seconds -- zero is the sanctioned way to say "asynq's own
// default", and long enough that a test asserting on WithDelay/
// WithScheduledAt or on retry timing should lower this, exactly as
// integration_test/ does, the same way it lowers WithThrottleRetryDelay
// and StandaloneQueue's own tests lower WithPollInterval/WithBackoff. A
// value below zero is refused at option time with a coded panic
// (jobs.delayed_task_check_interval_negative): unlike TaskCheckInterval,
// asynqlib.NewServer's defaulting catches only exactly zero (its == 0
// check), so a negative value passes straight through to asynq's forwarder
// goroutine, whose timer fires immediately and re-arms to the same
// negative interval -- a busy loop hammering Redis inside asynq's own
// goroutine only after Start has already reported success.
func WithDelayedTaskCheckInterval(d time.Duration) Option {
	if d < 0 {
		panic(apperr.Invalid("jobs.delayed_task_check_interval_negative"))
	}
	return func(q *Queue) { q.delayedTaskCheckInterval = d }
}

// NewQueue returns a Queue connected via redisOpt (typically
// asynqlib.RedisClientOpt for a single Redis instance; asynq also defines
// RedisFailoverClientOpt and RedisClusterClientOpt for sentinel/cluster
// deployments, both accepted unmodified since RedisConnOpt is asynq's own
// interface). Like jobs.NewStandaloneQueue, NewQueue performs no I/O of its
// own: the underlying redis.UniversalClient (go-redis) dials lazily on
// first command, exactly as asynqlib.NewClient/NewServer/NewInspector
// already rely on when constructed this same way.
//
// The SAME redis.UniversalClient backs the asynqlib.Client,
// asynqlib.Inspector and asynqlib.Server this constructs (via
// *FromRedisClient, not separate RedisConnOpt.MakeRedisClient() calls) plus
// Queue's own cancellation-marker bookkeeping -- one shared connection
// pool, not four. Close (below) is therefore the only place any of them
// are closed; per asynq's own documented "shared connection" contract,
// Client.Close/Inspector.Close would simply error if called, so this
// package never calls them.
func NewQueue(redisOpt asynqlib.RedisConnOpt, opts ...Option) *Queue {
	q := &Queue{
		tenantConcurrency:      DefaultTenantConcurrencyLimit,
		defaultTimeout:         jobs.DefaultTimeout,
		completedRetention:     DefaultCompletedRetention,
		cancelledRetention:     DefaultCancelledRetention,
		throttleRetryDelay:     DefaultThrottleRetryDelay,
		queueWeights:           defaultQueueWeights,
		businessRetryDelayFunc: asynqlib.DefaultRetryDelayFunc,
		handlers:               make(map[string]jobs.Handler),
		runningPerTenant:       make(map[pkgcore.TenantID]int),
		stopCh:                 make(chan struct{}),
	}
	for _, opt := range opts {
		opt(q)
	}

	// Same panic-on-unsupported-RedisConnOpt-type this exact type
	// assertion triggers inside asynqlib.NewClient/NewServer/NewInspector
	// -- reproduced here (rather than delegated to one of them) only
	// because constructing the shared client ourselves is what lets
	// client/inspector/server/our own bookkeeping share one pool.
	rdb, ok := redisOpt.MakeRedisClient().(redis.UniversalClient)
	if !ok {
		panic(fmt.Sprintf("jobs/queue/asynq: unsupported asynq.RedisConnOpt type %T", redisOpt))
	}
	q.rdb = rdb
	q.client = asynqlib.NewClientFromRedisClient(rdb)
	q.inspector = asynqlib.NewInspectorFromRedisClient(rdb)
	q.server = asynqlib.NewServerFromRedisClient(rdb, asynqlib.Config{
		Concurrency:              q.concurrency,
		Queues:                   q.queueWeights,
		RetryDelayFunc:           q.retryDelay,
		IsFailure:                isFailure,
		ErrorHandler:             asynqlib.ErrorHandlerFunc(q.handleError),
		TaskCheckInterval:        q.taskCheckInterval,
		DelayedTaskCheckInterval: q.delayedTaskCheckInterval,
	})
	return q
}

// RegisterHandler adds h to the set this Queue dispatches to, keyed by
// h.Type(). Identical contract to StandaloneQueue.RegisterHandler,
// including jobs.ErrDuplicateHandlerType (the same sentinel, reused rather
// than redeclared) and remaining safe to call after Start.
func (q *Queue) RegisterHandler(h jobs.Handler) error {
	q.handlersMu.Lock()
	defer q.handlersMu.Unlock()
	if _, exists := q.handlers[h.Type()]; exists {
		return jobs.ErrDuplicateHandlerType.WithParam("type", h.Type())
	}
	q.handlers[h.Type()] = h
	return nil
}

// handler returns the Handler registered for jobType, or nil.
func (q *Queue) handler(jobType string) jobs.Handler {
	q.handlersMu.RLock()
	defer q.handlersMu.RUnlock()
	return q.handlers[jobType]
}

// Start wires the jobs.queue.depth gauge and the jobs.job.duration /
// jobs.job.attempts / jobs.job.dead_letter instruments (best-effort,
// exactly like StandaloneQueue.Start -- a registration failure is logged
// and does not prevent the server from starting) and launches asynq's own
// background processor goroutines via asynqlib.Server.Start. Like
// Server.Start (and unlike Server.Run), this returns immediately once
// processing has launched; it does not block waiting for shutdown.
//
// A Start that FAILED may be retried by calling Start again: started stays
// false, so the whole sequence genuinely re-runs (asynqlib.Server.Start
// itself only ever errors when already running, never after a failure).
// A Start after a successful one is a no-op returning nil, matching
// StandaloneQueue.Start's own contract -- a Start after Close included:
// this Queue is one-shot, and Close (above) has ended it for good.
func (q *Queue) Start(ctx context.Context) error {
	q.startMu.Lock()
	defer q.startMu.Unlock()
	if q.started {
		return nil
	}
	if err := q.registerQueueDepthGauge(otel.Meter(jobs.InstrumentationName)); err != nil {
		obs.FromContext(ctx).Warn("jobs: registering queue depth gauge failed", "error", err)
	}
	if err := q.registerJobMetrics(); err != nil {
		// Same fail-open contract as registerQueueDepthGauge above.
		obs.FromContext(ctx).Warn("jobs: registering job metrics failed", "error", err)
	}
	if err := q.server.Start(asynqlib.HandlerFunc(q.processTask)); err != nil {
		return err
	}
	q.started = true
	return nil
}

// Close gracefully shuts down asynq's Server -- waiting for in-flight
// attempts to finish naturally, up to ctx's deadline -- then closes the
// shared redis.UniversalClient. See asynqlib.Server.Shutdown's own doc
// comment for the one behavioral nuance from StandaloneQueue.Close worth
// calling out: Shutdown's own internal wait is bounded by asynqlib.Config.
// ShutdownTimeout (default 8s, a construction-time setting, not a per-call
// parameter), not directly by ctx; ctx here bounds how long THIS call
// waits for that to finish, exactly like StandaloneQueue.Close, but does
// not itself cancel an in-flight Handle call either way -- see AGENTS.md's
// "Close/Shutdown" section for the full comparison. Idempotent and safe to
// call more than once, or without a prior Start, exactly like
// StandaloneQueue.Close: asynqlib.Server.Shutdown is itself safe to invoke
// repeatedly and before any Start (server.go's own state-machine check),
// and this package's own addition -- closing the shared redis client -- is
// guarded separately so a second call cannot double-close it.
//
// A Close whose done path has run also ends this Queue object for good --
// it is one-shot, exactly like StandaloneQueue.Close: started stays true,
// so a later Start is that documented no-op returning nil with nothing
// relaunched (asynqlib.Server cannot be started again after Shutdown), and
// Enqueue fails against the now-closed shared redis client. A host whose
// queue must run again builds a new Queue. On the ctx.Done path none of
// this applies -- Close timed out, the server is still processing and the
// client is still open, so the Queue keeps working exactly as before.
//
// The done path also stops the queue-depth gauge, under the same lock that
// orders it against the client close: stopCh is closed and the shared
// client closed inside depthGaugeMu's write section, after which the gauge
// callback answers nil instead of touching the closed client (see
// registerQueueDepthGauge's doc comment). On the ctx.Done path neither
// happens -- the queue is still running and its data source still open --
// so the gauge keeps answering there.
func (q *Queue) Close(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		q.server.Shutdown()
		close(done)
	}()

	select {
	case <-done:
		q.depthGaugeMu.Lock()
		q.closeRDBOnce.Do(func() {
			close(q.stopCh)
			_ = q.rdb.Close()
		})
		q.depthGaugeMu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Enqueue implements jobs.Queue.
func (q *Queue) Enqueue(ctx context.Context, task jobs.Task, opts ...jobs.EnqueueOption) (jobs.JobID, error) {
	if err := task.Validate(); err != nil {
		return "", err
	}

	now := time.Now()
	resolved := jobs.ResolveEnqueueOptions(now, q.defaultTimeout, opts)

	if task.IdempotencyKey == "" {
		id, err := q.enqueueNew(ctx, task, resolved, uuid.NewString(), now)
		if err != nil {
			return "", err
		}
		q.logEnqueued(ctx, task, id)
		return id, nil
	}
	id, fresh, err := q.enqueueIdempotent(ctx, task, resolved, now)
	if err != nil {
		return "", err
	}
	// See StandaloneQueue.Enqueue's identical fix for the full rationale:
	// derive the logger's tenant from task.TenantID -- the Job's own
	// owner -- rather than layering an explicit "tenant_id" kv on top of
	// whatever obs.FromContext(ctx) already auto-attaches from ctx's own
	// ambient tenant, which AGENTS.md documents as legitimately different
	// (or absent) for both Queue.Enqueue and StandaloneQueue.Enqueue alike.
	if fresh {
		q.logEnqueued(ctx, task, id)
	}
	return id, nil
}

// logEnqueued emits the "job enqueued" line both Enqueue paths share after
// a genuinely NEW task was created -- never for an idempotent Enqueue that
// merely returned an existing Job's id, which would misreport that Job as
// freshly enqueued.
func (q *Queue) logEnqueued(ctx context.Context, task jobs.Task, id jobs.JobID) {
	obs.FromContext(pkgcore.WithTenant(ctx, task.TenantID)).Info("job enqueued", "job_id", string(id), "job_type", task.Type)
}

// enqueueNew runs one asynqlib.Client.EnqueueContext call for a task with
// no idempotency key: a fresh random TaskID into the priority queue its
// resolved priority selects. Returns the new Job's id (the TaskID itself).
func (q *Queue) enqueueNew(ctx context.Context, task jobs.Task, resolved jobs.ResolvedEnqueueOptions, taskID string, now time.Time) (jobs.JobID, error) {
	if _, err := q.client.EnqueueContext(ctx,
		asynqlib.NewTaskWithHeaders(task.Type, task.Payload, buildTaskHeaders(task, now)),
		asynqlib.TaskID(taskID),
		asynqlib.Queue(queueForPriority(resolved.Priority)),
		asynqlib.MaxRetry(resolved.MaxRetries),
		asynqlib.Timeout(resolved.Timeout),
		// Always set -- see DefaultCompletedRetention's own doc comment
		// for why omitting this would break Get() for a succeeded Job
		// almost immediately.
		asynqlib.Retention(q.completedRetention),
		asynqlib.ProcessAt(resolved.ScheduledAt),
	); err != nil {
		if errors.Is(err, asynqlib.ErrTaskIDConflict) {
			// Defensive: a random uuid collision is astronomically
			// unlikely, but the pre-existing code resolved the conflict
			// by returning the colliding task's id, so keep that answer.
			existing, ferr := q.findTaskInfo(taskID)
			if ferr != nil {
				// This Enqueue carries NO idempotency key -- taskID is this
				// call's own fresh random uuid -- so the conflict is exactly
				// what the comment above says it is: a uuid collision with an
				// existing task's id. The message names the mechanism (never
				// an idempotency-key dedupe, which does not exist on this
				// path) and what to look for: the colliding task's own
				// record, reachable under the id the message names.
				return "", fmt.Errorf("jobs: new task's id %s collided with an existing job's id (uuid collision); look up that existing job's record to reconcile: %w", taskID, ferr)
			}
			return jobs.JobID(existing.ID), nil
		}
		return "", fmt.Errorf("jobs: enqueue job: %w", err)
	}
	return jobs.JobID(taskID), nil
}

// idempotencyClaimTTL bounds how long the per-key claim below may outlive
// the Enqueue call that won it. The claim exists only to serialize
// concurrent first-inserts of one key; the winner releases it as soon as
// its own insert finished, so the TTL is never the dedupe's lifetime (the
// task record itself is) -- it only bounds how long a duplicate Enqueue
// waits when the winner crashed between claiming and inserting, after which
// the claim lapses and a duplicate takes over. Thirty seconds is far longer
// than any pause a live winner can take between two Redis commands, and
// far shorter than any wait an operator would accept after a crash.
const idempotencyClaimTTL = 30 * time.Second

// idempotencyTaskID derives a keyed Task's deterministic id, exactly as
// before (AGENTS.md's idempotency section): "idem:" + tenant + ":" + key.
// The deterministic id is what makes the dedupe answer stable -- a second
// Enqueue for the same pair returns this same string, which is also the
// JobID Get/Cancel/DeadLetterJobs resolve by -- and its lifetime in asynq's
// own records is the dedupe's lifetime, unchanged by the claim machinery
// below.
//
// The composition is verbatim: the caller's key text is this string's
// tail, and through it the tail of the JobID every job_id attribute this
// queue logs carries. Task.IdempotencyKey's caller-side warning is
// therefore the only content gate the key text ever passes -- this
// function deliberately does no content screening of its own.
func idempotencyTaskID(task jobs.Task) string {
	return "idem:" + string(task.TenantID) + ":" + task.IdempotencyKey
}

// idempotencyClaimKey is the Redis key under which concurrent Enqueue calls
// for one idempotency key serialize their inserts. It lives in this
// package's own namespaced keyspace ("asynqjobs:..."), never asynq's, and
// is deliberately separate from the task record itself: asynq's own
// TaskID-uniqueness check is scoped PER QUEUE (internal/rdb's enqueueCmd
// only ever looks at asynq:{<qname>}:t:<taskID> -- confirmed against the
// pinned v0.26.0 source), so the same deterministic TaskID enqueued into
// two of this package's three priority queues would pass both checks and
// create two independent, concurrently-executing copies of one Job. The
// claim key is what makes the check-and-insert atomic ACROSS the three
// queues, letting the keyed task still land in its own priority queue.
func idempotencyClaimKey(taskID string) string { return "asynqjobs:enqueue-claim:" + taskID }

// enqueueIdempotent implements the keyed half of Enqueue: it creates one
// Job per (TenantID, IdempotencyKey) pair no matter which of the three
// priority queues the callers chose, returning the first Job's id --
// queue-independently -- while still enqueueing that first Job into the
// priority queue its own WithPriority selected (per-priority delivery
// semantics survive; only a duplicate Enqueue is deflected to the
// original). Fresh reports whether this call created the task (true) or
// returned an existing Job's id (false), so Enqueue can log accordingly.
//
// The mechanism: the deterministic TaskID stays asynq's own dedupe record,
// exactly as before -- while a task under that id exists anywhere in the
// three queues, any Enqueue for the pair returns it. What the claim key
// adds is atomicity for the CONCURRENT-first-insert race asynq's
// per-queue check cannot arbitrate: two same-key Enqueues at different
// priorities could both pass their own queue's check and both insert. The
// winner of a redis SET NX on the claim key is the single inserter; it
// re-probes for a task a completed enqueue may have left behind, inserts at
// its own priority, and releases the claim. Losers wait -- the winner's
// insert lands in milliseconds -- for the task to appear (dedupe answer) or
// the claim to lapse (the winner crashed mid-insert; the loser retakes and
// becomes the inserter itself). ctx bounds the wait. A stale claim can
// never wedge the key: it expires on its own after idempotencyClaimTTL.
func (q *Queue) enqueueIdempotent(ctx context.Context, task jobs.Task, resolved jobs.ResolvedEnqueueOptions, now time.Time) (id jobs.JobID, fresh bool, err error) {
	taskID := idempotencyTaskID(task)
	claimKey := idempotencyClaimKey(taskID)
	for {
		won, err := q.rdb.SetNX(ctx, claimKey, "1", idempotencyClaimTTL).Result()
		if err != nil {
			return "", false, fmt.Errorf("jobs: claim idempotent enqueue: %w", err)
		}
		if won {
			id, fresh, err := q.enqueueIdempotentClaimed(ctx, task, resolved, taskID, queueForPriority(resolved.Priority), now)
			if delErr := q.rdb.Del(context.Background(), claimKey).Err(); delErr != nil {
				// Best-effort: an unreleased claim self-heals through its
				// TTL, so a release failure only delays (never breaks) a
				// concurrent duplicate.
				obs.FromContext(ctx).Warn("jobs: releasing idempotent enqueue claim failed", "job_id", taskID, "error", delErr)
			}
			return id, fresh, err
		}
		// Another Enqueue holds the claim for this key right now. Wait for
		// its task to become visible (then the outer loop's next probe
		// settles the dedupe) or for its claim to lapse (it crashed
		// mid-insert; the outer loop retakes the claim).
		if _, found, err := q.waitForIdempotentTask(ctx, taskID, claimKey); err != nil {
			return "", false, err
		} else if found {
			return jobs.JobID(taskID), false, nil
		}
	}
}

// enqueueIdempotentClaimed is the winner's half of enqueueIdempotent, run
// while holding the claim: it re-probes the three queues (a completed
// enqueue may have inserted since the loser loop last looked -- dedupe on
// it), then inserts at the winner's own priority queue.
func (q *Queue) enqueueIdempotentClaimed(ctx context.Context, task jobs.Task, resolved jobs.ResolvedEnqueueOptions, taskID, queueName string, now time.Time) (jobs.JobID, bool, error) {
	if info, err := q.findTaskInfo(taskID); err == nil {
		return jobs.JobID(info.ID), false, nil
	} else if !errors.Is(err, jobs.ErrJobNotFound) {
		return "", false, fmt.Errorf("jobs: look up existing job for idempotency key: %w", err)
	}
	if _, err := q.client.EnqueueContext(ctx,
		asynqlib.NewTaskWithHeaders(task.Type, task.Payload, buildTaskHeaders(task, now)),
		asynqlib.TaskID(taskID),
		asynqlib.Queue(queueName),
		asynqlib.MaxRetry(resolved.MaxRetries),
		asynqlib.Timeout(resolved.Timeout),
		asynqlib.Retention(q.completedRetention),
		asynqlib.ProcessAt(resolved.ScheduledAt),
	); err != nil {
		if errors.Is(err, asynqlib.ErrTaskIDConflict) {
			// Defensive: a task under this deterministic id appeared
			// between the probe above and this insert -- only possible if
			// another writer enqueued it without holding the claim (a
			// replica still running a pre-fix release, whose per-queue
			// conflict check this id passed because it landed in another
			// queue). Dedupe on it rather than failing the call.
			if existing, ferr := q.findTaskInfo(taskID); ferr == nil {
				return jobs.JobID(existing.ID), false, nil
			}
		}
		return "", false, fmt.Errorf("jobs: enqueue job: %w", err)
	}
	return jobs.JobID(taskID), true, nil
}

// waitForIdempotentTask is the loser's wait loop: it polls until either a
// task under taskID is visible in one of the three queues (the claim holder
// finished inserting -- the caller returns that id) or the claim key is
// gone (the holder crashed before inserting -- the caller retakes the
// claim and inserts itself). found distinguishes the two, so the caller
// never pays a redundant claim round trip when the task is already there.
func (q *Queue) waitForIdempotentTask(ctx context.Context, taskID, claimKey string) (jobs.JobID, bool, error) {
	for {
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case <-time.After(15 * time.Millisecond):
		}
		if info, err := q.findTaskInfo(taskID); err == nil {
			return jobs.JobID(info.ID), true, nil
		} else if !errors.Is(err, jobs.ErrJobNotFound) {
			return "", false, fmt.Errorf("jobs: look up existing job for idempotency key: %w", err)
		}
		exists, err := q.rdb.Exists(ctx, claimKey).Result()
		if err != nil {
			return "", false, fmt.Errorf("jobs: check idempotent enqueue claim: %w", err)
		}
		if exists == 0 {
			return "", false, nil
		}
	}
}

// Get implements jobs.Queue.
func (q *Queue) Get(ctx context.Context, id jobs.JobID) (*jobs.Job, error) {
	info, err := q.findTaskInfo(string(id))
	if err != nil {
		return nil, err
	}
	tenantID := pkgcore.TenantID(info.Headers[headerTenantID])
	if !jobs.CallerMayAccess(ctx, tenantID) {
		return nil, jobs.ErrJobNotFound
	}

	cancelledAt, cerr := q.readCancelMarker(ctx, string(id))
	if cerr != nil {
		// Fail closed, restating dispatchAfterMarkerRead's own rationale
		// (worker.go) on the reporting side: an unreadable cancellation
		// state must never let a possibly-cancelled Job be reported as
		// succeeded -- processTask records a cancelled task's skipped run
		// as an ordinary asynq Completed, so a swallowed read failure
		// would answer StatusSucceeded with an empty Result (the skip
		// never writes one). The execution side chose bounce-class refusal
		// because a transient outage should delay rather than lose; the
		// reporting side's equivalent is returning this error for the
		// caller to retry -- and the marker is permanent (the contract
		// above), so a retry necessarily gets the right answer: a
		// transient marker outage delays the report instead of corrupting
		// it.
		return nil, fmt.Errorf("jobs: read cancellation marker: %w", cerr)
	}
	return jobFromTaskInfo(info, cancelledAt), nil
}

// Cancel implements jobs.Queue. See AGENTS.md's "Cancel" section: the
// cancellation marker (store.go) is the ONLY authoritative, permanent
// effect -- Get() reports StatusCancelled from it unconditionally,
// exactly mirroring StandaloneQueue's own markCancelled +
// completeSucceeded/completeRetrying/completeDeadLetter no-op-when-not-
// running guard. "Unconditionally" carries one fail-closed exception: a
// marker that cannot be READ (Redis answered an error, never "no
// marker") makes Get() and DeadLetterJobs return an error rather than
// report the Job's natural asynq state -- see Get's marker-read
// handling above, which restates dispatchAfterMarkerRead's rationale on
// the reporting side.
//
// Cancel deliberately does NOT call Inspector.DeleteTask for a not-yet-
// running Job, even though that looks like the obvious way to stop asynq
// from ever processing it: DeleteTask removes the task's entire TaskInfo
// record, and Get() would then have nothing left to combine the
// cancellation marker with -- id would look exactly like an id that never
// existed, i.e. jobs.ErrJobNotFound, breaking Get()'s contract for a
// cancelled Job (this was a genuine bug in an earlier version of this
// method, caught specifically by integration_test/cancel_test.go's
// TestRedisQueue_Cancel_PendingJobNeverRuns running against a real Redis --
// a unit test built on a hand-constructed *asynqlib.TaskInfo, never having
// actually exercised a real delete, could not have caught it -- the
// concrete reason this task's own instructions required real-backend
// testing here rather than mocks). Instead:
//
//  1. Cancel leaves the task's own asynq record alone, and just writes the
//     marker (plus, for an already-Active Job, a best-effort
//     Inspector.CancelProcessing signal -- see "The tenant context trap"
//     above for why that can actually interrupt a running Handle call for
//     Queue, unlike StandaloneQueue).
//  2. processTask checks for exactly this same marker as the very FIRST
//     thing it does whenever this task is next dequeued -- returning nil
//     without ever calling Handle if a marker exists, which asynq records
//     as an ordinary successful completion (retained for
//     completedRetention, same as a real success) rather than a retry or
//     an archive.
//  3. Get()'s marker-overlay (step 0 above) then reports StatusCancelled
//     for that Job regardless, using the still-fully-intact TaskInfo for
//     every other field (Type, Payload, TenantID, etc.).
//
// This correctly satisfies "cancelling a StatusPending/StatusRetrying Job
// prevents it from ever being dispatched" (Handle never runs) without ever
// breaking Get()'s visibility contract. The accepted cost: every dispatched
// task now costs one extra Redis round trip (the marker check) even when
// never cancelled, and asynq's own internal processed/failed counters
// (visible via Inspector.GetQueueInfo or asynqmon) count a
// cancelled-before-dispatch Job as "processed" -- asynq has no third
// bucket for "revoked."
//
// For an already-Active Job, Cancel also best-effort signals
// Inspector.CancelProcessing -- strictly better than, and not in tension
// with, StandaloneQueue's own documented "does not preempt" limitation
// (jobs.Queue.Cancel's doc comment only ever promises a running Job "is
// allowed to" keep executing, never that it is guaranteed to). Its failure
// is logged, never returned, since the marker alone already satisfies the
// contract either way.
//
// The marker is written BEFORE the CancelProcessing signal is sent, and
// deliberately so: the signal's only effect is to interrupt the in-flight
// attempt, and every failure-processing path that interruption can trigger
// inside asynq's own dispatch loop (processor.go's handleFailedMessage
// invokes this package's handleError -- and through it handleErrorAttempt's
// cancel-marker check -- for a ctx-cancelled attempt exactly as for a
// handler-returned error) must find the cancellation already durably
// observable, never race it. A Cancel that has returned therefore
// guarantees: any OnFailure decision made for this Job from then on sees
// StatusCancelled, mirroring StandaloneQueue's "the cancellation wins"
// semantics. Writing the marker first also means a marker-write failure
// leaves the attempt completely uninterrupted -- previously the signal
// could go out and only the marker fail, interrupting an attempt whose
// cancellation was never recorded.
func (q *Queue) Cancel(ctx context.Context, id jobs.JobID) error {
	info, err := q.findTaskInfo(string(id))
	if err != nil {
		return err
	}
	tenantID := pkgcore.TenantID(info.Headers[headerTenantID])
	if !jobs.CallerMayAccess(ctx, tenantID) {
		return jobs.ErrJobNotFound
	}

	if already, _ := q.readCancelMarker(ctx, string(id)); already != nil {
		return nil // idempotent: already cancelled.
	}
	if info.State == asynqlib.TaskStateCompleted || info.State == asynqlib.TaskStateArchived {
		return nil // idempotent: already otherwise terminal.
	}

	if merr := q.writeCancelMarker(ctx, string(id)); merr != nil {
		return fmt.Errorf("jobs: record cancellation: %w", merr)
	}

	if info.State == asynqlib.TaskStateActive {
		if cerr := q.inspector.CancelProcessing(string(id)); cerr != nil {
			obs.FromContext(ctx).Warn("jobs: best-effort CancelProcessing signal failed", "job_id", string(id), "error", cerr)
		}
	}
	return nil
}

// DeadLetterJobs returns every archived (dead-lettered) Job ctx may access,
// across all three priority queues -- the asynq-backed analog of
// StandaloneQueue.DeadLetterJobs, mapping onto asynq's own archived-task
// mechanism (Inspector.ListArchivedTasks) rather than a parallel one; see
// AGENTS.md's dead-letter mapping section. Not part of the jobs.Queue
// interface, exactly like StandaloneQueue's version. Fully drains each
// queue's archive (paginating internally) rather than truncating at one
// page, though it still exposes no pagination of its own to the caller --
// the same "no pagination" shape StandaloneQueue.DeadLetterJobs documents
// as a known limitation.
func (q *Queue) DeadLetterJobs(ctx context.Context) ([]*jobs.Job, error) {
	const pageSize = 100
	var result []*jobs.Job
	for _, queueName := range priorityQueues {
		for page := 1; ; page++ {
			infos, err := q.inspector.ListArchivedTasks(queueName, asynqlib.PageSize(pageSize), asynqlib.Page(page))
			if err != nil {
				if errors.Is(err, asynqlib.ErrQueueNotFound) {
					break
				}
				return nil, fmt.Errorf("jobs: list archived jobs in queue %s: %w", queueName, err)
			}
			for _, info := range infos {
				tenantID := pkgcore.TenantID(info.Headers[headerTenantID])
				if !jobs.CallerMayAccess(ctx, tenantID) {
					continue
				}
				cancelledAt, cerr := q.readCancelMarker(ctx, info.ID)
				if cerr != nil {
					// Fail closed, exactly as Get does above: an archived
					// Job a concurrent Cancel may have settled as
					// StatusCancelled (a Cancel landing during its terminal
					// attempt -- handleErrorAttempt -- archives with the
					// marker still intact) must never be reported as its
					// natural StatusDeadLetter while that cancellation
					// state cannot be read. The whole listing fails rather
					// than silently misreporting one row; the marker is
					// permanent, so a retried listing necessarily gets the
					// right answer.
					return nil, fmt.Errorf("jobs: read cancellation marker for job %s in queue %s: %w", info.ID, queueName, cerr)
				}
				result = append(result, jobFromTaskInfo(info, cancelledAt))
			}
			if len(infos) < pageSize {
				break
			}
		}
	}
	return result, nil
}

// findTaskInfo locates id by probing each of priorityQueues in turn --
// asynqlib.Inspector.GetTaskInfo requires a queue name and offers no
// search-by-id-alone primitive, so this bounded, three-probe fan-out
// (never more, since Queue only ever enqueues into these three) is how
// every Get/Cancel/idempotency-conflict lookup in this file finds a Job
// without the caller having to remember which priority it used.
func (q *Queue) findTaskInfo(id string) (*asynqlib.TaskInfo, error) {
	for _, queueName := range priorityQueues {
		info, err := q.inspector.GetTaskInfo(queueName, id)
		switch {
		case err == nil:
			return info, nil
		case errors.Is(err, asynqlib.ErrTaskNotFound), errors.Is(err, asynqlib.ErrQueueNotFound):
			continue
		default:
			return nil, fmt.Errorf("jobs: look up job: %w", err)
		}
	}
	return nil, jobs.ErrJobNotFound
}

// cancelMarkerKey namespaces Queue's own cancellation-marker keys clearly
// apart from every key asynq itself owns (all under "asynq:...", per
// internal/base) to eliminate any collision risk on the shared
// redis.UniversalClient.
func cancelMarkerKey(id string) string { return "asynqjobs:cancelled:" + id }

// writeCancelMarker records that id has been cancelled, expiring after
// q.cancelledRetention -- see DefaultCancelledRetention's own doc comment
// for why this is bounded rather than permanent.
func (q *Queue) writeCancelMarker(ctx context.Context, id string) error {
	return q.rdb.Set(ctx, cancelMarkerKey(id), time.Now().UTC().Format(time.RFC3339Nano), q.cancelledRetention).Err()
}

// readCancelMarker reports id's cancellation time, or nil if it was never
// cancelled (or its marker has since expired).
func (q *Queue) readCancelMarker(ctx context.Context, id string) (*time.Time, error) {
	val, err := q.rdb.Get(ctx, cancelMarkerKey(id)).Result()
	switch {
	case errors.Is(err, redis.Nil):
		return nil, nil
	case err != nil:
		return nil, err
	}
	t, perr := time.Parse(time.RFC3339Nano, val)
	if perr != nil {
		return nil, fmt.Errorf("jobs: parse cancellation marker: %w", perr)
	}
	return &t, nil
}

// registerQueueDepthGauge wires the same "jobs.queue.depth" instrument name
// StandaloneQueue.registerQueueDepthGauge does (docs/internal/09-
// observability.md's must-instrument table), reusing jobs.
// InstrumentationName so both implementations share one instrumentation
// scope. The label set differs from StandaloneQueue's, and deliberately so
// -- see AGENTS.md's observability section: asynq's Inspector.GetQueueInfo
// reports backlog per QUEUE (one of our three priority tiers), not per job
// TYPE the way StandaloneQueue's own SQL GROUP BY can, so this gauge is
// labeled (queue, status) instead of (job_type, status). Both label sets
// are low-cardinality and neither ever includes tenant_id, per root
// CLAUDE.md's Prometheus-cardinality rule.
//
// The callback reports only the tiers asynq's own registry actually names,
// discovered per scrape from Inspector.Queues rather than assumed:
// asynq registers a queue only when its first task lands on it, so on a
// fresh deployment most of the tier set has never been registered. Probing
// a never-registered tier with GetQueueInfo is the one Inspector path
// whose missing-queue answer does not wrap the exported
// asynqlib.ErrQueueNotFound (asynq's GetQueueInfo delegates to its
// CurrentStats, which reports an internal NotFound instead), so an
// errors.Is tolerance against that sentinel never matched and a single
// traffic-less tier used to fail every Collect with NOT_FOUND until a task
// had reached it -- the defect the ListQueues-driven discovery fixes (see
// the regression pair in go/jobs/integration_test's queue_depth_gauge_test
// and job_metrics_test).
//
// The gauge's callback carries the same unregisterable-lifecycle contract
// StandaloneQueue.registerQueueDepthGauge's own doc comment records: it
// holds depthGaugeMu's read lock across its stopped-check AND its Redis
// queries, and Close holds the write lock while closing stopCh and the
// shared Redis client, so once Close returns no callback is mid-query and
// every later callback answers nil. One nuance is Queue-specific: Close's
// ctx.Done path stops nothing -- the server is still processing and the
// Redis client is still open -- so the callback legitimately keeps
// answering there; only the done path releases the data source and stops
// the gauge.
func (q *Queue) registerQueueDepthGauge(meter metric.Meter) error {
	_, err := meter.Int64ObservableGauge(
		"jobs.queue.depth",
		metric.WithDescription("Number of jobs waiting to run (pending or retrying), by queue and status."),
		metric.WithUnit("{job}"),
		metric.WithInt64Callback(func(ctx context.Context, o metric.Int64Observer) error {
			q.depthGaugeMu.RLock()
			defer q.depthGaugeMu.RUnlock()
			select {
			case <-q.stopCh:
				return nil
			default:
			}
			// Discover which of the three priority tiers this Queue
			// actually owns from asynq's queue registry (its asynq:queues
			// set) instead of probing the fixed tier set: asynq registers
			// a queue only when its first task lands on it, and
			// GetQueueInfo for a never-registered tier answers an
			// internal NotFound that does not wrap the exported
			// asynqlib.ErrQueueNotFound -- see this function's doc
			// comment for the failure that caused. Enumerating first
			// makes absence a fact of the registry, not an error shape
			// to recognize; the membership filter keeps the (queue,
			// status) label set bounded to this Queue's own three tiers,
			// so a queue name another asynq application shares this
			// Redis DB with can never appear under our gauge either.
			registeredQueues, err := q.inspector.Queues()
			if err != nil {
				return err
			}
			registered := make(map[string]bool, len(registeredQueues))
			for _, queueName := range registeredQueues {
				registered[queueName] = true
			}
			for _, queueName := range priorityQueues {
				if !registered[queueName] {
					// A tier with no traffic yet: nothing to report.
					continue
				}
				info, err := q.inspector.GetQueueInfo(queueName)
				if err != nil {
					if errors.Is(err, asynqlib.ErrQueueNotFound) {
						continue
					}
					return err
				}
				o.Observe(int64(info.Pending), metric.WithAttributes(
					attribute.String("queue", queueName),
					attribute.String("status", string(jobs.StatusPending)),
				))
				o.Observe(int64(info.Retry), metric.WithAttributes(
					attribute.String("queue", queueName),
					attribute.String("status", string(jobs.StatusRetrying)),
				))
			}
			return nil
		}),
	)
	return err
}

// The three "jobs.job.*" instrument-name literals StandaloneQueue's own
// registerJobMetrics uses (jobs' standalone_queue.go's
// jobDurationMetricName/jobAttemptsMetricName/jobDeadLetterMetricName
// constants, which are package-private there) -- spelled out here rather
// than exported from the root package: this subpackage is the one other
// place that must emit these exact names, and the root package's
// observability section (AGENTS.md) is the authority that keeps the two
// spellings in step, exactly like the shared "jobs.queue.depth" literal
// both implementations already use.
const (
	jobDurationMetricName   = "jobs.job.duration"
	jobAttemptsMetricName   = "jobs.job.attempts"
	jobDeadLetterMetricName = "jobs.job.dead_letter"
)

// registerJobMetrics wires the "jobs.job.duration" Histogram and
// "jobs.job.attempts"/"jobs.job.dead_letter" Counters
// docs/internal/09-observability.md's must-instrument table requires for
// the task-queue domain beyond queue backlog depth, mirroring
// jobs.StandaloneQueue.registerJobMetrics (jobs' own standalone_queue.go)
// instrument for instrument: the same three names, the same units, the
// same (job_type, status) label sets and the same fail-open registration
// shape. This is the recorded fix for the Known-limitations gap that only
// "jobs.queue.depth" was wired here (docs/internal/17-risks.md's task-queue
// row; go/jobs/AGENTS.md's asynq.Queue-specific Known limitations): the
// execution-duration percentiles, failure rate, retry count and
// dead-letter count rows of the must-instrument table now exist on both
// deployment modes. Like StandaloneQueue's twin, these three are
// synchronous instruments recorded imperatively at the point each attempt
// concludes (worker.go's recordJobMetrics/recordDeadLetter), never an
// ObservableGauge callback, since "how long did this attempt take" and
// "did this attempt fail" are events, not a value that can be sampled on
// demand from Redis. Labeled by job_type and status only -- deliberately
// never tenant_id, for the identical cardinality reason the depth-gauge
// callback's own doc comment gives.
func (q *Queue) registerJobMetrics() error {
	meter := otel.Meter(jobs.InstrumentationName)

	duration, err := meter.Float64Histogram(
		jobDurationMetricName,
		metric.WithDescription("Duration of one job Handle attempt, in seconds, by job type and resulting status."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return err
	}

	attempts, err := meter.Int64Counter(
		jobAttemptsMetricName,
		metric.WithDescription("Number of job Handle attempts completed, by job type and resulting status (succeeded, retrying or dead_letter). Failure rate and retry count are both derivable from this by status."),
		metric.WithUnit("{attempt}"),
	)
	if err != nil {
		return err
	}

	deadLetter, err := meter.Int64Counter(
		jobDeadLetterMetricName,
		metric.WithDescription("Number of jobs moved to the dead letter status, by job type."),
		metric.WithUnit("{job}"),
	)
	if err != nil {
		return err
	}

	q.jobDuration, q.jobAttempts, q.jobDeadLetter = duration, attempts, deadLetter
	return nil
}

// recordJobMetrics records one completed Handle attempt on the
// "jobs.job.duration" Histogram and "jobs.job.attempts" Counter
// registerJobMetrics wires, labeled by jobType and status -- the exact
// mirror of jobs.StandaloneQueue.recordJobMetrics (jobs' own worker.go):
// the same two instruments, the same labels, the same nil-guard (a nil
// q.jobDuration means registerJobMetrics never ran or failed, and
// registration always sets the fields together, so checking one stands for
// both). Unlike its StandaloneQueue twin, whose call sites all sit after
// the outcome-persisting write, this Queue's call sites sit at the points
// asynq's own dispatch loop decides an attempt's outcome -- asynq offers
// no hook after it settles the task, so the record precedes the
// settlement write exactly like this Queue's FailureHook.OnFailure calls
// do (worker.go's handleErrorAttempt); see AGENTS.md's "Dead-letter
// mapping" section for the ordering concession and its one documented edge
// case, which apply identically to the metrics.
func (q *Queue) recordJobMetrics(jobType string, status jobs.Status, duration time.Duration) {
	if q.jobDuration == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("job_type", jobType),
		attribute.String("status", string(status)),
	)
	q.jobDuration.Record(context.Background(), duration.Seconds(), attrs)
	q.jobAttempts.Add(context.Background(), 1, attrs)
}

// recordJobMetricsAttemptOnly records one completed attempt on the
// "jobs.job.attempts" Counter alone, without a duration data point on the
// "jobs.job.duration" Histogram. It exists for the one class of attempt
// whose duration this Queue genuinely cannot measure: an attempt that
// concluded without a measured Handle run -- a terminal-attempt
// tenant-concurrency bounce or cancellation-marker refusal that asynq
// archives, or a Handle that panicked (asynq's own processor recovers the
// panic and hands the failure to the ErrorHandler with no timing
// metadata, worker.go's perform). Counting the outcome while omitting the
// duration keeps both instruments truthful to their own descriptions --
// the histogram measures Handle-attempt durations, and no such duration
// exists for these -- while the attempts row still reflects the outcome
// DeadLetterJobs reports. Every other attempt records through
// recordJobMetrics with a measured duration. Same nil-guard discipline,
// guarding on q.jobAttempts alone since this call never touches
// q.jobDuration.
func (q *Queue) recordJobMetricsAttemptOnly(jobType string, status jobs.Status) {
	if q.jobAttempts == nil {
		return
	}
	q.jobAttempts.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("job_type", jobType),
		attribute.String("status", string(status)),
	))
}

// recordDeadLetter records one job moving to StatusDeadLetter on the
// "jobs.job.dead_letter" Counter registerJobMetrics wires, labeled by
// jobType only -- the exact mirror of
// jobs.StandaloneQueue.recordDeadLetter (jobs' own worker.go), including
// the identical nil-guard rationale.
func (q *Queue) recordDeadLetter(jobType string) {
	if q.jobDeadLetter == nil {
		return
	}
	q.jobDeadLetter.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("job_type", jobType),
	))
}

// compile-time check that *Queue satisfies jobs.Queue, mirroring
// StandaloneQueue's identical assertion.
var _ jobs.Queue = (*Queue)(nil)
