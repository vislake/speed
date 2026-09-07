package metering

import (
	"context"
	"sync"
	"time"

	"gorm.io/gorm"

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
	// interval -- one retry per cycle per row, the pacing this module
	// always documented -- but measured from the failure itself rather
	// than from queue position.
	defaultDispatchRetryDelay = 2 * time.Second
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
// failure rather than dropping (docs/internal/06-billing-and-metering.md's
// billing-grade row: delivery failure retries indefinitely, plus an
// alert). This round's implementation is an in-process goroutine (the
// task's own scope explicitly allows a jobs-queue-driven poller as
// later-round hardening); see AGENTS.md's Known limitations for exactly
// what that costs.
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
// delay this round (a real backoff curve remains future hardening, see
// AGENTS.md): every failing row is retried at most once per retryDelay,
// measured from the failure itself.
//
// # A failed row re-enters the queue at a future moment, never its head
//
// Because RetryAfter -- not attempts, not age -- orders the claim
// (claimPendingOutboxRecords), a row that failed is out of the candidate
// set for the whole retry delay. That one property delivers both fairness
// directions an ordering alone could not hold at once:
//
//   - A pile of permanently failing rows cannot occupy batch after batch
//     ahead of a healthy row enqueued behind them: every row enqueued
//     while the pile waits its window out sorts ahead of it, so the
//     healthy row is claimed on the very next cycle, whatever the pile's
//     age or size.
//   - A row that failed once -- or fifty times -- is reached the moment
//     its window opens, whatever the arrival rate of new rows: no
//     sustained flood can push its schedule slot later than the retry
//     delay itself. (The ordering this replaces ranked never-failed rows
//     as a strict class ahead of every failed row, so under a sustained
//     enqueue rate -- every batch full of never-failed rows -- a row that
//     failed once was never claimed again: permanent starvation of
//     exactly the rows retry exists to reach. Reviewer finding
//     P1-metering-10; see migration 0005.)
//
// # Retention: delivered rows and receipts are retired
//
// run also drives the outbox retention sweep
// (retireDeliveredOutboxRecords) each cycle: delivered rows that have
// stayed delivered for d.retention, and the ingest receipt each delivered
// row's fold created, are deleted together in one transaction. Pending
// rows and their receipts are never touched -- a pending row is the retry
// queue, and its receipt is what makes its redelivery idempotent. Without
// the sweep both tables grew without bound (reviewer finding
// P3-metering-16). The window is the idempotency horizon documented on
// defaultOutboxRetention: a caller retrying an Enqueue whose answer it
// never saw resolves against the existing row while it lives; a key
// re-enqueued after its row was retired is a genuinely new event.
//
// # Single in-process dispatcher assumed
//
// claimPendingOutboxRecords is a read, not an atomic claim-and-lock: it
// does not mark a row as "being processed" before RunOnce attempts
// delivery. That is safe with exactly one Dispatcher running against a
// database at a time (this round's whole story -- an in-process goroutine,
// not a distributed worker pool), and would double-deliver under two
// concurrent Dispatcher processes racing the same pending row. See
// AGENTS.md's Known limitations for what a jobs-queue-driven poller (the
// explicitly allowed later hardening) would need to add.
type Dispatcher struct {
	db         *gorm.DB
	aggregator *Aggregator

	interval   time.Duration
	batchSize  int
	retryDelay time.Duration
	retention  time.Duration

	// mu guards every lifecycle field below, exactly as on
	// AnalyticsRecorder. The poll goroutine reads stop/done only through
	// the channel values Start passes it as arguments (see run), so no
	// lifecycle field is ever read outside mu -- the sync.Once pair this
	// replaces left stop/done readable from Stop's goroutine while a
	// concurrent Start wrote them, a race the detector could see.
	mu         sync.Mutex
	started    bool // a poll goroutine is running (spawned, not yet stopped)
	stopClosed bool // stop has been closed (at most once per loop generation)
	stop       chan struct{}
	done       chan struct{}
}

// NewDispatcher returns a Dispatcher polling db for aggregator's pending
// outbox rows, with the default interval, batch size, retry delay and
// retention. Module's WithDispatchInterval / WithDispatchBatchSize /
// WithDispatchRetryDelay / WithOutboxRetention options override the
// fields Module wires (same package, see module.go).
func NewDispatcher(db *gorm.DB, aggregator *Aggregator) *Dispatcher {
	return &Dispatcher{
		db:         db,
		aggregator: aggregator,
		interval:   defaultDispatchInterval,
		batchSize:  defaultDispatchBatchSize,
		retryDelay: defaultDispatchRetryDelay,
		retention:  defaultOutboxRetention,
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
// without the clearing, started would stay true forever, a later Start
// would no-op, and Record would buffer into a loop that would never run
// again (the lifecycle defect reviewer finding P3-metering-14 closes for
// the AnalyticsRecorder; this is its Dispatcher twin). The generation
// check (d.done == done) makes the clearing a no-op when a newer Start
// has already replaced the channels.
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
// RunOnce is exported so a host -- or a test proving the crash-recovery
// property Enqueue's atomicity promises -- can drive one delivery cycle
// synchronously without waiting on the poll interval.
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
// why.
//
// It calls Aggregator.IngestBillingGrade, never the plain Ingest: this is
// the billing-grade delivery path, and markOutboxDelivered below (the
// SEPARATE write that actually retires rec from "pending") can itself
// fail, or the process can die between the two calls, leaving rec pending
// for the next RunOnce cycle to reclaim and redeliver -- IngestBillingGrade
// is what makes that redelivery a safe no-op instead of a silent double
// count. See IngestReceipt's doc comment for the full argument.
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
		obs.FromContext(ctx).Warn("metering.outbox_delivery_failed",
			"error", err,
			"outbox_id", rec.ID,
			"tenant_id", rec.TenantID,
			"feature", rec.Feature,
		)
		return false
	}

	if err := markOutboxDelivered(ctx, d.db, rec.ID, time.Now()); err != nil {
		obs.FromContext(ctx).Warn("metering.outbox_mark_delivered_error",
			"error", err,
			"outbox_id", rec.ID,
		)
		return false
	}
	return true
}
