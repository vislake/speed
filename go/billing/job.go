package billing

import (
	"context"
	"errors"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// This file is the active-polling fallback
// docs/internal/06-billing-and-metering.md requires: a callback may arrive
// before the order-creation response returns, or may never arrive at all,
// so an active-polling fallback is mandatory -- a jobs scheduled task
// scanning orders stuck in an intermediate state. A PaymentEvent row
// inserted at ChannelStatusPending (payment_event.go) and never updated by
// a later webhook is exactly that intermediate-state order;
// PollingService.Poll re-queries each such row's owning channel through
// PaymentGateway.QueryStatus -- the same authoritative,
// never-trust-the-callback-body re-query docs/internal/06-billing-and-metering.md's
// callbacks-cannot-be-trusted rule requires of live webhook processing too,
// run here proactively instead of reactively.

// taskTypePoll names the jobs queue task PollingService.EnqueuePoll
// schedules and pollHandler claims. The task is tenant-scoped, mirroring
// go/storage's taskTypeExpirySweep: every query Poll runs is tenant-scoped
// (billing_payment_events is tenant data -- PaymentEvent's own doc comment),
// so a host with many tenants schedules one task per tenant.
const taskTypePoll = "billing.poll_pending_payments"

// pollIdempotencyKey derives the jobs idempotency key of a tenant's poll
// task from the tenant id, per the rule that an idempotency key derives
// from the business operation, never random -- the identical shape
// go/storage's expirySweepIdempotencyKey uses for its own per-tenant sweep:
// two enqueues for the same tenant's poll (a scheduler with two replicas, a
// manual re-run) collapse into one job.
func pollIdempotencyKey(tenant pkgcore.TenantID) string {
	return "billing.poll:" + string(tenant)
}

// DefaultPollStuckAfter is how long a PaymentEvent may sit at
// ChannelStatusPending before PollingService.Poll re-queries it. Chosen as
// a conservative default longer than any channel's own ordinary webhook
// delivery latency, so an ordinary, on-time webhook is never raced by the
// poll -- a poll re-querying a row whose webhook is about to arrive
// naturally just confirms the same answer the webhook would have reported.
const DefaultPollStuckAfter = 15 * time.Minute

// defaultPollBatchLimit bounds how many rows one Poll call re-queries, so a
// tenant with an unusually large backlog of stuck rows cannot make one poll
// task run unboundedly long; the rest are picked up by the next scheduled
// run.
const defaultPollBatchLimit = 100

// PollingService is the active-polling fallback: Poll finds every
// ChannelStatusPending PaymentEvent row of the caller's tenant older than
// StuckAfter and re-queries its owning channel's authoritative status
// through PaymentGateway.QueryStatus, recording what it finds back onto the
// row.
//
// # What this round's Poll does NOT do
//
// It updates PaymentEvent.Status alone -- it does not drive any
// Subscription or Invoice transition from what QueryStatus reports. That is
// a deliberate round boundary, not an oversight: no HTTP surface exists yet
// to receive a live webhook in the first place (this round's own stated
// non-scope), so there is no live processing loop for this round's polling
// fallback to feed into, and half-wiring one without a real caller to prove
// it against is exactly the kind of speculative build-ahead
// go/billing/AGENTS.md's own "Live audit.Emit calls" entry already declines
// for a different mechanism, for the identical reason. A later round's live
// webhook endpoint is where a PaymentEvent's Status actually starts driving
// billing's own domain state.
type PollingService struct {
	events *PaymentEventRepository
	// gateways maps NormalizedEvent.Channel/PaymentEvent.Channel (e.g.
	// "stripe") to the already-constructed PaymentGateway a host wired for
	// it -- WithGateways' own doc comment explains why this is a plain map
	// a host builds once, rather than a per-call PaymentGatewayRegistry.Build
	// lookup: a registry Build call re-constructs a fresh implementation
	// (and, for the providers in this round, would re-parse credentials)
	// from a Config on every call, which is the wrong cost to pay once per
	// stuck row on every poll tick.
	gateways map[string]PaymentGateway
	queue    jobs.Queue

	now        func() time.Time
	stuckAfter time.Duration
	batchLimit int
}

// newPollingService returns a PollingService over events, re-querying stuck
// rows through gateways (may be nil or empty -- a channel with no wired
// gateway is skipped, logged, and left pending for the next run rather than
// failing the whole pass; see Poll's own doc comment) and enqueuing further
// runs onto queue (may be nil -- see EnqueuePoll's own doc comment).
func newPollingService(events *PaymentEventRepository, gateways map[string]PaymentGateway, queue jobs.Queue) *PollingService {
	return &PollingService{
		events:     events,
		gateways:   gateways,
		queue:      queue,
		now:        time.Now,
		stuckAfter: DefaultPollStuckAfter,
		batchLimit: defaultPollBatchLimit,
	}
}

// Poll runs one pass over the caller's tenant: every ChannelStatusPending
// PaymentEvent row older than StuckAfter is re-queried through
// PaymentGateway.QueryStatus and its Status updated to whatever the channel
// authoritatively reports right now -- never the webhook body's own
// numbers, per docs/internal/06-billing-and-metering.md's
// callbacks-cannot-be-trusted rule,
// which this active-polling fallback obeys identically to live webhook
// processing.
//
// ctx must carry a tenant -- the worker rebuilds it from the task's
// TenantID before Handle runs (see pollHandler.Handle), and a direct caller
// passes pkgcore.WithTenant -- because every query this runs is
// tenant-scoped.
//
// A row whose Channel has no wired PaymentGateway (gateways has no entry
// for it) is skipped with a logged warning, left pending for a later run --
// this is a configuration gap, not a data error, and must not fail the rest
// of the tenant's batch. A QueryStatus call that itself errors is likewise
// logged and skipped, not fatal to the pass: unlike storage's Sweep (whose
// phases fail fast because a failed row usually means a broken store or
// database, worth stopping over), a single channel being unreachable for
// one row must not block every other row's re-query in the same batch.
func (s *PollingService) Poll(ctx context.Context) error {
	now := s.now()
	stuck, err := s.events.listPending(ctx, now.Add(-s.stuckAfter), s.batchLimit)
	if err != nil {
		return err
	}

	log := observability.FromContext(ctx)
	for _, row := range stuck {
		gw, ok := s.gateways[row.Channel]
		if !ok {
			log.Warn("payment poll: no gateway wired for channel",
				"channel", row.Channel, "payment_event_id", row.ID)
			continue
		}

		status, _, err := gw.QueryStatus(ctx, ChannelReference(row.ChannelReference))
		if err != nil {
			log.Warn("payment poll: query status failed",
				"channel", row.Channel, "payment_event_id", row.ID, "error", err)
			continue
		}
		if status == ChannelStatusPending {
			// Still pending: nothing changed, nothing to write.
			continue
		}
		if err := s.events.markStatus(ctx, row.ID, status); err != nil {
			log.Warn("payment poll: mark status failed",
				"channel", row.Channel, "payment_event_id", row.ID, "error", err)
			continue
		}
		log.Info("payment poll: status resolved",
			"channel", row.Channel, "payment_event_id", row.ID, "status", string(status))
	}
	return nil
}

// EnqueuePoll enqueues the poll task for the tenant ctx carries -- the
// host-facing schedule point, matching go/storage's EnqueueExpirySweep and
// go/pki's EnqueueExpiryScan: a host with workers runs this on its own
// timer per tenant, and the task's per-tenant idempotency key collapses
// concurrent enqueues into one job.
//
// ctx must carry a tenant. With no queue wired (nil -- Module constructed
// without WithQueue), this fails with a plain error: polling is optional
// work, and a host running no workers must not be forced to wire a queue it
// cannot drain.
func (s *PollingService) EnqueuePoll(ctx context.Context) error {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return err
	}
	if s.queue == nil {
		return errors.New("billing: no queue wired")
	}
	_, err = s.queue.Enqueue(ctx, jobs.Task{
		Type:           taskTypePoll,
		TenantID:       tenant,
		IdempotencyKey: pollIdempotencyKey(tenant),
	})
	return err
}

// pollHandler is the jobs.Handler claiming taskTypePoll, the task
// EnqueuePoll schedules. Its Handle runs PollingService.Poll on the tenant
// context the worker rebuilt from the task.
type pollHandler struct {
	svc *PollingService
}

// Type implements jobs.Handler.
func (h pollHandler) Type() string { return taskTypePoll }

// Handle implements jobs.Handler. The task's payload must be empty -- a
// poll pass takes its inputs from the rows and the clock at run time, the
// identical shape go/storage's expirySweepHandler and go/pki's
// expiryScanHandler both use for their own periodic tasks.
func (h pollHandler) Handle(ctx context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
	if len(job.Payload) != 0 {
		return jobs.Result{}, errors.New("billing: poll task carries an unexpected payload")
	}
	if err := h.svc.Poll(ctx); err != nil {
		return jobs.Result{}, err
	}
	return jobs.Result{}, nil
}

// compile-time check that pollHandler satisfies jobs.Handler.
var _ jobs.Handler = pollHandler{}
