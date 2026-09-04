package metering

import (
	"context"
	"sync"
	"time"

	"gorm.io/gorm"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
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

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}
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
// call at most once per Dispatcher (sync.Once), matching
// AnalyticsRecorder.Start's and go/jobs.StandaloneQueue.Start's identical
// contract.
func (d *Dispatcher) Start(ctx context.Context) {
	d.startOnce.Do(func() {
		d.stop = make(chan struct{})
		d.done = make(chan struct{})
		go d.run(ctx)
	})
}

func (d *Dispatcher) run(ctx context.Context) {
	defer close(d.done)
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		if _, err := d.RunOnce(ctx); err != nil {
			obs.FromContext(ctx).Warn("metering.dispatch_cycle_failed", "error", err)
		}
		select {
		case <-ticker.C:
		case <-d.stop:
			return
		case <-ctx.Done():
			return
		}
	}
}

// Stop signals the poll loop to exit and waits for it to actually do so.
// Calling Stop before Start, or more than once, is safe.
func (d *Dispatcher) Stop() {
	d.stopOnce.Do(func() {
		if d.stop != nil {
			close(d.stop)
		}
	})
	if d.done != nil {
		<-d.done
	}
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
func (d *Dispatcher) deliverOne(ctx context.Context, rec OutboxRecord) bool {
	event := UsageEvent{
		TenantID:       rec.TenantID,
		Feature:        rec.Feature,
		Quantity:       rec.Quantity,
		IdempotencyKey: rec.IdempotencyKey,
		OccurredAt:     rec.OccurredAt,
		Metadata:       decodeMetadata(rec.Metadata),
	}
	tenantCtx := pkgcore.WithTenant(ctx, pkgcore.TenantID(rec.TenantID))

	if err := d.aggregator.Ingest(tenantCtx, event); err != nil {
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
