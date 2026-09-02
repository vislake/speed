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

// instrumentationName identifies DemoQueue's own tracer/meter, mirroring
// observability.Middleware's identical use of its own package path for the
// same purpose.
const instrumentationName = "github.com/vislake/speed/go/jobs"

// Defaults for DemoQueue's construction Options, applied when the
// corresponding With* option is not given. Named package-level constants
// per the backend coding standard §10. Unlike dbkit's connection-pool
// limits (deliberately fixed, "so there is exactly one place to
// reconsider them"), worker-pool sizing and per-tenant concurrency really
// are deployment-dependent — a small demo box and a beefier one
// legitimately want different values — which is why this package exposes
// them as Options instead of hard-coding them the way dbkit.Options does
// for its own pool limits.
const (
	DefaultWorkerCount            = 4
	DefaultTenantConcurrencyLimit = 2
	DefaultPollInterval           = 200 * time.Millisecond
	DefaultBackoffBase            = 1 * time.Second
	DefaultBackoffMax             = 5 * time.Minute
)

// ErrDuplicateHandlerType is returned by RegisterHandler when a Handler
// for the same Type is already registered on this DemoQueue.
var ErrDuplicateHandlerType = apperr.Invalid("jobs.duplicate_handler_type")

// Option configures a DemoQueue at construction time.
type Option func(*DemoQueue)

// WithWorkerCount sets how many Jobs DemoQueue executes concurrently
// across all tenants combined. Defaults to DefaultWorkerCount.
func WithWorkerCount(n int) Option {
	return func(q *DemoQueue) { q.workerCount = n }
}

// WithTenantConcurrencyLimit caps how many Jobs belonging to any one
// tenant may be StatusRunning at once, so one tenant's backlog cannot
// starve every worker (docs/internal/07-platform-services.md). Defaults to
// DefaultTenantConcurrencyLimit.
func WithTenantConcurrencyLimit(n int) Option {
	return func(q *DemoQueue) { q.tenantConcurrency = n }
}

// WithPollInterval sets how often the dispatcher checks for newly-eligible
// Jobs. Defaults to DefaultPollInterval.
func WithPollInterval(d time.Duration) Option {
	return func(q *DemoQueue) { q.pollInterval = d }
}

// WithJobTimeout sets the per-attempt timeout applied to an Enqueue call
// that does not use WithTimeout. Defaults to DefaultTimeout.
func WithJobTimeout(d time.Duration) Option {
	return func(q *DemoQueue) { q.defaultTimeout = d }
}

// WithBackoff sets the exponential retry backoff's base and cap. Defaults
// to DefaultBackoffBase and DefaultBackoffMax.
func WithBackoff(base, max time.Duration) Option {
	return func(q *DemoQueue) { q.backoffBase, q.backoffMax = base, max }
}

// DemoQueue is the demo-profile Queue implementation: an in-process worker
// pool backed by a SQLite-persisted task table (survives a process
// restart, per docs/internal/07-platform-services.md — task loss matters
// more than a briefly miscounted quota). See AGENTS.md for the full design
// and its documented known limitations.
//
// DemoQueue is returned as its own concrete exported type, not the
// narrower Queue interface (compare pkgcore.NewMemoryKVStore, which
// returns KVStore): a caller needs RegisterHandler, Start and Close to
// actually configure and run it, and none of those belong on Queue's
// portable surface — a production, Redis/asynq-backed implementation is
// expected to need a different setup shape of its own. Code that only
// needs the portable surface should still depend on the Queue interface,
// not this type; see the compile-time assertion below.
type DemoQueue struct {
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

	stopCh    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	startOnce sync.Once
}

// NewDemoQueue returns a DemoQueue backed by db, which must come from
// dbkit.Open (directly, or through dbkit/dbtest) — see AGENTS.md for why a
// bare gorm.Open connection is not sufficient (Enqueue's idempotency check
// relies on dbkit.Open's TranslateError:true, and dbkit.Open's
// tenant-scoping plugin, though a no-op for jobRecord specifically, is the
// only sanctioned way to obtain a *gorm.DB anywhere in this codebase).
// NewDemoQueue performs no I/O; Start does.
func NewDemoQueue(db *gorm.DB, opts ...Option) *DemoQueue {
	q := &DemoQueue{
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

// RegisterHandler adds h to the set this DemoQueue dispatches to, keyed by
// h.Type(). Register every Handler before calling Start. RegisterHandler
// remains safe to call after Start too (it is mutex-guarded), but a Job
// already claimed before a late registration will not retroactively use
// it — see ErrHandlerNotRegistered.
func (q *DemoQueue) RegisterHandler(h Handler) error {
	q.handlersMu.Lock()
	defer q.handlersMu.Unlock()
	if _, exists := q.handlers[h.Type()]; exists {
		return ErrDuplicateHandlerType.WithParam("type", h.Type())
	}
	q.handlers[h.Type()] = h
	return nil
}

// handler returns the Handler registered for jobType, or nil.
func (q *DemoQueue) handler(jobType string) Handler {
	q.handlersMu.RLock()
	defer q.handlersMu.RUnlock()
	return q.handlers[jobType]
}

// Start creates the persistence schema if it does not already exist,
// recovers any Job left StatusRunning by an unclean previous exit back to
// StatusPending, wires the "jobs.queue.depth" metric, then launches the
// dispatcher and worker goroutines. It is safe to call only once per
// DemoQueue; later calls are a no-op.
func (q *DemoQueue) Start(ctx context.Context) error {
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
// rooted independently of DemoQueue's own lifecycle (see jobContext and
// execute), so that closing the queue never abruptly truncates a business
// operation already underway. Close is idempotent and safe to call more
// than once, or without a prior Start.
func (q *DemoQueue) Close(ctx context.Context) error {
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
func (q *DemoQueue) Enqueue(ctx context.Context, task Task, opts ...EnqueueOption) (JobID, error) {
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

	obs.FromContext(ctx).Info("job enqueued", "job_id", id, "job_type", task.Type, "tenant_id", string(task.TenantID))
	return JobID(id), nil
}

// Get implements Queue.
func (q *DemoQueue) Get(ctx context.Context, id JobID) (*Job, error) {
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
func (q *DemoQueue) Cancel(ctx context.Context, id JobID) error {
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
// profile need offer identically.
func (q *DemoQueue) DeadLetterJobs(ctx context.Context) ([]*Job, error) {
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
func (q *DemoQueue) registerQueueDepthGauge() error {
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

// compile-time check that *DemoQueue satisfies Queue.
var _ Queue = (*DemoQueue)(nil)
