package metering

import (
	"context"
	"sync"
	"sync/atomic"

	obs "github.com/vislake/speed/go/observability"
	"go.opentelemetry.io/otel/metric"
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
// # Shutdown is a drain, not a drop
//
// Stop delivers every event still buffered into the aggregator before
// returning -- the shutdown counterpart of the fail-open rule above. The
// "never silently lost" promise holds across the Stop boundary: an event
// is lost only where the loss is explicit and counted (a full buffer, a
// Record made after Stop has latched closed, or a buffered event whose
// Ingest into the aggregator failed -- deliver counts all three into
// Dropped(), the third added so a delivery failure is not a silent loss
// any more than a full buffer is).
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

	// Metric instruments (metrics.go): metering.events.ingested and
	// metering.events.dropped, registered by NewAnalyticsRecorder.
	// Nil for a recorder built as a bare struct literal (tests), which
	// the record sites guard the same way authn's recordAuthMetric
	// guards its instruments.
	ingestedCount metric.Int64Counter
	droppedMetric metric.Int64Counter

	// mu guards every lifecycle field below, and is held by Record around
	// the stopped check and the buffer enqueue so that check is atomic with
	// respect to Stop: an event either lands in the buffer before Stop
	// latches closed -- and is then delivered by the flush loop or by
	// Stop's own drain -- or is a counted drop afterwards. It is never
	// buffered into a drain that has already run. The flush goroutine
	// reads stop/done only through the channel values Start passes it as
	// arguments (see run), so no lifecycle field is ever read outside mu --
	// the sync.Once pair this replaces left stop/done readable from Stop's
	// goroutine while a concurrent Start wrote them, a race the detector
	// could see.
	mu      sync.Mutex
	started bool // a flush goroutine is running (spawned, not yet stopped)
	stopped bool // Stop has been called; Record drops and counts from here on
	// stopClosed records that stop has been closed, so a Stop racing
	// another Stop closes it once per loop generation.
	stopClosed bool
	stop       chan struct{}
	done       chan struct{}
}

// NewAnalyticsRecorder returns an AnalyticsRecorder that flushes into
// aggregator, with the default buffer size. Module's WithAnalyticsBufferSize
// option overrides the channel Module wires (same package, see module.go);
// a caller building one directly outside Module can do the same by setting
// the events field before calling Start.
func NewAnalyticsRecorder(aggregator *Aggregator) *AnalyticsRecorder {
	ingested, dropped := registerIngestDropMetrics()
	return &AnalyticsRecorder{
		aggregator:    aggregator,
		events:        make(chan UsageEvent, defaultAnalyticsBufferSize),
		ingestedCount: ingested,
		droppedMetric: dropped,
	}
}

// Record implements Recorder. See the type's own doc comment for the
// fail-open drop behavior. After Stop has been called, Record drops and
// counts instead of buffering: the recorder's drain has already run or is
// about to, so a buffered event could never be delivered (see Stop's own
// doc comment for the shutdown contract).
func (r *AnalyticsRecorder) Record(ctx context.Context, event UsageEvent) error {
	if err := event.validate(); err != nil {
		return err
	}
	// The stopped check and the enqueue share one lock with Stop, so the
	// two are mutually atomic: see the mu field's own comment for why that
	// is what makes "delivered, or dropped and counted" airtight across
	// the shutdown boundary. The send below is non-blocking (default
	// branch), so holding mu for it can never block on a full buffer.
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		r.drop(ctx, event)
		return nil
	}
	select {
	case r.events <- event:
		r.mu.Unlock()
		// metering.events.ingested -- the analytics channel's ingest
		// rate (metrics.go's doc comment maps the row).
		if r.ingestedCount != nil {
			r.ingestedCount.Add(ctx, 1)
		}
		return nil
	default:
		r.mu.Unlock()
		r.drop(ctx, event)
		return nil
	}
}

// drop counts event as dropped and logs the structured warning the
// type's fail-open contract promises, for both drop reasons: a full
// buffer (Record's select default) and a stopped recorder.
func (r *AnalyticsRecorder) drop(ctx context.Context, event UsageEvent) {
	r.dropped.Add(1)
	// metering.events.dropped -- the fail-open contract's counted-loss
	// rate (metrics.go's doc comment maps the row); the internal
	// counter above stays for the Dropped() accessor's API.
	if r.droppedMetric != nil {
		r.droppedMetric.Add(ctx, 1)
	}
	obs.FromContext(ctx).Warn("metering.analytics_event_dropped",
		"tenant_id", event.TenantID,
		"feature", event.Feature,
	)
}

// Dropped returns the number of events lost so far -- because the buffer
// was full, because the recorder had already been stopped when Record was
// called, or because a buffered event's Ingest into the aggregator failed
// (deliver counts that too) -- for a host to wire into its own metrics.
func (r *AnalyticsRecorder) Dropped() int64 { return r.dropped.Load() }

// Start runs the background flush loop until ctx is done or Stop is
// called. Safe to call with one loop running at a time: a Start while a
// loop is already running is a no-op, and a Start after the running loop
// has exited -- a completed Stop, or a canceled ctx, which run clears the
// started flag for itself on exit (see run) -- runs a fresh loop with the
// new ctx. A fresh loop consumes whatever the previous one left buffered,
// so events Recorded between a cancel and the restart are delivered, not
// lost. Calling Start immediately after canceling the previous ctx,
// before the exiting loop has finished its own exit, is still a no-op by
// the "one loop at a time" rule; wait for the exit (Stop, or observe the
// loop's end) before restarting.
func (r *AnalyticsRecorder) Start(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return
	}
	r.started = true
	r.stopped = false
	r.stopClosed = false
	r.stop = make(chan struct{})
	r.done = make(chan struct{})
	stop, done := r.stop, r.done
	go r.run(ctx, stop, done)
}

// run drains r.events into r.aggregator until stopped. A per-event
// Ingest failure is logged and counted into Dropped (see deliver) and
// does not stop the loop -- one malformed or transiently failing event
// must not silence the rest of the buffer, which is the whole point of a
// fail-open tier. stop and done are passed as arguments, never read off
// the receiver: Start and Stop exchange them under mu, and the goroutine
// must not touch fields the caller is mutating.
//
// On exit it clears the started flag for its own loop generation unless
// Stop is already handling that: a loop that ends because Stop closed
// stop leaves the clearing (and the drain) to Stop's own post-wait code,
// while a loop that ends because ctx was canceled has no Stop to do it --
// without the clearing, started would stay true forever, a later Start
// would no-op, and Record would buffer into a loop that would never run
// again (reviewer finding P3-metering-14). Buffered events survive the
// exit: the stopped latch is NOT set here, so a Record made after the
// cancel still buffers honestly, and whatever sits in the buffer when the
// next Start runs a fresh loop -- or when a later Stop drains -- is
// delivered then. The generation check (r.done == done) makes the
// clearing a no-op when a newer Start has already replaced the channels.
func (r *AnalyticsRecorder) run(ctx context.Context, stop <-chan struct{}, done chan struct{}) {
	defer close(done)
	for {
		select {
		case event := <-r.events:
			r.deliver(ctx, event)
		case <-stop:
			r.clearStartedIfCurrent(done)
			return
		case <-ctx.Done():
			r.clearStartedIfCurrent(done)
			return
		}
	}
}

// clearStartedIfCurrent clears the started flag when the loop that is
// exiting is still the live generation, so a later Start can run a fresh
// loop. A Stop that initiated this exit clears the flag itself after
// waiting on done (see Stop); the clearing here is idempotent with that,
// and is what makes a ctx-canceled loop leave Start restartable.
func (r *AnalyticsRecorder) clearStartedIfCurrent(done chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done == done {
		r.started = false
	}
}

// Stop latches the recorder closed and delivers every event still
// buffered into the aggregator before returning. It waits for the flush
// loop (if one is running) to exit first -- the loop may itself be
// mid-drain, and must never be a second concurrent consumer of the same
// buffer -- then drains whatever is left itself, so an event that was
// buffered when Stop was called is delivered, not silently lost: the
// type doc's "an event is dropped, or delivered; it is never silently
// lost" promise holds across the shutdown boundary. A Record made after
// Stop has latched closed can no longer be delivered, so it is dropped
// and counted exactly like a full-buffer drop.
//
// Stop is safe to call before Start, or more than once; a Stop while no
// loop is running simply drains the buffer. The drain delivers each
// event best-effort over context.Background() -- deliberately not over
// the loop's ctx, which may already be canceled at shutdown -- and logs
// a per-event failure exactly like the loop does; a host that needs
// stronger guarantees uses the billing-grade Enqueue tier instead.
func (r *AnalyticsRecorder) Stop() {
	r.mu.Lock()
	// Latch closed under mu, before the drain: every Record that acquires
	// mu after this point takes the counted-drop branch (see Record), so
	// nothing can land in the buffer behind this Stop's own drain.
	r.stopped = true
	if !r.started {
		r.mu.Unlock()
		r.drain()
		return
	}
	if !r.stopClosed {
		r.stopClosed = true
		close(r.stop)
	}
	done := r.done
	r.mu.Unlock()

	<-done

	r.mu.Lock()
	// Clear started only if the loop this Stop waited on is still the
	// live one: a Start racing this Stop's wait has replaced the channels
	// with a fresh generation, whose started flag must survive.
	if r.done == done {
		r.started = false
	}
	r.mu.Unlock()

	r.drain()
}

// drain delivers every event still sitting in the buffer, best-effort.
// Callers must ensure the flush goroutine has exited (or was never
// started) and that stopped is latched, so this drain is the buffer's
// only consumer and no Record can race it -- see Stop for both.
func (r *AnalyticsRecorder) drain() {
	for {
		select {
		case event := <-r.events:
			r.deliver(context.Background(), event)
		default:
			return
		}
	}
}

// deliver attempts to ingest event into the aggregator, logging -- never
// propagating -- a per-event failure, and counting the failed event into
// Dropped (reviewer finding P2-metering-11): a buffered event whose
// Ingest fails is a lost event exactly like a full-buffer drop -- it will
// never reach the summary row or the real-time counter -- so the
// fail-open tier's "an event is dropped, or delivered; it is never
// silently lost" accounting must count it. Shared by the flush loop and
// Stop's drain so both honor the identical contract: one bad event must
// not stop the rest of the buffer from being delivered.
func (r *AnalyticsRecorder) deliver(ctx context.Context, event UsageEvent) {
	if err := r.aggregator.Ingest(ctx, event); err != nil {
		r.dropped.Add(1)
		obs.FromContext(ctx).Warn("metering.analytics_ingest_failed",
			"error", err,
			"tenant_id", event.TenantID,
			"feature", event.Feature,
		)
	}
}

// compile-time check that *AnalyticsRecorder satisfies Recorder.
var _ Recorder = (*AnalyticsRecorder)(nil)
