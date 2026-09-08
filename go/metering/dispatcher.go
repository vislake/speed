package metering

import (
	"context"
	"sync"
	"time"

	"gorm.io/gorm"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	obs "github.com/vislake/speed/go/observability"
)

// Defaults for Dispatcher's poll loop, overridden by Module's
// WithDispatchInterval / WithDispatchBatchSize / WithDispatchRetryDelay /
// WithOutboxRetention options.
const (
	defaultDispatchInterval  = 2 * time.Second
	defaultDispatchBatchSize = 50
	// defaultDispatchRetryDelay is how long a failed delivery attempt
	// keeps its row out of the candidate set: the row's retry_after moves
	// to the failure time plus this delay, so a re-failed row is retried
	// at most once per delay and never re-joins the queue head while rows
	// enqueued during its window wait (see claimPendingOutboxRecords' and
	// markOutboxAttemptFailed's doc comments). It defaults to the poll
	// interval -- one retry per cycle per row -- but measured from the
	// failure itself rather than from queue position.
	defaultDispatchRetryDelay = 2 * time.Second
	// defaultDispatchEscalationAttempts is the stated escalation horizon
	// for the billing-grade delivery contract's alert half (see
	// Dispatcher's "Escalation" doc comment): a permanently failing outbox
	// row whose failed delivery attempts reach this count is logged at
	// Error level on that attempt and on every later failed one, where a
	// per-attempt Warn alone would not make operations notice it.
	// At the module's default pacing (one retry per
	// defaultDispatchRetryDelay, two seconds) the horizon is reached
	// roughly two minutes after a row's first failure; a host that
	// overrides the retry delay -- or the escalation count itself, via
	// WithDispatchEscalationAttempts -- scales the wall-clock meaning with
	// it.
	defaultDispatchEscalationAttempts = 60
	// defaultOutboxRetention is how long a delivered outbox row (and the
	// ingest receipt its delivery created) stays on the table before the
	// retention sweep retires both (see retireDeliveredOutboxRecords).
	// The window is the module's Enqueue-idempotency horizon: a caller
	// retrying an Enqueue whose answer it never saw resolves against the
	// existing row while it lives; after the window row and receipt are
	// gone and a re-enqueued key is a genuinely new event. The receipt is
	// what makes redelivery of a still-pending row idempotent, which is
	// why the sweep never touches pending rows or their receipts.
	defaultOutboxRetention = 7 * 24 * time.Hour
)

// Dispatcher is the billing-grade tier's delivery half of the outbox
// pattern: a background poller that claims pending metering_outbox_records
// rows and delivers each into Aggregator.Ingest, retrying INDEFINITELY on
// failure rather than dropping (the billing-grade delivery contract: a
// delivery failure retries indefinitely, plus an alert). The alert half of
// that contract is the escalation described in its own section below. The
// implementation is an in-process goroutine poller; a jobs-queue-driven
// poller would need a real claim step before it could run more than one
// worker -- see the "Single in-process dispatcher assumed" section below
// for what that costs.
//
// # Retry is scheduled, not priority-classed
//
// A delivery failure leaves the row outboxStatusPending (Attempts
// incremented, LastError recorded, RetryAfter moved to the failure time
// plus d.retryDelay) rather than retrying it immediately in a loop -- the
// claim query refuses the row until its RetryAfter arrives, and the next
// RunOnce cycle after that moment, driven by the poll interval, is the
// retry. This is the scheduled-eligibility discipline go/jobs' own
// scheduled_at gives its retrying jobs, with a fixed rather than curving
// delay (a real backoff curve -- the delay growing per attempt -- is not
// implemented): every failing row is retried at most once per retryDelay,
// measured from the failure itself.
//
// # A failed row re-enters the queue at a future moment, never its head
//
// Because RetryAfter -- not attempts, not age -- orders the claim
// (claimPendingOutboxRecords), a row that failed is out of the candidate
// set for the whole retry delay. That one property delivers both fairness
// directions at once, which an attempts-based class ranking could not:
//
//   - A pile of permanently failing rows cannot occupy batch after batch
//     ahead of a healthy row enqueued behind them: every row enqueued
//     while the pile waits its window out sorts ahead of it, so the
//     healthy row is claimed on the very next cycle, whatever the pile's
//     age or size.
//   - A row that failed once -- or fifty times -- is reached the moment
//     its window opens, whatever the arrival rate of new rows: no
//     sustained flood can push its schedule slot later than the retry
//     delay itself. Ranking never-failed rows as a strict class ahead of
//     every failed row would starve exactly this row: under a sustained
//     enqueue rate, every batch full of never-failed rows, a row that
//     failed once would never be claimed again -- permanent starvation of
//     the very rows retry exists to reach. The retry schedule is the
//     claim order precisely so no class ranking can do that.
//
// # Escalation: the alert the billing-grade contract promises
//
// A row that fails forever is not a data-loss case -- it is an
// observability one. The billing-grade delivery contract -- "retries
// indefinitely, plus an alert" -- has its retry half in the schedule above;
// the alert half is this: a row whose failed delivery attempts reach the
// stated escalation
// horizon (escalationAttempts, default defaultDispatchEscalationAttempts,
// host-tunable through Module.WithDispatchEscalationAttempts) switches its
// failure cadence from the per-attempt Warn
// (metering.outbox_delivery_failed) to an Error-level alert
// (metering.outbox_delivery_escalated) naming the row -- outbox_id,
// tenant_id, feature, the attempt count and the threshold. The alert
// repeats on every subsequent failed attempt, so a stuck row stays
// continuously visible to log-based alerting on that Error key instead of
// alerting once and falling silent: operations discovers the row within
// roughly escalationAttempts x the retry delay of its first failure (about
// two minutes at the module's default pacing). The count is the row's own
// durable Attempts, so the horizon survives a process restart. The
// escalation is a signal layered on the retry, never a cap under it: the
// row keeps being retried indefinitely exactly as before, never converges
// to a terminal state, and a row that recovers -- fails below the horizon,
// then delivers -- never escalates at all (the Dispatcher tests pin both
// halves).
//
// # Retention: delivered rows and receipts are retired
//
// run also drives the outbox retention sweep
// (retireDeliveredOutboxRecords) each cycle: delivered rows that have
// stayed delivered for d.retention, and the ingest receipt each delivered
// row's fold created, are deleted together in one transaction. Pending
// rows and their receipts are never touched -- a pending row is the retry
// queue, and its receipt is what makes its redelivery idempotent. Without
// the sweep both tables would grow without bound. The window is the
// idempotency horizon documented on defaultOutboxRetention: a caller
// retrying an Enqueue whose answer it never saw resolves against the
// existing row while it lives; a key re-enqueued after its row was retired
// is a genuinely new event.
//
// # Single in-process dispatcher assumed
//
// claimPendingOutboxRecords is a read, not an atomic claim-and-lock: it
// does not mark a row as "being processed" before RunOnce attempts
// delivery. That is safe with exactly one Dispatcher running against a
// database at a time -- the dispatcher is an in-process goroutine, not a
// distributed worker pool -- and would double-deliver under two concurrent
// Dispatcher processes racing the same pending row. A jobs-queue-driven
// poller would need a real claim step (an atomic status: pending ->
// processing transition with a visibility timeout) before running more
// than one worker.
type Dispatcher struct {
	db         *gorm.DB
	aggregator *Aggregator

	interval   time.Duration
	batchSize  int
	retryDelay time.Duration
	retention  time.Duration
	// escalationAttempts is the stated escalation horizon: a row whose
	// failed delivery attempts reach this count is escalated from the
	// per-attempt Warn to the Error-level alert described in the type's
	// "Escalation" doc comment. See defaultDispatchEscalationAttempts for
	// the default's wall-clock meaning.
	escalationAttempts int

	// delivery carries metering.outbox.delivery (metrics.go),
	// registered by NewDispatcher; nil for a bare struct literal, which
	// the record sites guard.
	delivery metric.Int64Counter

	// mu guards every lifecycle field below, exactly as on
	// AnalyticsRecorder. The poll goroutine reads stop/done only through
	// the channel values Start passes it as arguments (see run), so no
	// lifecycle field is ever read outside mu.
	mu         sync.Mutex
	started    bool // a poll goroutine is running (spawned, not yet stopped)
	stopClosed bool // stop has been closed (at most once per loop generation)
	stop       chan struct{}
	done       chan struct{}
}

// NewDispatcher returns a Dispatcher polling db for aggregator's pending
// outbox rows, with the default interval, batch size, retry delay,
// escalation threshold and retention. Module's WithDispatchInterval /
// WithDispatchBatchSize / WithDispatchRetryDelay /
// WithDispatchEscalationAttempts / WithOutboxRetention options override
// the fields Module wires (same package, see module.go).
func NewDispatcher(db *gorm.DB, aggregator *Aggregator) *Dispatcher {
	return &Dispatcher{
		db:                 db,
		aggregator:         aggregator,
		delivery:           registerOutboxDeliveryMetric(),
		interval:           defaultDispatchInterval,
		batchSize:          defaultDispatchBatchSize,
		retryDelay:         defaultDispatchRetryDelay,
		retention:          defaultOutboxRetention,
		escalationAttempts: defaultDispatchEscalationAttempts,
	}
}

// Start runs the poll loop until ctx is done or Stop is called. Safe to
// call with one loop running at a time: a Start while a loop is already
// running is a no-op, and a Start after the running loop has exited --
// a completed Stop, or a canceled ctx, which run clears the started flag
// for itself on exit (see run) -- runs a fresh loop with the new ctx.
// Calling Start immediately after canceling the previous ctx, before the
// exiting loop has finished its own exit, is still a no-op by the "one
// loop at a time" rule; wait for the exit (Stop, or observe the loop's
// end) before restarting.
func (d *Dispatcher) Start(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started {
		return
	}
	d.started = true
	d.stopClosed = false
	d.stop = make(chan struct{})
	d.done = make(chan struct{})
	stop, done := d.stop, d.done
	go d.run(ctx, stop, done)
}

// run is the poll loop: one delivery cycle (RunOnce) and one bounded
// retention pass per tick. stop and done are passed as arguments, never
// read off the receiver: Start and Stop exchange them under mu, and the
// goroutine must not touch fields the caller is mutating.
//
// On exit it clears the started flag for its own loop generation unless
// Stop is already handling that: a loop that ends because Stop closed
// stop leaves the clearing (and the drain) to Stop's own post-wait code,
// while a loop that ends because ctx was canceled has no Stop to do it --
// without the clearing, started would stay true forever and a later Start
// would no-op, leaving pending rows unclaimed. The generation check
// (d.done == done) makes the clearing a no-op when a newer Start has
// already replaced the channels.
func (d *Dispatcher) run(ctx context.Context, stop <-chan struct{}, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		if _, err := d.RunOnce(ctx); err != nil {
			obs.FromContext(ctx).Warn("metering.dispatch_cycle_failed", "error", err)
		}
		if err := d.retentionSweep(ctx); err != nil {
			obs.FromContext(ctx).Warn("metering.outbox_retention_sweep_failed", "error", err)
		}
		select {
		case <-ticker.C:
		case <-stop:
			d.clearStartedIfCurrent(done)
			return
		case <-ctx.Done():
			d.clearStartedIfCurrent(done)
			return
		}
	}
}

// clearStartedIfCurrent clears the started flag when the loop that is
// exiting is still the live generation, so a later Start can run a fresh
// loop. A Stop that initiated this exit clears the flag itself after
// waiting on done (see Stop); the clearing here is idempotent with that,
// and is what makes a ctx-canceled loop leave Start restartable.
func (d *Dispatcher) clearStartedIfCurrent(done chan struct{}) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.done == done {
		d.started = false
	}
}

// retentionSweep retires delivered outbox rows that have stayed delivered
// past d.retention, and their receipts, up to one bounded batch per call
// (the delivery batch size) -- see retireDeliveredOutboxRecords for what
// the sweep can safely delete and why.
func (d *Dispatcher) retentionSweep(ctx context.Context) error {
	_, err := retireDeliveredOutboxRecords(ctx, d.db, time.Now().Add(-d.retention), d.batchSize)
	return err
}

// Stop signals the poll loop to exit and waits for it to actually do so.
// Safe to call before Start, or more than once: a Stop before any Start
// leaves a later Start's loop fully stoppable (nothing is consumed by
// the early call), and a Start after a completed Stop runs a fresh loop.
func (d *Dispatcher) Stop() {
	d.mu.Lock()
	if !d.started {
		d.mu.Unlock()
		return
	}
	if !d.stopClosed {
		d.stopClosed = true
		close(d.stop)
	}
	done := d.done
	d.mu.Unlock()

	<-done

	d.mu.Lock()
	// Clear started only if the loop this Stop waited on is still the
	// live one: a Start racing this Stop's wait has replaced the channels
	// with a fresh generation, whose started flag must survive.
	if d.done == done {
		d.started = false
	}
	d.mu.Unlock()
}

// RunOnce claims up to one batch of pending outbox records and attempts to
// deliver each into the aggregation pipeline, returning how many were
// successfully delivered this cycle. A per-row delivery failure is logged
// and leaves that row pending for the next cycle; it is never returned as
// this method's own error, since one bad row must not stop the rest of the
// batch from being attempted. The returned error is non-nil only when
// claiming the batch itself failed (a database-level problem, not a
// per-row one).
//
// RunOnce is exported so a host -- or a test -- can drive one delivery
// cycle synchronously without waiting on the poll interval.
func (d *Dispatcher) RunOnce(ctx context.Context) (delivered int, err error) {
	records, err := claimPendingOutboxRecords(ctx, d.db, d.batchSize)
	if err != nil {
		return 0, err
	}
	for _, rec := range records {
		if d.deliverOne(ctx, rec) {
			delivered++
		}
	}
	return delivered, nil
}

// deliverOne attempts to ingest rec into d.aggregator and mark it
// delivered, reporting whether it succeeded. Every failure along the way
// is logged rather than propagated -- see RunOnce's own doc comment for
// why. A failed delivery attempt is logged either as the per-attempt Warn
// or, once the row's failed attempts reach the escalation horizon, as the
// Error-level alert -- see the type's "Escalation" doc comment.
//
// It calls Aggregator.IngestBillingGrade, never the plain Ingest: this is
// the billing-grade delivery path, and markOutboxDelivered below (the
// SEPARATE write that actually retires rec from "pending") can itself
// fail, or the process can die between the two calls, leaving rec pending
// for the next RunOnce cycle to reclaim and redeliver -- IngestBillingGrade
// is what makes that redelivery a safe no-op instead of a silent double
// count. See IngestReceipt's doc comment for the full argument.
// recordDeliveryOutcome counts one delivery attempt onto
// metering.outbox.delivery (metrics.go) under its outcome.
func (d *Dispatcher) recordDeliveryOutcome(ctx context.Context, outcome string) {
	if d.delivery == nil {
		return
	}
	d.delivery.Add(ctx, 1,
		metric.WithAttributes(attribute.String(outcomeAttr, outcome)))
}

func (d *Dispatcher) deliverOne(ctx context.Context, rec OutboxRecord) bool {
	event := UsageEvent{
		TenantID:       rec.TenantID,
		Feature:        rec.Feature,
		Quantity:       rec.Quantity,
		IdempotencyKey: rec.IdempotencyKey,
		OccurredAt:     rec.OccurredAt,
		Metadata:       decodeMetadata(rec.Metadata),
	}

	if err := d.aggregator.IngestBillingGrade(ctx, event); err != nil {
		if markErr := markOutboxAttemptFailed(ctx, d.db, rec.ID, err.Error(), time.Now().Add(d.retryDelay)); markErr != nil {
			obs.FromContext(ctx).Warn("metering.outbox_mark_failed_attempt_error",
				"error", markErr,
				"outbox_id", rec.ID,
			)
		}
		// The alert half of the billing-grade delivery contract (see the
		// type's "Escalation" doc comment): once this row's failed attempts
		// reach the stated horizon -- this attempt is number
		// rec.Attempts+1, rec.Attempts being the failures recorded before
		// it -- the failure cadence switches from the per-attempt Warn to
		// an Error-level alert naming the row, repeated on every later
		// failed attempt so the stuck row stays visible until it is fixed.
		if rec.Attempts+1 >= d.escalationAttempts {
			obs.FromContext(ctx).Error("metering.outbox_delivery_escalated",
				"error", err,
				"outbox_id", rec.ID,
				"tenant_id", rec.TenantID,
				"feature", rec.Feature,
				"attempts", rec.Attempts+1,
				"escalation_threshold", d.escalationAttempts,
			)
		} else {
			obs.FromContext(ctx).Warn("metering.outbox_delivery_failed",
				"error", err,
				"outbox_id", rec.ID,
				"tenant_id", rec.TenantID,
				"feature", rec.Feature,
			)
		}
		d.recordDeliveryOutcome(ctx, outcomeFailed)
		return false
	}

	if err := markOutboxDelivered(ctx, d.db, rec.ID, time.Now()); err != nil {
		obs.FromContext(ctx).Warn("metering.outbox_mark_delivered_error",
			"error", err,
			"outbox_id", rec.ID,
		)
		d.recordDeliveryOutcome(ctx, outcomeFailed)
		return false
	}
	d.recordDeliveryOutcome(ctx, outcomeSucceeded)
	return true
}
