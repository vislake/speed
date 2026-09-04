package sharing

// This file is the module's expiry-cleanup runtime: a periodic pass that
// finds every share past its ExpiresAt or whose MaxViews has been exhausted
// and marks it, so an owner-facing listing converges on "revoked" rather
// than leaving a row that Access already refuses (isLive already checks
// both conditions live, on every call) sitting around indistinguishable
// from one nobody has looked at yet.
//
// The sweep is deliberately NOT what makes Access correctness hold --
// Share.isLive is evaluated fresh on every Service.Access call regardless
// of whether a sweep has ever run, mirroring go/storage's own LifecycleService
// doc comment ("Sweep is what makes interrupted deletions finish", not what
// makes a single Delete call correct). What the sweep buys is row hygiene:
// ListAccessLog's caller and any future admin-facing share listing see an
// explicit RevokedAt rather than inferring "must be expired" by re-deriving
// isLive themselves.
//
// EnqueueExpirySweepIdempotencyKey and the task type follow go/storage's own
// expiry-sweep convention (cleanup.go there) as the established precedent
// for "a tenant-scoped jobs.Task, one per tenant, idempotency keyed on the
// tenant so a scheduler with replicas or a manual re-run collapses into one
// job".

import (
	"context"
	"errors"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// taskTypeExpirySweep names the jobs queue task a sweep run is enqueued
// under. The task carries no payload -- the sweep takes its inputs from the
// rows and the clock at run time.
const taskTypeExpirySweep = "sharing.expiry_sweep"

// expirySweepIdempotencyKey derives the jobs idempotency key of a tenant's
// expiry-sweep task from the tenant id, mirroring go/storage's identical
// expirySweepIdempotencyKey: two enqueues for the same tenant's sweep
// collapse into one job, so a tenant is never swept by two workers at once.
func expirySweepIdempotencyKey(tenant pkgcore.TenantID) string {
	return "sharing.sweep:" + string(tenant)
}

// Sweep marks every one of the caller tenant's (read from ctx) shares that
// is past its ExpiresAt or whose MaxViews has been reached, and is not
// already revoked, with RevokedAt set to now. It is the expiry-sweep task's
// body (expirySweepHandler) and doubles as Service's own synchronous entry
// point for a host that wants a tenant swept without going through the
// queue.
//
// Marking reuses Share.RevokedAt rather than a separate column: isLive
// already treats any non-nil RevokedAt as "not live" regardless of why, so
// an expired-and-marked row and an owner-revoked row behave identically to
// Access (which does not need the sweep to have run at all -- see this
// file's own header comment) and are told apart, if a caller cares, by
// comparing RevokedAt against ExpiresAt/MaxViews rather than by a second
// status column.
//
// Each row is updated independently; a failure on one row stops the pass
// (mirroring go/storage's Sweep, "failing the pass rather than plowing
// through the rest of the rows on a broken seam is what keeps each row's
// error attributable"), leaving every row this pass has not yet reached for
// the next run to pick up.
func (s *Service) Sweep(ctx context.Context) error {
	now := s.now()
	rows, err := s.shares.listExpiredOrExhausted(ctx, now)
	if err != nil {
		return err
	}
	for _, row := range rows {
		revokedAt := now
		row.RevokedAt = &revokedAt
		if err := s.shares.Update(ctx, &row); err != nil {
			return err
		}
		observability.FromContext(ctx).Info("share expired by sweep", "share_id", row.ID)
	}
	return nil
}

// expirySweepHandler is the jobs.Handler claiming taskTypeExpirySweep, the
// task EnqueueExpirySweep schedules. Its Handle runs Service.Sweep on the
// tenant context the worker rebuilt from the task -- Register registers one
// instance of this handler, backed by the module's own Service, onto
// reg.Jobs so a host that drains the registry's handlers onto its own
// jobs.Queue gets a worker that reaps expired shares.
type expirySweepHandler struct {
	svc *Service
}

// Type returns the task type this handler claims.
func (h expirySweepHandler) Type() string { return taskTypeExpirySweep }

// Handle runs one expiry-sweep task. The task's payload must be empty --
// the sweep takes its inputs from the rows and the clock at run time.
func (h expirySweepHandler) Handle(ctx context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
	if len(job.Payload) != 0 {
		return jobs.Result{}, errors.New("sharing: expiry-sweep task carries an unexpected payload")
	}
	if err := h.svc.Sweep(ctx); err != nil {
		return jobs.Result{}, err
	}
	return jobs.Result{}, nil
}

// compile-time check that expirySweepHandler satisfies jobs.Handler.
var _ jobs.Handler = expirySweepHandler{}
