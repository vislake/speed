package billing

import (
	"context"
	"errors"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// This file is the active-polling fallback the billing design requires: a
// callback may arrive before the order-creation response returns, or may
// never arrive at all, so an active-polling fallback is mandatory -- a
// jobs scheduled task scanning orders stuck in an intermediate state. A
// PaymentEvent row inserted at ChannelStatusPending (payment_event.go)
// and never updated by a later webhook is exactly that intermediate-state
// order; PollingService.Poll re-queries each such row's owning channel
// through PaymentGateway.QueryStatus -- the same authoritative re-query
// of a channel whose callbacks cannot be trusted, run here proactively
// instead of reactively.

// taskTypePoll names the jobs queue task PollingService.EnqueuePoll
// schedules and pollHandler claims. The task is tenant-scoped, mirroring
// go/storage's taskTypeExpirySweep: every query Poll runs is tenant-scoped
// (billing_payment_events is tenant data -- PaymentEvent's own doc comment),
// so a host with many tenants schedules one task per tenant.
const taskTypePoll = "billing.poll_pending_payments"

// pollIdempotencyWindowSize is the period one poll idempotency key
// covers, mirroring go/storage's expirySweepWindowSize and go/compliance's
// retentionSweepWindowSize exactly: a poll is enqueued under the key of
// the pollIdempotencyWindowSize window (pollWindowStart) its enqueue falls
// in, so the same-window duplicates the original key existed to collapse
// -- a scheduler with two replicas, a manual re-run -- still merge into
// one job, while an enqueue in a later window becomes a NEW job and the
// poll runs again. The window is what makes the poll periodic at all:
// jobs' idempotency is unconditional for one key on StandaloneQueue (a
// resolved key is held forever), so a tenant-only key would give each
// tenant exactly one poll task per database file -- and, worse, a poll
// job that dead-letters would poison its tenant forever, since every
// later enqueue would keep returning the dead job's id. A dead-lettered
// job poisons only its own window; the next window's enqueue is a fresh
// key and runs. The
// window is chosen equal to DefaultPollStuckAfter (15 minutes), the
// poll's own detection granularity: a poll cadence finer than the stuck
// threshold has nothing extra to detect (no row is poll-eligible until it
// has sat Pending for StuckAfter), and the window bounds a stuck
// payment's unpolled time to at most one StuckAfter after it becomes
// stuck -- the fallback keeps detecting newly-stuck payments on the same
// scale its own threshold names, which an hour-scale window (the
// day-scale sweeps' choice) would soften to hour-granularity detection.
const pollIdempotencyWindowSize = DefaultPollStuckAfter

// pollWindowStart is the poll window the enqueue at now belongs to -- the
// absolute boundary now.Truncate(pollIdempotencyWindowSize) lands in, the
// twin of storage's expirySweepWindowStart. Two replicas enqueuing within
// the same window share one key (and one job); a tick in a later window
// gets its own. Truncation is on the absolute clock, never a
// timezone-local calendar cut, so every replica agrees on the boundary
// regardless of its own location.
func pollWindowStart(now time.Time) time.Time {
	return now.Truncate(pollIdempotencyWindowSize)
}

// pollIdempotencyKey derives the jobs idempotency key of one poll window
// for a tenant, per the rule that an idempotency key derives from the
// business operation, never random -- the identical shape go/storage's
// expirySweepIdempotencyKey uses for its own per-tenant sweep: the
// operation one key names is "the poll of windowStart", not "some poll or
// other" -- a periodic task's identity inherently includes WHICH period
// it is for. windowStart is the pollIdempotencyWindowSize window start
// the enqueue belongs to (pollWindowStart). Two enqueues for the same
// tenant's poll inside one window (a scheduler with two replicas, a
// manual re-run) collapse into one job; a dead-lettered job poisons only
// its own window; and an enqueue in a later window resolves a fresh key
// and the poll runs again -- a stuck PaymentEvent is actively polled as
// long as a host keeps scheduling EnqueuePoll, instead of once per tenant
// lifetime.
func pollIdempotencyKey(tenant pkgcore.TenantID, windowStart time.Time) string {
	return "billing.poll:" + string(tenant) + ":" + windowStart.UTC().Format(time.RFC3339)
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
// # What Poll does NOT do
//
// It updates PaymentEvent.Status alone -- it does not drive any
// Subscription or Invoice transition from what QueryStatus reports. No
// HTTP surface exists yet to receive a live webhook, so there is no live
// processing loop for the polling fallback to feed into, and half-wiring
// one without a real caller to prove it against would be speculative
// build-ahead. A live webhook endpoint is where a PaymentEvent's Status
// actually starts driving billing's own domain state.
type PollingService struct {
	events *PaymentEventRepository
	// gateways maps NormalizedEvent.Channel/PaymentEvent.Channel (e.g.
	// "stripe") to the already-constructed PaymentGateway a host wired for
	// it -- WithGateways' own doc comment explains why this is a plain map
	// a host builds once, rather than a per-call PaymentGatewayRegistry.Build
	// lookup: a registry Build call re-constructs a fresh implementation
	// (and re-parses credentials for the providers that store them in
	// their Config) from a Config on every call, which is the wrong cost
	// to pay once per stuck row on every poll tick.
	gateways map[string]PaymentGateway
	queue    jobs.Queue

	// now is the clock both Poll and EnqueuePoll read: Poll's own stuck
	// cutoff (listPending's now.Add(-stuckAfter)) and EnqueuePoll's window
	// placement (pollWindowStart). It is a field, not a time.Now() call at
	// each site, so both the poll cutoff and the window an enqueue lands
	// under are deterministic in tests -- the same clock-seam pattern
	// go/storage's expiry sweep uses -- while defaulting to the real clock
	// for every production call.
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
// numbers, whose trustworthiness this active-polling fallback treats
// exactly as live webhook processing does.
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
// logged (with its bounded failure classification only, never the raw
// gateway error text -- see pollFailureClass) and skipped, not fatal to the
// pass: unlike storage's Sweep (whose phases fail fast because a failed row
// usually means a broken store or database, worth stopping over), a single
// channel being unreachable for one row must not block every other row's
// re-query in the same batch.
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

		status, amount, err := gw.QueryStatus(ctx, ChannelReference(row.ChannelReference))
		if err != nil {
			log.Warn("payment poll: query status failed",
				"channel", row.Channel, "payment_event_id", row.ID,
				"failure_class", pollFailureClass(err))
			continue
		}
		if status == ChannelStatusPending {
			// Still pending: nothing changed, nothing to write.
			continue
		}
		// amount is QueryStatus's own freshly re-queried Money, persisted
		// alongside the status transition -- never discarded. A row can
		// reach this point holding a zero-valued Amount (event.go's
		// normalizeCheckoutSession deliberately zeroes it on the
		// ChannelStatusPending row a checkout.session.completed-but-unpaid
		// webhook inserts, since an unsettled session carries no real
		// amount yet), and this is often the ONLY place that ever resolves
		// such a row: recording only the Status here would permanently
		// ledger a genuinely successful, money-moved payment as a
		// zero-amount success.
		if err := s.events.markStatus(ctx, row.ID, status, amount); err != nil {
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
// timer per tenant, relying on the task's window-scoped idempotency key
// (pollIdempotencyKey) to collapse the enqueues of one
// pollIdempotencyWindowSize window into one job. An enqueue whose clock
// has moved into a later window (pollWindowStart) is a new job and runs
// again -- this is what makes the poll periodic on queues whose
// idempotency is unconditional (StandaloneQueue holds a resolved key
// forever, so a tenant-only key would poll a tenant exactly once per
// database file, and a dead-lettered poll would silence its tenant's
// later enqueues entirely), and what keeps one dead-lettered poll window
// from poisoning its tenant's later windows; see pollIdempotencyKey's doc
// comment for the full window semantics.
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
		IdempotencyKey: pollIdempotencyKey(tenant, pollWindowStart(s.now())),
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

// pollFailureClassUnclassified is pollFailureClass's fixed answer for a
// QueryStatus error that carries no typed billing classification -- the
// poll layer's honest ceiling: it cannot see into a gateway's own error
// (a WeChat/Alipay business-envelope message, a Stripe SDK error), so the
// line says exactly that the failure is unclassified rather than guessing
// from the error's text.
const pollFailureClassUnclassified = "unclassified"

// pollFailureClass returns the bounded classification the poll log line
// may carry for one QueryStatus failure: the error's own apperr code when
// the error is (or wraps) a typed *apperr.Error -- the code space of the
// module error indexes, code-authored and bounded, the one classification
// this layer can name across every gateway a host may wire -- and the
// fixed pollFailureClassUnclassified literal otherwise.
//
// The raw error's text is deliberately never logged, here or anywhere in
// Poll's failure branch: a PaymentGateway error is external-provider free
// text by construction (the wechatAPIError message a WeChat Pay APIv3
// refusal body carries, alipay's query-failed envelope fields, a Stripe
// SDK error), and go/observability's redaction layer masks secret-shaped
// values and sensitive attribute-key names, never arbitrary free text
// under a benign, ubiquitous key like "error" -- so the guarantee has to
// be made at this log point, by bounding the value. The classification is
// read from the error's Code only, never by stringifying it: an apperr
// with an unbounded cause renders "code: cause", so even the typed case
// must not reach the log as text. A status code is not logged because
// none is available here in bounded form: the error object carries no
// status field this layer can name (a gateway embeds an HTTP status in
// its own text at best, which this function must not parse), and
// apperr.Error.Status is the suggested HTTP status for an API response,
// not the channel's own answer -- logging it would mislead. A gateway
// that maps more of its provider's conditions onto billing error codes
// automatically refines this line, since the code is read from the error
// chain itself.
func pollFailureClass(err error) string {
	if appErr, ok := apperr.As(err); ok {
		return appErr.Code
	}
	return pollFailureClassUnclassified
}

// compile-time check that pollHandler satisfies jobs.Handler.
var _ jobs.Handler = pollHandler{}
