package pki

import (
	"context"
	"errors"

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
// A host is expected to call this on its own schedule (a cron trigger, a
// periodic goroutine) -- per docs/internal/22-pki.md's "rotation" section,
// this module drives the state machine but never schedules its own execution;
// scheduling when a scan runs is host wiring, the same division
// EnqueueExpirySweep's own doc comment draws for storage. No idempotency
// key is set: unlike a per-tenant sweep whose repeated triggers over the
// SAME occurrence should collapse into one Job, each expiry-scan tick is
// its own independent occurrence, and ScanExpiry's guarded, status-checked
// updates (PromoteToActive, RetireRetiring) make two ticks racing or
// overlapping safe to run concurrently -- a second run simply finds nothing
// left to do for whatever the first run already advanced.
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
		Type:     taskTypeExpiryScan,
		TenantID: platformScanTenantID,
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
