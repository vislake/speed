package pki

import (
	"context"
	"errors"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// taskTypeExpiryScan names the jobs queue task EnqueueExpiryScan schedules
// and the module's registered scan handler claims. One run walks every purpose's signing keys
// (lifecycle.go's ScanExpiry) -- there is no per-tenant shape to this task
// the way storage.taskTypeExpirySweep has one, because pki_signing_keys is
// platform data (model.go), not tenant data: a signing key belongs to the
// whole deployment, never to one tenant.
const taskTypeExpiryScan = "pki.expiry_scan"

// DefaultExpiryScanWindow is the period one expiry-scan idempotency key
// covers when the host does not override it (WithExpiryScanWindow): a scan
// is enqueued under the key of the DefaultExpiryScanWindow window
// (jobs.ScheduleWindowStart) its enqueue falls in, so the same-window
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

// expiryScanKeyPrefix is the prefix of every expiry-scan idempotency key.
// It is a named constant because two derivations must agree on it byte for
// byte: the manual EnqueueExpiryScan path and the declaration
// (Service.expiryScanSchedule) a jobs.Scheduler expands -- one window must
// resolve one key through both paths, and both derive it through
// jobs.SchedulePlatformIdempotencyKey with this prefix.
const expiryScanKeyPrefix = "pki.expiry_scan:"

// expiryScanSchedule is the module's declaration of the expiry scan on the
// ComponentRegistry's Schedules seat: one platform-wide task per window,
// under the standard sentinel tenant, at the service's configured scan
// window (WithExpiryScanWindow included) and keyed with the same prefix
// and the same window derivation the manual EnqueueExpiryScan path uses
// (jobs.ScheduleWindowStart / jobs.SchedulePlatformIdempotencyKey) -- so a
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
// have chosen on its own -- the registered scan handler never reads it,
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
// The task carries a window-scoped idempotency key (jobs.SchedulePlatformIdempotencyKey,
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
// The scan's default schedule is the module's own: whenever a queue is
// wired, Register declares the task on the ComponentRegistry's Schedules seat
// (Service.expiryScanSchedule, a platform-scope declaration at the
// service's configured window), so a host that runs a jobs.Scheduler over
// the finished registry runs the scan without writing a schedule point of
// its own. A host that runs no scheduler can still call this directly on a
// cadence of its own -- go/pki itself never enqueues anything -- and a
// manual trigger off that cadence is always legal, deduping onto the
// scheduler's job through the window-scoped key above.
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
		IdempotencyKey: jobs.SchedulePlatformIdempotencyKey(expiryScanKeyPrefix, jobs.ScheduleWindowStart(s.now(), s.expiryScanWindow)),
	})
	return err
}

// runScheduledExpiryScan runs one scheduled expiry scan: ScanExpiry with
// the RotationConfig zero value, which falls back to whatever
// propagationWindow/renewalLeadTime Service was constructed with (NewModule,
// WithPropagationWindow/WithRenewalLeadTime), with the regenerated list
// discarded -- a scheduled task's outcome is success or failure alone.
// Module.Register wires it through jobs.NewEmptyPayloadHandler.
func (s *Service) runScheduledExpiryScan(ctx context.Context) error {
	_, err := s.ScanExpiry(ctx, RotationConfig{})
	return err
}
