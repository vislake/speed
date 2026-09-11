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
// an owner-facing listing shows an explicit RevokedAt rather than leaving
// the reader to infer "must be expired" by re-deriving isLive.
//
// The task type and the window-scoped idempotency key follow go/storage's
// own expiry-sweep convention (cleanup.go there) as the established
// precedent for "a tenant-scoped jobs.Task, one per expirySweepWindowSize
// window, idempotency keyed on the tenant and the window so a scheduler
// with replicas or a manual re-run collapses one window's enqueues into
// one job while a later window's enqueue schedules the sweep again"; both
// modules derive the key through jobs.ScheduleIdempotencyKey.

import (
	"context"
	"time"

	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// taskTypeExpirySweep names the jobs queue task a sweep run is enqueued
// under. The task carries no payload -- the sweep takes its inputs from the
// rows and the clock at run time.
const taskTypeExpirySweep = "sharing.expiry_sweep"

// expirySweepWindowSize is the period one expiry-sweep idempotency key
// covers: a sweep is enqueued under the key of the expirySweepWindowSize
// window (jobs.ScheduleWindowStart) its enqueue falls in, so same-window
// duplicates -- a scheduler with two replicas, a manual re-run -- merge
// into one job, while an enqueue in a later window becomes a NEW job and
// the sweep runs again. The window is what makes the sweep periodic at
// all: jobs' idempotency is unconditional for one key on StandaloneQueue
// (a resolved key is held forever), so a tenant-only key would give each
// tenant exactly one sweep per database file and let one dead-lettered
// sweep job poison its tenant forever, since every later enqueue would
// keep returning the dead job's id. A dead-lettered job poisons only its
// own window; the next window's enqueue is a fresh key and runs. One hour
// means a share past its expiry is marked --
// and a view reservation that has outlived viewReservationTimeout is
// refunded -- at most expirySweepWindowSize after the sweep that should
// have caught it was enqueued, while keeping the sweep load at one task
// per tenant per hour at most.
const expirySweepWindowSize = time.Hour

// expirySweepKeyPrefix is the prefix of every expiry-sweep idempotency
// key. It is a named constant because two derivations must agree on it
// byte for byte: the manual EnqueueExpirySweep path and the declaration
// (expirySweepSchedule) a jobs.Scheduler expands -- one window must
// resolve one key through both paths, and both derive it through
// jobs.ScheduleIdempotencyKey with this prefix.
const expirySweepKeyPrefix = "sharing.sweep:"

// expirySweepSchedule is the module's declaration of the expiry sweep on
// the pkgcore.ComponentRegistry.Schedules seat: a per-tenant task at the sweep's
// own window, keyed with the same prefix and the same window derivation
// the manual EnqueueExpirySweep path uses (jobs.ScheduleWindowStart /
// jobs.ScheduleIdempotencyKey), so a scheduler tick and a manual enqueue
// landing in one window resolve one key and dedupe onto one job.
var expirySweepSchedule = pkgcore.PeriodicTask{
	Type:      taskTypeExpirySweep,
	Every:     expirySweepWindowSize,
	Scope:     pkgcore.PeriodicScopePerTenant,
	KeyPrefix: expirySweepKeyPrefix,
}

// Sweep runs the expiry-sweep task's two arms over the caller tenant's
// (read from ctx) rows. It is the expiry-sweep task's body (the handler
// Module.Register wires) and doubles as Service's own synchronous entry
// point for a host that wants a tenant swept without going through the
// queue.
//
// Arm 1 marks every share that is past its ExpiresAt or whose MaxViews has
// been reached, and is not already revoked, with RevokedAt set to now.
// Marking reuses Share.RevokedAt rather than a separate column: isLive
// already treats any non-nil RevokedAt as "not live" regardless of why, so
// an expired-and-marked row and an owner-revoked row behave identically to
// Access (which does not need the sweep to have run at all -- see this
// file's own header comment) and are told apart, if a caller cares, by
// comparing RevokedAt against ExpiresAt/MaxViews rather than by a second
// status column.
//
// Arm 2 refunds every view reservation that has OUTLIVED
// viewReservationTimeout (ShareRepository.listStaleReserved) -- the sweep
// half of the "an interrupted reservation is converged" lifecycle, whose
// next-access half is tryReserveView's takeover clause and tryRecordView's
// stale-or-free clause (service.go's viewReservationTimeout doc comment).
// The refund is the same guarded tryRefundView a failed serve's own
// refundAccessView uses, idempotent exactly like it.
//
// The marks go through ShareRepository's guarded markRevoked -- the same
// narrow revoked_at-only UPDATE Service.Revoke uses, never a full-row
// write-back -- and EventShareRevoked is published for every share THIS
// pass actually transitioned (the module.go constant's "owner-initiated or
// sweep-initiated alike" contract), with a failure to publish logged, never
// returned, exactly as on Revoke's own publish path. A share a concurrent
// Revoke already transitioned between this pass's listing and its mark
// affects zero rows, publishes nothing (that revoke already announced
// itself), and is simply skipped.
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
		won, markErr := s.shares.markRevoked(ctx, row.ID, now)
		if markErr != nil {
			return markErr
		}
		if !won {
			// A concurrent Revoke got there first -- it published.
			continue
		}
		observability.FromContext(ctx).Info("share expired by sweep", "share_id", row.ID)
		if pubErr := s.publish(ctx, pkgcore.Event{
			Type:     EventShareRevoked,
			TenantID: pkgcore.TenantID(row.TenantID),
			Payload:  ShareRevokedPayload{ShareID: row.ID},
		}); pubErr != nil {
			observability.FromContext(ctx).Warn("share-revoked event publish failed", "share_id", row.ID, "error", pubErr)
		}
	}

	stale, err := s.shares.listStaleReserved(ctx, now)
	if err != nil {
		return err
	}
	for _, row := range stale {
		won, refundErr := s.shares.tryRefundView(ctx, row.ID, now)
		if refundErr != nil {
			return refundErr
		}
		if !won {
			// The reservation was already resolved -- its serve confirmed or
			// refunded it, or a concurrent revoke's own settle cleared it --
			// between the listing and this refund; nothing to converge.
			continue
		}
		observability.FromContext(ctx).Info("interrupted share view reservation refunded by sweep", "share_id", row.ID)
	}
	return nil
}
