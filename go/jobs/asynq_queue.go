package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// AsynqQueue is the distributed deployment mode's Queue implementation:
// backed by Redis via github.com/hibiken/asynq, per docs/internal/07-
// platform-services.md's jobs section ("the distributed deployment mode
// = Redis (hibiken/asynq, mature and ships its own retry/delay/
// scheduling/Web UI, not worth reimplementing ourselves)"). It
// implements the exact same Queue interface StandaloneQueue does
// (queue.go) -- see AGENTS.md's "Distributed: AsynqQueue" section for
// the full mapping from every Task/Job/EnqueueOption concept onto asynq's
// own client/server/inspector primitives, including the two places (per-
// tenant concurrency, progress reporting) where asynq's own primitives
// needed a thin layer on top rather than a direct configuration.
//
// Like StandaloneQueue, AsynqQueue is returned as its own concrete exported type,
// not the narrower Queue interface, for the same reason: RegisterHandler/
// Start/Close are not part of Queue's portable surface (queue.go's own doc
// comment reserves exactly this freedom for the distributed deployment
// mode's own Redis/asynq-backed implementation). Code that only needs
// the portable surface should still depend on Queue, not this type; see
// the compile-time
// assertion at the bottom of this file.
type AsynqQueue struct {
	client    *asynq.Client
	inspector *asynq.Inspector
	server    *asynq.Server
	rdb       redis.UniversalClient // shared with client/inspector/server; also backs our own cancellation markers (asynq_store.go's cancelMarkerKey).

	concurrency              int
	tenantConcurrency        int
	defaultTimeout           time.Duration
	completedRetention       time.Duration
	cancelledRetention       time.Duration
	throttleRetryDelay       time.Duration
	queueWeights             map[string]int
	businessRetryDelayFunc   asynq.RetryDelayFunc
	taskCheckInterval        time.Duration
	delayedTaskCheckInterval time.Duration

	handlersMu sync.RWMutex
	handlers   map[string]Handler

	tenantMu         sync.Mutex
	runningPerTenant map[pkgcore.TenantID]int

	startOnce    sync.Once
	closeRDBOnce sync.Once
}

// Defaults for AsynqQueue's construction Options, applied when the
// corresponding With* option is not given. Named package-level constants
// per the backend coding standard's configuration rule (§10), mirroring
// StandaloneQueue's own Default* constants immediately above in spirit (kept in
// this file, not standalone_queue.go, since they are specific to the
// distributed deployment mode).
const (
	// DefaultAsynqTenantConcurrencyLimit matches StandaloneQueue's own
	// DefaultTenantConcurrencyLimit -- see AGENTS.md for why the SAME
	// default is used despite the two deployment modes enforcing it at different
	// points in the pipeline.
	DefaultAsynqTenantConcurrencyLimit = 2

	// DefaultAsynqCompletedRetention bounds how long a succeeded Job
	// remains visible to Get() after it completes. Unlike StandaloneQueue, whose
	// SQLite row for a succeeded Job is never deleted, asynq deletes a
	// completed task immediately unless told to retain it (asynq.
	// Retention) -- AsynqQueue always passes this value on every Enqueue
	// call; see AGENTS.md's "Get() after a Job succeeds" section for why
	// omitting it would silently break Get()'s contract for the success
	// path.
	DefaultAsynqCompletedRetention = 24 * time.Hour

	// DefaultAsynqCancelledRetention bounds how long a cancellation
	// marker (asynq_store.go's cancelMarkerKey) survives in Redis, and so
	// how long Get() keeps reporting StatusCancelled for a cancelled Job
	// once its underlying asynq task record has itself expired or been
	// evicted. Unlike StandaloneQueue's SQLite row, this is not forever -- see
	// AGENTS.md's Known limitations.
	DefaultAsynqCancelledRetention = 30 * 24 * time.Hour

	// DefaultAsynqThrottleRetryDelay is the base of the short, jittered
	// delay used when a Job is bounced back for redelivery because its
	// tenant is at its concurrency limit -- deliberately much shorter than
	// asynq.DefaultRetryDelayFunc's exponential business-failure backoff.
	// See errTenantAtCapacity's own doc comment (asynq_worker.go).
	DefaultAsynqThrottleRetryDelay = 200 * time.Millisecond
)

// defaultAsynqQueueWeights is the Config.Queues AsynqQueue uses unless
// overridden by WithAsynqQueueWeights -- the exact 6/3/1 ratio server.go's
// own Config.Queues doc comment uses as its illustrative example, applied
// here to our fixed critical/default/low tiers (asynq_store.go).
var defaultAsynqQueueWeights = map[string]int{
	asynqQueueCritical: 6,
	asynqQueueDefault:  3,
	asynqQueueLow:      1,
}

// AsynqOption configures an AsynqQueue at construction time, mirroring
// StandaloneQueue's own Option -- a distinct type (not a reuse of StandaloneQueue's
// Option) since the two constructors configure structurally different
// fields, but the same functional-options convention throughout this
// codebase (see observability.Option, StandaloneQueue's Option above).
type AsynqOption func(*AsynqQueue)

// WithAsynqConcurrency sets asynq.Config.Concurrency: the maximum number of
// Jobs AsynqQueue executes concurrently across all tenants and queues
// combined, the direct analog of StandaloneQueue's WithWorkerCount. Zero or
// negative (the default here) leaves asynq.Config.Concurrency at its own
// zero value, which asynq.NewServer resolves to runtime.NumCPU().
func WithAsynqConcurrency(n int) AsynqOption {
	return func(q *AsynqQueue) { q.concurrency = n }
}

// WithAsynqTenantConcurrencyLimit caps how many Jobs belonging to any one
// tenant may be running at once, the direct analog of StandaloneQueue's
// WithTenantConcurrencyLimit -- see AGENTS.md for how this is enforced
// differently (a bounce-and-redeliver inside processTask, not a pre-dequeue
// skip) given what asynq itself offers. Defaults to
// DefaultAsynqTenantConcurrencyLimit.
func WithAsynqTenantConcurrencyLimit(n int) AsynqOption {
	return func(q *AsynqQueue) { q.tenantConcurrency = n }
}

// WithAsynqQueueWeights overrides the relative weight asynq gives each of
// the three fixed priority queues (asynq_store.go's queueForPriority) when
// choosing which to service next -- passed straight through to asynq.
// Config.Queues, so asynq's own documented behavior for a zero or negative
// weight (that tier is never serviced) applies unmodified. Defaults to 6/3/1
// (critical/default/low), server.go's own Config.Queues example ratio.
func WithAsynqQueueWeights(critical, normal, low int) AsynqOption {
	return func(q *AsynqQueue) {
		q.queueWeights = map[string]int{
			asynqQueueCritical: critical,
			asynqQueueDefault:  normal,
			asynqQueueLow:      low,
		}
	}
}

// WithAsynqJobTimeout sets the per-attempt timeout applied to an Enqueue
// call that does not use WithTimeout, the direct analog of StandaloneQueue's
// WithJobTimeout. Defaults to DefaultTimeout (queue.go).
func WithAsynqJobTimeout(d time.Duration) AsynqOption {
	return func(q *AsynqQueue) { q.defaultTimeout = d }
}

// WithAsynqCompletedRetention overrides DefaultAsynqCompletedRetention.
func WithAsynqCompletedRetention(d time.Duration) AsynqOption {
	return func(q *AsynqQueue) { q.completedRetention = d }
}

// WithAsynqCancelledRetention overrides DefaultAsynqCancelledRetention.
func WithAsynqCancelledRetention(d time.Duration) AsynqOption {
	return func(q *AsynqQueue) { q.cancelledRetention = d }
}

// WithAsynqThrottleRetryDelay overrides DefaultAsynqThrottleRetryDelay.
func WithAsynqThrottleRetryDelay(d time.Duration) AsynqOption {
	return func(q *AsynqQueue) { q.throttleRetryDelay = d }
}

// WithAsynqRetryDelayFunc overrides the backoff formula applied to a
// genuine Handler failure (never to a tenant-concurrency bounce, which
// always uses WithAsynqThrottleRetryDelay's short delay regardless -- see
// asynq_worker.go's retryDelay). Defaults to asynq.DefaultRetryDelayFunc,
// letting asynq's own exponential-backoff formula stand in for StandaloneQueue's
// hand-rolled backoffDelay rather than reimplementing one; WithBackoff
// (StandaloneQueue's equivalent knob) configures a formula, not a pluggable
// function, only because StandaloneQueue rolls its own -- this is the same
// tuning knob shifted to asynq's own extension point.
func WithAsynqRetryDelayFunc(fn asynq.RetryDelayFunc) AsynqOption {
	return func(q *AsynqQueue) { q.businessRetryDelayFunc = fn }
}

// WithAsynqTaskCheckInterval sets asynq.Config.TaskCheckInterval: how often
// the processor polls a queue it just found empty. Left at zero (the
// default here), asynq.NewServer applies its own default of 1 second.
// Lowering it (integration_test/ uses a few tens of milliseconds) trades
// Redis polling load for faster pickup -- the same trade-off StandaloneQueue's
// WithPollInterval documents for its own dispatcher.
func WithAsynqTaskCheckInterval(d time.Duration) AsynqOption {
	return func(q *AsynqQueue) { q.taskCheckInterval = d }
}

// WithAsynqDelayedTaskCheckInterval sets asynq.Config.
// DelayedTaskCheckInterval: how often asynq's forwarder checks scheduled
// and retry tasks for ones now ready to run and moves them to pending.
// Left at zero (the default here), asynq.NewServer applies its own default
// of 5 seconds -- long enough that a test asserting on WithDelay/
// WithScheduledAt or on retry timing should lower this, exactly as
// integration_test/ does, the same way it lowers WithAsynqThrottleRetryDelay
// and StandaloneQueue's own tests lower WithPollInterval/WithBackoff.
func WithAsynqDelayedTaskCheckInterval(d time.Duration) AsynqOption {
	return func(q *AsynqQueue) { q.delayedTaskCheckInterval = d }
}

// NewAsynqQueue returns an AsynqQueue connected via redisOpt (typically
// asynq.RedisClientOpt for a single Redis instance; asynq also defines
// RedisFailoverClientOpt and RedisClusterClientOpt for sentinel/cluster
// deployments, both accepted unmodified since RedisConnOpt is asynq's own
// interface). Like NewStandaloneQueue, NewAsynqQueue performs no I/O of its own:
// the underlying redis.UniversalClient (go-redis) dials lazily on first
// command, exactly as asynq.NewClient/NewServer/NewInspector already rely
// on when constructed this same way.
//
// The SAME redis.UniversalClient backs the asynq.Client, asynq.Inspector
// and asynq.Server this constructs (via *FromRedisClient, not separate
// RedisConnOpt.MakeRedisClient() calls) plus AsynqQueue's own cancellation-
// marker bookkeeping -- one shared connection pool, not four. Close (below)
// is therefore the only place any of them are closed; per asynq's own
// documented "shared connection" contract, Client.Close/Inspector.Close
// would simply error if called, so this package never calls them.
func NewAsynqQueue(redisOpt asynq.RedisConnOpt, opts ...AsynqOption) *AsynqQueue {
	q := &AsynqQueue{
		tenantConcurrency:      DefaultAsynqTenantConcurrencyLimit,
		defaultTimeout:         DefaultTimeout,
		completedRetention:     DefaultAsynqCompletedRetention,
		cancelledRetention:     DefaultAsynqCancelledRetention,
		throttleRetryDelay:     DefaultAsynqThrottleRetryDelay,
		queueWeights:           defaultAsynqQueueWeights,
		businessRetryDelayFunc: asynq.DefaultRetryDelayFunc,
		handlers:               make(map[string]Handler),
		runningPerTenant:       make(map[pkgcore.TenantID]int),
	}
	for _, opt := range opts {
		opt(q)
	}

	// Same panic-on-unsupported-RedisConnOpt-type this exact type
	// assertion triggers inside asynq.NewClient/NewServer/NewInspector --
	// reproduced here (rather than delegated to one of them) only because
	// constructing the shared client ourselves is what lets client/
	// inspector/server/our own bookkeeping share one pool.
	rdb, ok := redisOpt.MakeRedisClient().(redis.UniversalClient)
	if !ok {
		panic(fmt.Sprintf("jobs: unsupported asynq.RedisConnOpt type %T", redisOpt))
	}
	q.rdb = rdb
	q.client = asynq.NewClientFromRedisClient(rdb)
	q.inspector = asynq.NewInspectorFromRedisClient(rdb)
	q.server = asynq.NewServerFromRedisClient(rdb, asynq.Config{
		Concurrency:              q.concurrency,
		Queues:                   q.queueWeights,
		RetryDelayFunc:           q.retryDelay,
		IsFailure:                isFailure,
		ErrorHandler:             asynq.ErrorHandlerFunc(q.handleError),
		TaskCheckInterval:        q.taskCheckInterval,
		DelayedTaskCheckInterval: q.delayedTaskCheckInterval,
	})
	return q
}

// RegisterHandler adds h to the set this AsynqQueue dispatches to, keyed by
// h.Type(). Identical contract to StandaloneQueue.RegisterHandler, including
// ErrDuplicateHandlerType (the same sentinel, reused rather than
// redeclared) and remaining safe to call after Start.
func (q *AsynqQueue) RegisterHandler(h Handler) error {
	q.handlersMu.Lock()
	defer q.handlersMu.Unlock()
	if _, exists := q.handlers[h.Type()]; exists {
		return ErrDuplicateHandlerType.WithParam("type", h.Type())
	}
	q.handlers[h.Type()] = h
	return nil
}

// handler returns the Handler registered for jobType, or nil.
func (q *AsynqQueue) handler(jobType string) Handler {
	q.handlersMu.RLock()
	defer q.handlersMu.RUnlock()
	return q.handlers[jobType]
}

// Start wires the jobs.queue.depth metric (best-effort, exactly like
// StandaloneQueue.Start -- a registration failure is logged and does not prevent
// the server from starting) and launches asynq's own background processor
// goroutines via asynq.Server.Start. Like Server.Start (and unlike Server.
// Run), this returns immediately once processing has launched; it does not
// block waiting for shutdown. Safe to call only once per AsynqQueue -- a
// second call is a no-op, matching StandaloneQueue.Start's own contract, though
// enforced here by asynq.Server.Start's own "already running" error rather
// than a second sync.Once layer, since NewServer itself has no such guard.
func (q *AsynqQueue) Start(ctx context.Context) error {
	var startErr error
	q.startOnce.Do(func() {
		if err := q.registerQueueDepthGauge(); err != nil {
			obs.FromContext(ctx).Warn("jobs: registering queue depth gauge failed", "error", err)
		}
		startErr = q.server.Start(asynq.HandlerFunc(q.processTask))
	})
	return startErr
}

// Close gracefully shuts down asynq's Server -- waiting for in-flight
// attempts to finish naturally, up to ctx's deadline -- then closes the
// shared redis.UniversalClient. See asynq.Server.Shutdown's own doc
// comment for the one behavioral nuance from StandaloneQueue.Close worth calling
// out: Shutdown's own internal wait is bounded by asynq.Config.
// ShutdownTimeout (default 8s, a construction-time setting, not a per-call
// parameter), not directly by ctx; ctx here bounds how long THIS call waits
// for that to finish, exactly like StandaloneQueue.Close, but does not itself
// cancel an in-flight Handle call either way -- see AGENTS.md's
// "Close/Shutdown" section for the full comparison. Idempotent and safe to
// call more than once, or without a prior Start, exactly like StandaloneQueue.
// Close: asynq.Server.Shutdown is itself safe to invoke repeatedly and
// before any Start (server.go's own state-machine check), and this
// package's own addition -- closing the shared redis client -- is guarded
// separately so a second call cannot double-close it.
func (q *AsynqQueue) Close(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		q.server.Shutdown()
		close(done)
	}()

	select {
	case <-done:
		q.closeRDBOnce.Do(func() { _ = q.rdb.Close() })
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Enqueue implements Queue.
func (q *AsynqQueue) Enqueue(ctx context.Context, task Task, opts ...EnqueueOption) (JobID, error) {
	if err := task.validate(); err != nil {
		return "", err
	}

	now := time.Now()
	resolved := resolveEnqueueOptions(now, q.defaultTimeout, opts)

	taskID := newJobID()
	if task.IdempotencyKey != "" {
		// Deterministic per (tenant, idempotency key) -- see AGENTS.md's
		// idempotency section: asynq's own TaskID-uniqueness check
		// (below) is what makes a second Enqueue for the same pair return
		// the first call's JobID rather than creating a second task,
		// mapping our idempotency contract onto a primitive asynq already
		// has rather than a bespoke check-then-insert.
		taskID = "idem:" + string(task.TenantID) + ":" + task.IdempotencyKey
	}

	asynqTask := asynq.NewTaskWithHeaders(task.Type, task.Payload, buildTaskHeaders(task, now))
	info, err := q.client.EnqueueContext(ctx, asynqTask,
		asynq.TaskID(taskID),
		asynq.Queue(queueForPriority(resolved.priority)),
		asynq.MaxRetry(resolved.maxRetries),
		asynq.Timeout(resolved.timeout),
		// Always set -- see DefaultAsynqCompletedRetention's own doc
		// comment for why omitting this would break Get() for a succeeded
		// Job almost immediately.
		asynq.Retention(q.completedRetention),
		asynq.ProcessAt(resolved.scheduledAt),
	)
	if err != nil {
		if errors.Is(err, asynq.ErrTaskIDConflict) {
			existing, ferr := q.findTaskInfo(taskID)
			if ferr != nil {
				return "", fmt.Errorf("jobs: look up existing job for idempotency key after conflict: %w", ferr)
			}
			return JobID(existing.ID), nil
		}
		return "", fmt.Errorf("jobs: enqueue job: %w", err)
	}

	// See StandaloneQueue.Enqueue's identical fix for the full rationale: derive
	// the logger's tenant from task.TenantID -- the Job's own owner --
	// rather than layering an explicit "tenant_id" kv on top of whatever
	// obs.FromContext(ctx) already auto-attaches from ctx's own ambient
	// tenant, which AGENTS.md documents as legitimately different (or
	// absent) for both AsynqQueue.Enqueue and StandaloneQueue.Enqueue alike.
	obs.FromContext(pkgcore.WithTenant(ctx, task.TenantID)).Info("job enqueued", "job_id", info.ID, "job_type", task.Type)
	return JobID(info.ID), nil
}

// Get implements Queue.
func (q *AsynqQueue) Get(ctx context.Context, id JobID) (*Job, error) {
	info, err := q.findTaskInfo(string(id))
	if err != nil {
		return nil, err
	}
	tenantID := pkgcore.TenantID(info.Headers[headerTenantID])
	if !callerMayAccess(ctx, tenantID) {
		return nil, ErrJobNotFound
	}

	cancelledAt, cerr := q.readCancelMarker(ctx, string(id))
	if cerr != nil {
		obs.FromContext(ctx).Warn("jobs: reading cancellation marker failed", "job_id", string(id), "error", cerr)
	}
	return asynqJobFromTaskInfo(info, cancelledAt), nil
}

// Cancel implements Queue. See AGENTS.md's "Cancel" section: the
// cancellation marker (asynq_store.go) is the ONLY authoritative, permanent
// effect -- Get() reports StatusCancelled from it unconditionally, exactly
// mirroring StandaloneQueue's own markCancelled + completeSucceeded/
// completeRetrying/completeDeadLetter no-op-when-not-running guard.
//
// Cancel deliberately does NOT call Inspector.DeleteTask for a not-yet-
// running Job, even though that looks like the obvious way to stop asynq
// from ever processing it: DeleteTask removes the task's entire TaskInfo
// record, and Get() would then have nothing left to combine the
// cancellation marker with -- id would look exactly like an id that never
// existed, i.e. ErrJobNotFound, breaking Get()'s contract for a cancelled
// Job (this was a genuine bug in an earlier version of this method, caught
// by integration_test/cancel_test.go's TestRedisQueue_Cancel_PendingJobNeverRuns
// against a real Redis -- a plain unit test with a hand-built TaskInfo
// would not have caught it, since it never exercises a task actually being
// deleted). Instead, the Job's asynq record is left alone, and
// processTask (asynq_worker.go) itself checks for exactly this same marker
// as the very first thing it does whenever this task is next dequeued --
// returning nil without ever calling Handle if a marker exists, which
// asynq records as an ordinary successful completion (retained for
// completedRetention, same as a real success) rather than a retry or an
// archive. The one accepted cosmetic cost: asynq's own internal
// processed/failed counters (visible via asynqmon or Inspector.
// GetQueueInfo) count a cancelled-before-dispatch Job as "processed", not
// as cancelled -- asynq has no third bucket for that.
//
// For an already-Active Job, Cancel also best-effort signals Inspector.
// CancelProcessing -- strictly better than, and not in tension with,
// StandaloneQueue's own documented "does not preempt" limitation (Queue.Cancel's
// doc comment only ever promises a running Job "is allowed to" keep
// executing, never that it is guaranteed to). Its failure is logged, never
// returned, since the marker alone already satisfies the contract either
// way.
func (q *AsynqQueue) Cancel(ctx context.Context, id JobID) error {
	info, err := q.findTaskInfo(string(id))
	if err != nil {
		return err
	}
	tenantID := pkgcore.TenantID(info.Headers[headerTenantID])
	if !callerMayAccess(ctx, tenantID) {
		return ErrJobNotFound
	}

	if already, _ := q.readCancelMarker(ctx, string(id)); already != nil {
		return nil // idempotent: already cancelled.
	}
	if info.State == asynq.TaskStateCompleted || info.State == asynq.TaskStateArchived {
		return nil // idempotent: already otherwise terminal.
	}

	if info.State == asynq.TaskStateActive {
		if cerr := q.inspector.CancelProcessing(string(id)); cerr != nil {
			obs.FromContext(ctx).Warn("jobs: best-effort CancelProcessing signal failed", "job_id", string(id), "error", cerr)
		}
	}

	if merr := q.writeCancelMarker(ctx, string(id)); merr != nil {
		return fmt.Errorf("jobs: record cancellation: %w", merr)
	}
	return nil
}

// DeadLetterJobs returns every archived (dead-lettered) Job ctx may access,
// across all three priority queues -- the asynq-backed analog of
// StandaloneQueue.DeadLetterJobs, mapping onto asynq's own archived-task
// mechanism (Inspector.ListArchivedTasks) rather than a parallel one; see
// AGENTS.md's dead-letter mapping section. Not part of the Queue interface,
// exactly like StandaloneQueue's version. Fully drains each queue's archive
// (paginating internally) rather than truncating at one page, though it
// still exposes no pagination of its own to the caller -- the same "no
// pagination" shape StandaloneQueue.DeadLetterJobs documents as a known
// limitation.
func (q *AsynqQueue) DeadLetterJobs(ctx context.Context) ([]*Job, error) {
	const pageSize = 100
	var result []*Job
	for _, queueName := range asynqPriorityQueues {
		for page := 1; ; page++ {
			infos, err := q.inspector.ListArchivedTasks(queueName, asynq.PageSize(pageSize), asynq.Page(page))
			if err != nil {
				if errors.Is(err, asynq.ErrQueueNotFound) {
					break
				}
				return nil, fmt.Errorf("jobs: list archived jobs in queue %s: %w", queueName, err)
			}
			for _, info := range infos {
				tenantID := pkgcore.TenantID(info.Headers[headerTenantID])
				if !callerMayAccess(ctx, tenantID) {
					continue
				}
				cancelledAt, _ := q.readCancelMarker(ctx, info.ID)
				result = append(result, asynqJobFromTaskInfo(info, cancelledAt))
			}
			if len(infos) < pageSize {
				break
			}
		}
	}
	return result, nil
}

// findTaskInfo locates id by probing each of asynqPriorityQueues in turn --
// asynq.Inspector.GetTaskInfo requires a queue name and offers no
// search-by-id-alone primitive, so this bounded, three-probe fan-out (never
// more, since AsynqQueue only ever enqueues into these three) is how every
// Get/Cancel/idempotency-conflict lookup in this file finds a Job without
// the caller having to remember which priority it used.
func (q *AsynqQueue) findTaskInfo(id string) (*asynq.TaskInfo, error) {
	for _, queueName := range asynqPriorityQueues {
		info, err := q.inspector.GetTaskInfo(queueName, id)
		switch {
		case err == nil:
			return info, nil
		case errors.Is(err, asynq.ErrTaskNotFound), errors.Is(err, asynq.ErrQueueNotFound):
			continue
		default:
			return nil, fmt.Errorf("jobs: look up job: %w", err)
		}
	}
	return nil, ErrJobNotFound
}

// cancelMarkerKey namespaces AsynqQueue's own cancellation-marker keys
// clearly apart from every key asynq itself owns (all under "asynq:...",
// per internal/base) to eliminate any collision risk on the shared
// redis.UniversalClient.
func cancelMarkerKey(id string) string { return "asynqjobs:cancelled:" + id }

// writeCancelMarker records that id has been cancelled, expiring after
// q.cancelledRetention -- see DefaultAsynqCancelledRetention's own doc
// comment for why this is bounded rather than permanent.
func (q *AsynqQueue) writeCancelMarker(ctx context.Context, id string) error {
	return q.rdb.Set(ctx, cancelMarkerKey(id), time.Now().UTC().Format(time.RFC3339Nano), q.cancelledRetention).Err()
}

// readCancelMarker reports id's cancellation time, or nil if it was never
// cancelled (or its marker has since expired).
func (q *AsynqQueue) readCancelMarker(ctx context.Context, id string) (*time.Time, error) {
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
// StandaloneQueue.registerQueueDepthGauge does (docs/internal/09-observability.md's
// must-instrument table), reusing this package's shared instrumentationName
// constant (standalone_queue.go). The label set differs from StandaloneQueue's, and
// deliberately so -- see AGENTS.md's observability section: asynq's
// Inspector.GetQueueInfo reports backlog per QUEUE (one of our three
// priority tiers), not per job TYPE the way StandaloneQueue's own SQL GROUP BY
// can, so this gauge is labeled (queue, status) instead of (job_type,
// status). Both label sets are low-cardinality and neither ever includes
// tenant_id, per root CLAUDE.md's Prometheus-cardinality rule.
func (q *AsynqQueue) registerQueueDepthGauge() error {
	meter := otel.Meter(instrumentationName)
	_, err := meter.Int64ObservableGauge(
		"jobs.queue.depth",
		metric.WithDescription("Number of jobs waiting to run (pending or retrying), by queue and status."),
		metric.WithUnit("{job}"),
		metric.WithInt64Callback(func(ctx context.Context, o metric.Int64Observer) error {
			for _, queueName := range asynqPriorityQueues {
				info, err := q.inspector.GetQueueInfo(queueName)
				if err != nil {
					if errors.Is(err, asynq.ErrQueueNotFound) {
						continue
					}
					return err
				}
				o.Observe(int64(info.Pending), metric.WithAttributes(
					attribute.String("queue", queueName),
					attribute.String("status", string(StatusPending)),
				))
				o.Observe(int64(info.Retry), metric.WithAttributes(
					attribute.String("queue", queueName),
					attribute.String("status", string(StatusRetrying)),
				))
			}
			return nil
		}),
	)
	return err
}

// compile-time check that *AsynqQueue satisfies Queue, mirroring
// StandaloneQueue's identical assertion.
var _ Queue = (*AsynqQueue)(nil)
