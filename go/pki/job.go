package pki

import (
	"context"
	"errors"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// taskTypeExpiryScan names the jobs queue task EnqueueExpiryScan schedules
// and expiryScanHandler claims. One run walks every purpose's signing keys
// (lifecycle.go's ScanExpiry) -- there is no per-tenant shape to this task
// the way storage.taskTypeExpirySweep has one, because pki_signing_keys is
// platform data (model.go), not tenant data: a signing key belongs to the
// whole deployment, never to one tenant.
const taskTypeExpiryScan = "pki.expiry_scan"

// DefaultExpiryScanWindow is the period one expiry-scan idempotency key
// covers when the host does not override it (WithExpiryScanWindow): a scan
// is enqueued under the key of the DefaultExpiryScanWindow window
// (expiryScanWindowStart) its enqueue falls in, so the same-window
// duplicates a multi-replica scheduler produces -- every replica's tick of
// one schedule instant -- collapse into one job, while an enqueue in a
// later window is a NEW job and the scan runs again.
//
// The window is what makes the scan periodic at all on queues whose
// idempotency is unconditional: jobs' idempotency resolves one key forever
// on StandaloneQueue (go/jobs), so a keyless task (every tick its own job)
// lets N replicas at a 1-minute cadence fire N concurrent scans every
// minute -- the redundant-work and lost-race-error pattern the window
// exists to remove -- while a key WITHOUT a window would give exactly one
// scan per database file. A windowed key collapses the former without
// causing the latter: at most one scan per window runs, however many
// replicas tick, and a scan that dead-letters poisons only its own window.
//
// The window must be significantly larger than the host's scheduler
// interval, or every tick lands in a fresh window and the dedup is void.
// The host that schedules this task (examples/reference-app's periodic
// scheduler) ticks once a minute, leaving a 60:1 window-to-interval ratio
// against this one-hour default; a host whose own interval approaches the
// window must widen it through WithExpiryScanWindow, never shrink the
// interval toward it.
const DefaultExpiryScanWindow = time.Hour

// expiryScanWindowStart is the expiry-scan window the enqueue at now
// belongs to -- the absolute-clock boundary now.Truncate(window) lands in,
// the identical shape storage's expirySweepWindowStart and compliance's
// retentionSweepWindowStart use: every replica agrees on the boundary
// regardless of its own location, since Truncate is on the absolute clock,
// never a timezone-local calendar cut. Two replicas enqueuing within the
// same window share one key (and one job); a tick in a later window gets
// its own.
func expiryScanWindowStart(now time.Time, window time.Duration) time.Time {
	return now.Truncate(window)
}

// expiryScanKeyPrefix is the prefix of every expiry-scan idempotency key.
// It is a named constant because two derivations must agree on it byte for
// byte: expiryScanIdempotencyKey below, and the declaration
// (Service.expiryScanSchedule) a jobs.Scheduler composes keys from with
// its own window derivation -- one window must resolve one key through
// both paths.
const expiryScanKeyPrefix = "pki.expiry_scan:"

// expiryScanIdempotencyKey derives the jobs idempotency key of one
// expiry-scan window, per the rule that an idempotency key derives from
// the business operation, never random: the operation one key names is
// "the scan of windowStart", not "some scan or other" -- a periodic task's
// identity inherently includes WHICH period it is for (the same reasoning
// storage's expirySweepIdempotencyKey and compliance's
// retentionSweepIdempotencyKey document for their own sweeps). windowStart
// is the DefaultExpiryScanWindow window start the enqueue belongs to
// (expiryScanWindowStart). The expiryScanKeyPrefix keeps the key inside
// the task's own namespace within the shared queue store, and the RFC
// 3339 window stamp keeps the key readable in DeadLetterJobs while staying
// unambiguous.
func expiryScanIdempotencyKey(windowStart time.Time) string {
	return expiryScanKeyPrefix + windowStart.UTC().Format(time.RFC3339)
}

// expiryScanSchedule is the module's declaration of the expiry scan on the
// pkgcore.Registry.Schedules seat: one platform-wide task per window,
// under the standard sentinel tenant, at the service's configured scan
// window (WithExpiryScanWindow included) and keyed with the same prefix
// and window function the manual EnqueueExpiryScan path uses -- so a
// scheduler tick and a manual enqueue landing in one window resolve one
// key and dedupe onto one job.
func (s *Service) expiryScanSchedule() pkgcore.PeriodicTask {
	return pkgcore.PeriodicTask{
		Type:           taskTypeExpiryScan,
		Every:          s.expiryScanWindow,
		Scope:          pkgcore.PeriodicScopePlatform,
		KeyPrefix:      expiryScanKeyPrefix,
		PlatformTenant: platformScanTenantID,
	}
}

// platformScanTenantID is the fixed jobs.Task.TenantID every expiry-scan
// task is enqueued under.
//
// jobs.Task.Validate requires a non-empty TenantID unconditionally
// (go/jobs/task.go) -- every other module's periodic task has a real tenant
// to put there (storage.taskTypeExpirySweep is enqueued once per tenant,
// for exactly that reason) because every other module's scanned data is
// tenant data. pki_signing_keys is not: the expiry scan is a single,
// deployment-wide run, and jobs offers no "no tenant" task shape for that
// case. A fixed sentinel value is the accommodation, not a design pki would
// have chosen on its own -- expiryScanHandler.Handle never reads it,
// because SigningKeyRepository is a plain *gorm.DB with no tenant-filtering
// plugin engaged (SigningKey does not implement dbkit.TenantScoped), so the
// rebuilt tenant context the worker attaches (Handler.Handle's own doc
// comment) is inert for this task. It exists only to satisfy jobs' own
// validation, and using the same fixed value on every call is what makes
// this task's Job rows all belong to one visible "queue", rather than
// scattering across whatever tenant happened to trigger a given tick.
const platformScanTenantID = pkgcore.TenantID("_pki_platform_scan")

// EnqueueExpiryScan schedules one run of the expiry scan (ScanExpiry) onto
// the queue Module was wired with (WithQueue). It carries no payload --
// ScanExpiry takes its inputs from the rows and the clock at run time, plus
// the RotationConfig defaults Service was built with, the same "no payload,
// everything read at run time" shape storage.EnqueueExpirySweep uses.
//
// The task carries a window-scoped idempotency key (expiryScanIdempotencyKey,
// DefaultExpiryScanWindow -- overridable through WithExpiryScanWindow): the
// enqueues of one DefaultExpiryScanWindow window -- a multi-replica scheduler
// whose replicas each tick the same schedule instant, a manual re-run --
// collapse into one job, so the scan runs at most once per window however
// many replicas tick, while an enqueue whose clock has moved into a later
// window is a new job and the scan runs again: the window is what makes a
// keyless per-tick task (every tick its own independent job, so N replicas
// at a 1-minute cadence fire N redundant scans a minute) and a window-less
// keyed task (exactly one scan per database file, since jobs' idempotency
// resolves one key forever on StandaloneQueue) both wrong, and the windowed
// key right. A scan job that dead-letters poisons only its own window.
// ScanExpiry's guarded, status-checked updates (PromoteToActive,
// RetireRetiring) still make two scans of DIFFERENT windows racing or
// overlapping safe to run concurrently -- the state transitions are
// single-statement database-arbitrated writes (see repository.go) -- a
// second run simply finds nothing left to do for whatever the first run
// already advanced.
//
// A host is expected to call this on its own schedule (a cron trigger, a
// periodic goroutine): this module drives the state machine but never
// schedules its own execution; scheduling when a scan runs is host wiring,
// the same division EnqueueExpirySweep's own doc comment draws for
// storage.
//
// A nil queue (Module constructed without WithQueue) makes this report a
// plain error, the same "no queue wired" answer
// storage.LifecycleService.EnqueueExpirySweep gives: scheduling rotation is
// optional, and a host running no workers must not be forced to wire a
// queue it cannot drain.
func (s *Service) EnqueueExpiryScan(ctx context.Context) error {
	if s.queue == nil {
		return errors.New("pki: no queue wired")
	}
	_, err := s.queue.Enqueue(ctx, jobs.Task{
		Type:           taskTypeExpiryScan,
		TenantID:       platformScanTenantID,
		IdempotencyKey: expiryScanIdempotencyKey(expiryScanWindowStart(s.now(), s.expiryScanWindow)),
	})
	return err
}

// expiryScanHandler is the jobs.Handler claiming taskTypeExpiryScan, the
// task EnqueueExpiryScan schedules. Its Handle runs Service.ScanExpiry with
// the RotationConfig zero value, which falls back to whatever
// propagationWindow/renewalLeadTime Service was constructed with (NewModule,
// WithPropagationWindow/WithRenewalLeadTime).
type expiryScanHandler struct {
	svc *Service
}

// Type implements jobs.Handler.
func (h expiryScanHandler) Type() string { return taskTypeExpiryScan }

// Handle implements jobs.Handler. The task's payload must be empty -- see
// EnqueueExpiryScan's doc comment for why a scan needs none; a non-empty
// payload is a task-shape violation the queue's retry policy cannot fix by
// re-running, so it fails the attempt every time exactly like
// storage.expirySweepHandler.Handle's identical check.
func (h expiryScanHandler) Handle(ctx context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
	if len(job.Payload) != 0 {
		return jobs.Result{}, errors.New("pki: expiry-scan task carries an unexpected payload")
	}
	if _, err := h.svc.ScanExpiry(ctx, RotationConfig{}); err != nil {
		return jobs.Result{}, err
	}
	return jobs.Result{}, nil
}

// compile-time check that expiryScanHandler satisfies jobs.Handler.
var _ jobs.Handler = expiryScanHandler{}
