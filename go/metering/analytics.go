package metering

import (
	"context"
	"sync"
	"sync/atomic"

	obs "github.com/vislake/speed/go/observability"
)

// defaultAnalyticsBufferSize is AnalyticsRecorder's channel capacity when
// the host does not call WithAnalyticsBufferSize.
const defaultAnalyticsBufferSize = 1024

// AnalyticsRecorder is the analytics-grade Recorder implementation
// (docs/internal/06-billing-and-metering.md's "reliability tiers" section,
// analytics-grade row): an in-process bounded channel plus a background
// flush goroutine feeding Aggregator.Ingest.
//
// # Fail-open, loudly
//
// When the channel is full, Record does NOT block the caller and does NOT
// return an error: it drops event, increments an internal counter
// (Dropped), and logs a structured warning. This is the design doc's
// explicit rule that analytics-grade metering must never become a source
// of business-request latency or failure: a full buffer drops the event
// and counts it, with an alert, rather than blocking. A caller that needs
// "never dropped" uses Enqueue (the billing-grade tier) instead;
// AnalyticsRecorder is the wrong tool for that requirement by design, not
// by omission.
//
// # No idempotency dedup this round
//
// UsageEvent.IdempotencyKey is carried on every event this records, but
// AnalyticsRecorder does NOT deduplicate a retried Record call against it:
// doing so would require persisting every seen key somewhere durable
// (a Redis SETNX-backed check, for instance), which contradicts this
// tier's whole "cheap, in-memory, best-effort" positioning. See
// AGENTS.md's Known limitations. The billing-grade Enqueue path DOES
// dedupe, at the database level, because that path already pays for
// durable storage on every call.
type AnalyticsRecorder struct {
	aggregator *Aggregator
	events     chan UsageEvent
	dropped    atomic.Int64

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}
}

// NewAnalyticsRecorder returns an AnalyticsRecorder that flushes into
// aggregator, with the default buffer size. Module's WithAnalyticsBufferSize
// option overrides the channel Module wires (same package, see module.go);
// a caller building one directly outside Module can do the same by setting
// the events field before calling Start.
func NewAnalyticsRecorder(aggregator *Aggregator) *AnalyticsRecorder {
	return &AnalyticsRecorder{
		aggregator: aggregator,
		events:     make(chan UsageEvent, defaultAnalyticsBufferSize),
	}
}

// Record implements Recorder. See the type's own doc comment for the
// fail-open drop behavior.
func (r *AnalyticsRecorder) Record(ctx context.Context, event UsageEvent) error {
	if err := event.validate(); err != nil {
		return err
	}
	select {
	case r.events <- event:
		return nil
	default:
		r.dropped.Add(1)
		obs.FromContext(ctx).Warn("metering.analytics_event_dropped",
			"tenant_id", event.TenantID,
			"feature", event.Feature,
		)
		return nil
	}
}

// Dropped returns the number of events dropped so far because the buffer
// was full, for a host to wire into its own metrics.
func (r *AnalyticsRecorder) Dropped() int64 { return r.dropped.Load() }

// Start runs the background flush loop until ctx is done or Stop is
// called. It is safe to call at most once per AnalyticsRecorder; later
// calls are no-ops (sync.Once), matching go/jobs.StandaloneQueue.Start's
// identical single-start contract.
func (r *AnalyticsRecorder) Start(ctx context.Context) {
	r.startOnce.Do(func() {
		r.stop = make(chan struct{})
		r.done = make(chan struct{})
		go r.run(ctx)
	})
}

// run drains r.events into r.aggregator until stopped. A per-event
// Ingest failure is logged and does not stop the loop -- one malformed or
// transiently failing event must not silence the rest of the buffer,
// which is the whole point of a fail-open tier.
func (r *AnalyticsRecorder) run(ctx context.Context) {
	defer close(r.done)
	for {
		select {
		case event := <-r.events:
			if err := r.aggregator.Ingest(ctx, event); err != nil {
				obs.FromContext(ctx).Warn("metering.analytics_ingest_failed",
					"error", err,
					"tenant_id", event.TenantID,
					"feature", event.Feature,
				)
			}
		case <-r.stop:
			return
		case <-ctx.Done():
			return
		}
	}
}

// Stop signals the flush loop to exit and waits for it to actually do so.
// Calling Stop before Start, or more than once, is safe.
func (r *AnalyticsRecorder) Stop() {
	r.stopOnce.Do(func() {
		if r.stop != nil {
			close(r.stop)
		}
	})
	if r.done != nil {
		<-r.done
	}
}

// compile-time check that *AnalyticsRecorder satisfies Recorder.
var _ Recorder = (*AnalyticsRecorder)(nil)
