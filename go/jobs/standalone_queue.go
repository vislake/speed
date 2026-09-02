package jobs

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"gorm.io/gorm"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// instrumentationName identifies StandaloneQueue's own tracer/meter, mirroring
// observability.Middleware's identical use of its own package path for the
// same purpose.
const instrumentationName = "github.com/vislake/speed/go/jobs"

// Metric instrument names StandaloneQueue registers under instrumentationName,
// beyond "jobs.queue.depth" (registerQueueDepthGauge's own literal, kept
// inline there since it has no other reference site): the execution-
// duration-percentiles, failure-rate/retry-count and dead-letter-count
// rows docs/internal/09-observability.md's must-instrument table requires
// for the task-queue domain, on top of the queue-backlog-depth row that
// gauge already covers. See registerJobMetrics for how these are wired,
// and worker.go's recordJobMetrics/recordDeadLetter for where they are
// recorded -- mirroring observability.Middleware's own named-constants-
// plus-Counter/Histogram pattern for the HTTP row of the same table
// (go/observability/middleware.go's requestCountName/requestDurationName).
const (
	jobDurationMetricName   = "jobs.job.duration"
	jobAttemptsMetricName   = "jobs.job.attempts"
	jobDeadLetterMetricName = "jobs.job.dead_letter"
)

// Defaults for StandaloneQueue's construction Options, applied when the
// corresponding With* option is not given. Named package-level constants
// per the backend coding standard §10. Unlike dbkit's connection-pool
// limits (deliberately fixed, "so there is exactly one place to
// reconsider them"), worker-pool sizing and per-tenant concurrency really
// are deployment-dependent — a small standalone deployment and a beefier
// one legitimately want different values — which is why this package exposes
// them as Options instead of hard-coding them the way dbkit.Options does
// for its own pool limits.
const (
	// DefaultWorkerCount is how many Jobs a StandaloneQueue executes
	// concurrently across all tenants combined, unless overridden by
	// WithWorkerCount.
	DefaultWorkerCount = 4

	// DefaultTenantConcurrencyLimit is how many Jobs belonging to any one
	// tenant a StandaloneQueue runs at once, unless overridden by
	// WithTenantConcurrencyLimit -- see that option's own doc comment for
	// why this cap exists at all.
	DefaultTenantConcurrencyLimit = 2

	// DefaultPollInterval is how often a StandaloneQueue's dispatcher checks
	// for newly-eligible Jobs, unless overridden by WithPollInterval.
	DefaultPollInterval = 200 * time.Millisecond

	// DefaultBackoffBase is the base of the exponential retry backoff a
	// StandaloneQueue applies between failed attempts, unless overridden by
	// WithBackoff. See backoffDelay for the exact formula this seeds.
	DefaultBackoffBase = 1 * time.Second

	// DefaultBackoffMax caps the exponential retry backoff WithBackoff
	// would otherwise grow without bound.
	DefaultBackoffMax = 5 * time.Minute
)

// ErrDuplicateHandlerType is returned by RegisterHandler when a Handler
// for the same Type is already registered on this StandaloneQueue.
var ErrDuplicateHandlerType = apperr.Invalid("jobs.duplicate_handler_type")

// Option configures a StandaloneQueue at construction time.
type Option func(*StandaloneQueue)

// WithWorkerCount sets how many Jobs StandaloneQueue executes concurrently
// across all tenants combined. Defaults to DefaultWorkerCount.
func WithWorkerCount(n int) Option {
	return func(q *StandaloneQueue) { q.workerCount = n }
}

// WithTenantConcurrencyLimit caps how many Jobs belonging to any one
// tenant may be StatusRunning at once, so one tenant's backlog cannot
// starve every worker (docs/internal/07-platform-services.md). Defaults to
// DefaultTenantConcurrencyLimit.
func WithTenantConcurrencyLimit(n int) Option {
	return func(q *StandaloneQueue) { q.tenantConcurrency = n }
}

// WithPollInterval sets how often the dispatcher checks for newly-eligible
// Jobs. Defaults to DefaultPollInterval.
func WithPollInterval(d time.Duration) Option {
	return func(q *StandaloneQueue) { q.pollInterval = d }
}

// WithJobTimeout sets the per-attempt timeout applied to an Enqueue call
// that does not use WithTimeout. Defaults to DefaultTimeout.
func WithJobTimeout(d time.Duration) Option {
	return func(q *StandaloneQueue) { q.defaultTimeout = d }
}

// WithBackoff sets the exponential retry backoff's base and cap. Defaults
// to DefaultBackoffBase and DefaultBackoffMax.
func WithBackoff(base, max time.Duration) Option {
	return func(q *StandaloneQueue) { q.backoffBase, q.backoffMax = base, max }
}

// StandaloneQueue is the standalone deployment mode's Queue implementation: an
// in-process worker pool backed by a SQLite-persisted task table (survives a
// process restart, per docs/internal/07-platform-services.md — task loss
// matters more than a briefly miscounted quota). See AGENTS.md for the full
// design and its documented known limitations.
//
// StandaloneQueue is returned as its own concrete exported type, not the
// narrower Queue interface (compare pkgcore.NewMemoryKVStore, which
// returns KVStore): a caller needs RegisterHandler, Start and Close to
// actually configure and run it, and none of those belong on Queue's
// portable surface — the distributed deployment mode's Redis/asynq-backed
// implementation is expected to need a different setup shape of its own.
// Code that only needs the portable surface should still depend on the Queue interface,
// not this type; see the compile-time assertion below.
type StandaloneQueue struct {
	db *gorm.DB

	workerCount       int
	tenantConcurrency int
	pollInterval      time.Duration
	defaultTimeout    time.Duration
	backoffBase       time.Duration
	backoffMax        time.Duration

	handlersMu sync.RWMutex
	handlers   map[string]Handler

	tenantMu         sync.Mutex
	runningPerTenant map[pkgcore.TenantID]int

	// jobDuration, jobAttempts and jobDeadLetter back the
	// "jobs.job.duration"/"jobs.job.attempts"/"jobs.job.dead_letter"
	// instruments registerJobMetrics wires from Start. Left at their zero
	// value (nil) until then; worker.go's recordJobMetrics/
	// recordDeadLetter guard against that, mirroring
	// registerQueueDepthGauge's own fail-open contract -- a metrics
	// wiring failure must not prevent the queue itself from running, nor
	// panic a later job execution.
	jobDuration   metric.Float64Histogram
	jobAttempts   metric.Int64Counter
	jobDeadLetter metric.Int64Counter

	stopCh    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	startOnce sync.Once
}

// NewStandaloneQueue returns a StandaloneQueue backed by db, which must come from
// dbkit.Open (directly, or through dbkit/dbtest) — see AGENTS.md for why a
// bare gorm.Open connection is not sufficient (Enqueue's idempotency check
// relies on dbkit.Open's TranslateError:true, and dbkit.Open's
// tenant-scoping plugin, though a no-op for jobRecord specifically, is the
// only sanctioned way to obtain a *gorm.DB anywhere in this codebase).
// NewStandaloneQueue performs no I/O; Start does.
func NewStandaloneQueue(db *gorm.DB, opts ...Option) *StandaloneQueue {
	q := &StandaloneQueue{
		db:                db,
		workerCount:       DefaultWorkerCount,
		tenantConcurrency: DefaultTenantConcurrencyLimit,
		pollInterval:      DefaultPollInterval,
		defaultTimeout:    DefaultTimeout,
		backoffBase:       DefaultBackoffBase,
		backoffMax:        DefaultBackoffMax,
		handlers:          make(map[string]Handler),
		runningPerTenant:  make(map[pkgcore.TenantID]int),
		stopCh:            make(chan struct{}),
	}
	for _, opt := range opts {
		opt(q)
	}
	return q
}

// RegisterHandler adds h to the set this StandaloneQueue dispatches to, keyed by
// h.Type(). Register every Handler before calling Start. RegisterHandler
// remains safe to call after Start too (it is mutex-guarded), but a Job
// already claimed before a late registration will not retroactively use
// it — see ErrHandlerNotRegistered.
func (q *StandaloneQueue) RegisterHandler(h Handler) error {
	q.handlersMu.Lock()
	defer q.handlersMu.Unlock()
	if _, exists := q.handlers[h.Type()]; exists {
		return ErrDuplicateHandlerType.WithParam("type", h.Type())
	}
	q.handlers[h.Type()] = h
	return nil
}

// handler returns the Handler registered for jobType, or nil.
func (q *StandaloneQueue) handler(jobType string) Handler {
	q.handlersMu.RLock()
	defer q.handlersMu.RUnlock()
	return q.handlers[jobType]
}

// Start creates the persistence schema if it does not already exist,
// recovers any Job left StatusRunning by an unclean previous exit back to
// StatusPending, wires the "jobs.queue.depth", "jobs.job.duration",
// "jobs.job.attempts" and "jobs.job.dead_letter" metrics, then launches
// the dispatcher and worker goroutines. It is safe to call only once per
// StandaloneQueue; later calls are a no-op.
func (q *StandaloneQueue) Start(ctx context.Context) error {
	var startErr error
	q.startOnce.Do(func() {
		if err := ensureJobsSchema(ctx, q.db); err != nil {
			startErr = err
			return
		}
		if err := resetInterruptedRecords(ctx, q.db, time.Now()); err != nil {
			startErr = fmt.Errorf("jobs: recover interrupted jobs: %w", err)
			return
		}
		if err := q.registerQueueDepthGauge(); err != nil {
			// A metrics wiring failure must not prevent the queue itself
			// from running -- see this method's own doc comment; only
			// ensureJobsSchema/resetInterruptedRecords failures abort
			// Start.
			obs.FromContext(ctx).Warn("jobs: registering queue depth gauge failed", "error", err)
		}
		if err := q.registerJobMetrics(); err != nil {
			// Same fail-open contract as registerQueueDepthGauge above.
			obs.FromContext(ctx).Warn("jobs: registering job metrics failed", "error", err)
		}

		dispatch := make(chan jobRecord)
		q.wg.Add(1)
		go q.runDispatcher(dispatch)
		for i := 0; i < q.workerCount; i++ {
			q.wg.Add(1)
			go q.runWorker(dispatch)
		}
	})
	return startErr
}

// Close stops the dispatcher and waits for in-flight Jobs to reach their
// own natural completion or timeout, up to ctx's deadline. It does not
// itself cancel an in-flight Handle call: each one runs on a context
// rooted independently of StandaloneQueue's own lifecycle (see jobContext and
// execute), so that closing the queue never abruptly truncates a business
// operation already underway. Close is idempotent and safe to call more
// than once, or without a prior Start.
func (q *StandaloneQueue) Close(ctx context.Context) error {
	q.closeOnce.Do(func() { close(q.stopCh) })

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Enqueue implements Queue.
func (q *StandaloneQueue) Enqueue(ctx context.Context, task Task, opts ...EnqueueOption) (JobID, error) {
	if err := task.validate(); err != nil {
		return "", err
	}

	now := time.Now()
	resolved := resolveEnqueueOptions(now, q.defaultTimeout, opts)
	rec := newRecord(newJobID(), task, resolved, now)

	id, err := insertRecord(ctx, q.db, rec)
	if err != nil {
		return "", err
	}

	// Rebuild the logger's context from task.TenantID -- the Job's own
	// owning tenant -- rather than adding an explicit "tenant_id" kv on
	// top of whatever obs.FromContext(ctx) would already auto-attach from
	// ctx's own ambient tenant. Enqueue is legitimately called from
	// contexts whose ambient tenant differs from task.TenantID (see
	// AGENTS.md's "platform-level scheduler enqueuing one cleanup Task
	// per tenant in a loop" example) or carries none at all; either way,
	// a caller-supplied literal here would either duplicate ctx's own
	// value or silently disagree with it, and it is task.TenantID -- not
	// ctx's ambient tenant -- that this log line must attribute the job
	// to. Deriving from ctx (not context.Background(), unlike worker.go's
	// unrelated jobContext) keeps this Enqueue call's own trace_id/
	// span_id intact.
	obs.FromContext(pkgcore.WithTenant(ctx, task.TenantID)).Info("job enqueued", "job_id", id, "job_type", task.Type)
	return JobID(id), nil
}

// Get implements Queue.
func (q *StandaloneQueue) Get(ctx context.Context, id JobID) (*Job, error) {
	rec, err := findByID(ctx, q.db, id)
	if err != nil {
		return nil, err
	}
	if !callerMayAccess(ctx, pkgcore.TenantID(rec.TenantID)) {
		return nil, ErrJobNotFound
	}
	return toJob(rec), nil
}

// Cancel implements Queue.
func (q *StandaloneQueue) Cancel(ctx context.Context, id JobID) error {
	rec, err := findByID(ctx, q.db, id)
	if err != nil {
		return err
	}
	if !callerMayAccess(ctx, pkgcore.TenantID(rec.TenantID)) {
		return ErrJobNotFound
	}
	return markCancelled(ctx, q.db, string(id), time.Now())
}

// DeadLetterJobs returns every Job currently StatusDeadLetter that ctx may
// access — the tenant in ctx, or every tenant under a system context (see
// callerMayAccess). It is not part of the Queue interface: a convenience
// for operating this one implementation, not a portable contract every
// deployment mode need offer identically.
func (q *StandaloneQueue) DeadLetterJobs(ctx context.Context) ([]*Job, error) {
	recs, err := deadLetterRecords(ctx, q.db)
	if err != nil {
		return nil, err
	}
	result := make([]*Job, 0, len(recs))
	for i := range recs {
		if !callerMayAccess(ctx, pkgcore.TenantID(recs[i].TenantID)) {
			continue
		}
		result = append(result, toJob(&recs[i]))
	}
	return result, nil
}

// callerMayAccess reports whether ctx may see a Job owned by owner: either
// ctx carries owner as its tenant, or ctx carries a system context
// (pkgcore.WithSystemContext) — the same two legitimate paths
// dbkit.Repository[T] and the tenant-scoping plugin recognize elsewhere in
// this codebase (docs/internal/04-data-and-tenancy.md names jobs' own
// system tasks as one of the whitelisted WithSystemContext callers).
func callerMayAccess(ctx context.Context, owner pkgcore.TenantID) bool {
	if _, ok := pkgcore.SystemReasonFromContext(ctx); ok {
		return true
	}
	tenant, ok := pkgcore.TenantFromContext(ctx)
	return ok && tenant == owner
}

// registerQueueDepthGauge wires the "jobs.queue.depth" ObservableGauge
// docs/internal/09-observability.md's must-instrument table names for
// jobs: current backlog size, labeled by job_type and status only —
// deliberately NEVER tenant_id, which root CLAUDE.md's logging discipline
// forbids as a Prometheus label on cardinality grounds. Correlating a
// specific tenant's backlog is a job for structured logs and trace
// attributes instead (see execute's own log lines, which do carry
// tenant_id via obs.FromContext), exactly mirroring
// observability.Middleware's own documented split between metric labels
// and span/log attributes.
func (q *StandaloneQueue) registerQueueDepthGauge() error {
	meter := otel.Meter(instrumentationName)
	_, err := meter.Int64ObservableGauge(
		"jobs.queue.depth",
		metric.WithDescription("Number of jobs waiting to run (pending or retrying), by job type and status."),
		metric.WithUnit("{job}"),
		metric.WithInt64Callback(func(ctx context.Context, o metric.Int64Observer) error {
			counts, err := queueDepthByTypeAndStatus(ctx, q.db)
			if err != nil {
				return err
			}
			for _, c := range counts {
				o.Observe(c.Count, metric.WithAttributes(
					attribute.String("job_type", c.Type),
					attribute.String("status", c.Status),
				))
			}
			return nil
		}),
	)
	return err
}

// registerJobMetrics wires the "jobs.job.duration" Histogram and
// "jobs.job.attempts"/"jobs.job.dead_letter" Counters
// docs/internal/09-observability.md's must-instrument table requires for
// the task-queue domain beyond queue backlog depth: execution-duration
// percentiles ("jobs.job.duration"), and failure rate and retry count
// (both derivable from "jobs.job.attempts", sliced by its "status"
// attribute -- one of StatusSucceeded/StatusRetrying/StatusDeadLetter,
// the exact three outcomes execute (worker.go) can reach) and
// dead-letter count ("jobs.job.dead_letter", a dedicated counter since
// that is the one outcome operators alert on directly). Labeled by
// job_type and status only -- deliberately never tenant_id, for the
// identical cardinality reason registerQueueDepthGauge's own doc comment
// gives. Unlike registerQueueDepthGauge, these three are synchronous
// instruments recorded imperatively at the point each attempt concludes
// (worker.go's recordJobMetrics/recordDeadLetter), not an
// ObservableGauge callback, since "how long did this attempt take" and
// "did this attempt fail" are events, not a value that can be sampled on
// demand from q.db.
func (q *StandaloneQueue) registerJobMetrics() error {
	meter := otel.Meter(instrumentationName)

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

// compile-time check that *StandaloneQueue satisfies Queue.
var _ Queue = (*StandaloneQueue)(nil)
