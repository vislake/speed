package smilesim

import (
	"context"
	"strings"
	"time"

	"github.com/vislake/speed/go/jobs"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// DefaultReconcileInterval is how often StartReconciler sweeps for
// outstanding credit reservations when a caller does not choose its own
// interval -- the same "anti-loss poller" role go/config's
// DefaultPollInterval plays for its own cache, applied here to
// creditReservation rows instead of configuration cache entries: the
// durable store (reservation_store.go) is always the source of truth, and
// this sweep is the net that heals a reservation no client ever polls to
// completion, or one whose in-flight settlement was lost to a process
// restart. Five minutes is short enough that a genuinely abandoned
// reservation is corrected well within a support conversation's timescale,
// and long enough that a healthy poll-driven settlement (the ordinary
// path, still handled first by NotifyOnCompletion on every read) resolves
// the row long before the sweep would ever reach it.
const DefaultReconcileInterval = 5 * time.Minute

// ReconcileOutstandingCredits lists every reservation store.listAll still
// has on file, across every tenant, and settles each whose job has
// already reached a terminal status -- the same settleCredit logic
// NotifyOnCompletion's poll-driven path already runs, reused verbatim so
// there is exactly one place that decides what "settle" means. It returns
// the number of reservations it actually settled during this call.
//
// A row whose job id carries orphanRefundJobIDPrefix (see
// reservation_store.go) is settled differently, by design: it records an
// ORPHANED reservation -- Simulate's immediate refund of a failed enqueue
// could not run -- whose job never existed, so there is nothing to poll
// and settleCredit (which requires a *jobs.Job) can never act on it. The
// sweep recognizes such a row and refunds it directly against the row's
// own stored credit key, under CreditService's idempotent-refund
// contract, then deletes the row -- Refund being the only settlement that
// could ever be right for a job that was never enqueued (there is no
// Confirm-shaped outcome to wait for). This is what makes an orphaned
// reservation healable: however long the billing store stays down after
// Simulate's refund failed, the sweep retries on its own long-lived
// context, every pass, until the refund lands and the row goes away.
//
// This is what makes credit settlement reachable independent of any
// client ever polling the job-status route: a reservation whose owning
// job succeeded, dead-lettered or was cancelled while nobody was
// watching -- a closed tab, a network drop, or simply a caller that
// stopped polling once it got the result it wanted -- is corrected the
// next time this runs, not left Reserved forever (see this package's own
// doc comment's "Credit accounting" section, and the queue's own
// FailureHook doc comment on why compensation is a business-module
// concern, never a mechanism the queue itself can express for a
// cross-module case like this one: go/ai-gateway's job handler cannot call
// go/billing directly, since ai-gateway and billing sit at the same
// dependency tier and neither may import the other).
//
// A no-op, returning (0, nil), when this Service has no CreditService,
// no durable store or no jobs.Queue wired -- mirroring every other
// optional-seam no-op this package already documents.
func (s *Service) ReconcileOutstandingCredits(ctx context.Context) (int, error) {
	if s.credits == nil || s.store == nil || s.queue == nil {
		return 0, nil
	}

	rows, err := s.store.listAll(ctx)
	if err != nil {
		return 0, err
	}

	settled := 0
	for _, row := range rows {
		jobID := jobs.JobID(row.JobID)
		tenant := pkgcore.TenantID(row.TenantID)
		// Rebuilt from the reservation's own stored tenant, never trusted
		// from ctx -- root CLAUDE.md's "workers do not inherit tenant
		// context" trap, applied here to a background sweep that has no
		// single ambient tenant of its own at all: it walks rows spanning
		// every tenant in one pass, so each row supplies its own.
		tenantCtx := pkgcore.WithTenant(ctx, tenant)

		// An orphaned reservation -- see this method's own doc comment for
		// what the prefix means and why Refund is the only settlement that
		// could ever be right for it. There is no job to fetch: the sweep
		// refunds the row's stored credit key directly and deletes the row,
		// mirroring settleCredit's own idempotent-retry safety (a leftover
		// row after a successful refund is merely re-refunded -- safely --
		// on a later pass).
		if strings.HasPrefix(row.JobID, orphanRefundJobIDPrefix) {
			if _, refundErr := s.credits.Refund(tenantCtx, row.CreditKey); refundErr != nil {
				obs.FromContext(ctx).Warn("smilesim: reconcile: refund an orphaned credit reservation failed, will retry on the next sweep",
					"job_id", row.JobID, "error", refundErr)
				continue
			}
			if delErr := s.store.delete(tenantCtx, jobID); delErr != nil {
				obs.FromContext(ctx).Warn("smilesim: reconcile: orphaned credit reservation refunded but removing its reservation row failed -- it will be re-refunded (safely, idempotently) on the next sweep",
					"job_id", row.JobID, "error", delErr)
			}
			settled++
			continue
		}

		job, getErr := s.queue.Get(tenantCtx, jobID)
		if getErr != nil {
			obs.FromContext(ctx).Warn("smilesim: reconcile: fetch job status failed, will retry on the next sweep",
				"job_id", row.JobID, "error", getErr)
			continue
		}
		if !job.Status.Terminal() {
			continue
		}

		if settleErr := s.settleCredit(tenantCtx, job); settleErr != nil {
			obs.FromContext(ctx).Warn("smilesim: reconcile: settle credit reservation failed, will retry on the next sweep",
				"job_id", row.JobID, "error", settleErr)
			continue
		}
		settled++
	}
	return settled, nil
}

// StartReconciler launches a background goroutine that calls
// ReconcileOutstandingCredits every interval (DefaultReconcileInterval
// when interval is zero or negative), and returns a stop function that
// signals the goroutine to exit and blocks until it has -- the same
// ticker-goroutine-plus-stop-channel shape go/config's own anti-loss
// poller (service.go's startPoller) establishes for the identical
// "durable store is the source of truth, this loop is the healing net"
// role.
//
// It is a no-op returning a no-op stop func when this Service has no
// CreditService, no durable store or no jobs.Queue wired -- the same
// nil-is-legal contract ReconcileOutstandingCredits itself follows, so a
// caller can always call StartReconciler unconditionally and defer its
// stop func without checking whether reconciliation is actually wired.
//
// ctx is the background context ReconcileOutstandingCredits runs under on
// every tick -- it should carry no tenant (each row supplies its own; see
// ReconcileOutstandingCredits' own doc comment) and should outlive the
// stop func's own call, not be cancelled by the same shutdown sequence
// that calls stop, since a cancelled ctx would make every in-flight
// Queue.Get/settleCredit call on that final tick fail closed instead of
// simply not being started.
func (s *Service) StartReconciler(ctx context.Context, interval time.Duration) (stop func()) {
	if s.credits == nil || s.store == nil || s.queue == nil {
		return func() {}
	}
	if interval <= 0 {
		interval = DefaultReconcileInterval
	}

	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				if _, err := s.ReconcileOutstandingCredits(ctx); err != nil {
					obs.FromContext(ctx).Warn("smilesim: reconcile: sweep failed, will retry on the next tick",
						"error", err)
				}
			}
		}
	}()
	return func() {
		close(stopCh)
		<-doneCh
	}
}
