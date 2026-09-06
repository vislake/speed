package metering

import (
	"context"
	"sync"
	"time"

	"gorm.io/gorm"

	obs "github.com/vislake/speed/go/observability"
)

// Defaults for Dispatcher's poll loop, overridden by Module's
// WithDispatchInterval / WithDispatchBatchSize options.
const (
	defaultDispatchInterval  = 2 * time.Second
	defaultDispatchBatchSize = 50
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
// # Retry is the poll loop itself, not a per-row backoff
//
// RunOnce claims a batch and attempts each row once. A row whose delivery
// fails is left outboxStatusPending (with Attempts incremented and
// LastError recorded) rather than being retried immediately in a loop --
// the NEXT RunOnce cycle, driven by the poll interval, is the retry. This
// keeps the failure path simple (no per-row backoff scheduling this
// round) at the cost of every failing row being retried at the same fixed
// interval as every other pending row, regardless of how many times it
// has already failed -- a real backoff curve is future hardening, not
// this round's job (see AGENTS.md).
//
// # Failed rows never jump the queue
//
// The claim orders never-failed rows ahead of already-failed ones
// (attempts ASC, then created_at ASC -- see claimPendingOutboxRecords), so
// a pile of permanently failing rows at the head of the queue cannot
// occupy batch after batch ahead of a healthy row enqueued behind them:
// the healthy row is claimed on the very next cycle, whatever the pile's
// age or size, while the pile keeps being retried whenever the batch has
// room -- the same "a retried unit of work waits its turn behind new
// work" discipline go/jobs' own scheduled_at backoff gives its retrying
// jobs, without a timer column.
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

	interval  time.Duration
	batchSize int

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
// outbox rows, with the default interval and batch size. Module's
// WithDispatchInterval / WithDispatchBatchSize options override the
// fields Module wires (same package, see module.go).
func NewDispatcher(db *gorm.DB, aggregator *Aggregator) *Dispatcher {
	return &Dispatcher{
		db:         db,
		aggregator: aggregator,
		interval:   defaultDispatchInterval,
		batchSize:  defaultDispatchBatchSize,
	}
}

// Start runs the poll loop until ctx is done or Stop is called. Safe to
// call with one loop running at a time: a Start while a loop is already
// running is a no-op, and a Start after a completed Stop runs a fresh
// loop with the new ctx.
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

// run is the poll loop. stop and done are passed as arguments, never read
// off the receiver: Start and Stop exchange them under mu, and the
// goroutine must not touch fields the caller is mutating.
func (d *Dispatcher) run(ctx context.Context, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		if _, err := d.RunOnce(ctx); err != nil {
			obs.FromContext(ctx).Warn("metering.dispatch_cycle_failed", "error", err)
		}
		select {
		case <-ticker.C:
		case <-stop:
			return
		case <-ctx.Done():
			return
		}
	}
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
		if markErr := markOutboxAttemptFailed(ctx, d.db, rec.ID, err.Error()); markErr != nil {
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
