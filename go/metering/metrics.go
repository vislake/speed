package metering

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// InstrumentationName identifies this package's own meter, mirroring
// go/jobs/standalone_queue.go's, go/notification/delivery.go's and
// go/authn/service.go's identical use of their own package path.
const InstrumentationName = "github.com/vislake/speed/go/metering"

// Metric instrument names registerMeteringMetrics wires under
// InstrumentationName -- the "metering pipeline" row of
// docs/internal/09-observability.md's must-instrument table: event
// ingest rate, outbox delivery health and aggregation latency. The row
// is covered as follows, all low-cardinality labels, never tenant_id:
//
//   - metering.events.ingested -- the analytics tier's event ingest
//     rate: every UsageEvent AnalyticsRecorder.Record accepts into its
//     buffer. The billing-grade tier's ingest rate has no counter at
//     its write site because Enqueue is a free function over a caller's
//     transaction with no instance lifetime to hold an instrument --
//     its events surface on the aggregation side instead, as the
//     outbox channel of metering.aggregation.duration and through
//     metering.outbox.delivery.
//   - metering.events.dropped -- every event the analytics tier loses
//     for one of its three counted reasons (a full buffer, a Record
//     after Stop latched, a delivery whose Ingest failed) -- the
//     drop rate the tier's fail-open contract owes an alert on
//     (docs/internal/06-billing-and-metering.md's analytics row:
//     "never silently lost" is kept honest by counting).
//   - metering.outbox.delivery -- one count per outbox delivery
//     attempt by the Dispatcher poller, outcome-labeled succeeded or
//     failed. A failed attempt leaves the row pending for the next
//     cycle (the poller's retry schedule), so a sustained failure rate
//     IS the stuck-outbox signal; the 09-table row's "outbox backlog"
//     (outbox backlog) is derived rather than gauged -- ingested-analytics
//     and delivered-succeeded growth deltas -- because an honest depth
//     gauge would need a COUNT over the whole outbox table every poll
//     cycle, a scan the batch-bounded claim query deliberately never
//     pays (dispatcher.go's claimPendingOutboxRecords). This
//     derivation is recorded rather than silently substituted.
//   - metering.aggregation.duration -- one aggregation call's
//     duration, channel-labeled analytics (Aggregator.Ingest, fed by
//     the AnalyticsRecorder flush loop) or outbox (IngestBillingGrade,
//     fed by the Dispatcher). Aggregation latency's honest proxy: the
//     pipeline aggregates in-process, so a call's own duration is the
//     queueing-plus-work time of one event.
const (
	eventsIngestedMetricName    = "metering.events.ingested"
	eventsDroppedMetricName     = "metering.events.dropped"
	outboxDeliveryMetricName    = "metering.outbox.delivery"
	aggregationDurationMetric   = "metering.aggregation.duration"
	aggregationChannelAttr      = "channel"
	aggregationChannelAnalytics = "analytics"
	aggregationChannelOutbox    = "outbox"
	outcomeAttr                 = "outcome"
	outcomeSucceeded            = "succeeded"
	outcomeFailed               = "failed"
)

// registerIngestDropMetrics wires metering.events.ingested and
// metering.events.dropped, owned by AnalyticsRecorder (its Record and
// drop paths are the only sites). Registration happens once per
// constructed component, with the registration error ignored the same
// way go/observability/middleware.go ignores it -- the global otel API
// returns a working no-op instrument alongside any error.
func registerIngestDropMetrics() (metric.Int64Counter, metric.Int64Counter) {
	meter := otel.Meter(InstrumentationName)
	ingested, _ := meter.Int64Counter(
		eventsIngestedMetricName,
		metric.WithDescription("Usage events the analytics-grade recorder accepted into its buffer, one per Record call that did not drop."),
		metric.WithUnit("{event}"),
	)
	dropped, _ := meter.Int64Counter(
		eventsDroppedMetricName,
		metric.WithDescription("Usage events the analytics-grade recorder dropped and counted: full buffer, Record after Stop, or a buffered event whose aggregation failed."),
		metric.WithUnit("{event}"),
	)
	return ingested, dropped
}

// registerAggregationDurationMetric wires metering.aggregation.duration,
// owned by the Aggregator (both ingest entry points are its own).
func registerAggregationDurationMetric() metric.Float64Histogram {
	meter := otel.Meter(InstrumentationName)
	duration, _ := meter.Float64Histogram(
		aggregationDurationMetric,
		metric.WithDescription("Duration of one aggregation call, in seconds, labeled by the channel that fed it (analytics or outbox)."),
		metric.WithUnit("s"),
	)
	return duration
}

// registerOutboxDeliveryMetric wires metering.outbox.delivery, owned
// by the Dispatcher (its poll loop is the only delivery site).
func registerOutboxDeliveryMetric() metric.Int64Counter {
	meter := otel.Meter(InstrumentationName)
	delivery, _ := meter.Int64Counter(
		outboxDeliveryMetricName,
		metric.WithDescription("Outbox delivery attempts by the Dispatcher poller, labeled by outcome: a failed attempt leaves the row pending for the next cycle."),
		metric.WithUnit("{attempt}"),
	)
	return delivery
}

// recordAggregationDuration records one aggregation call onto
// metering.aggregation.duration under the caller's channel. The guard
// mirrors authn's recordAuthMetric nil-checks: components constructed
// as bare struct literals in tests carry no instruments.
func recordAggregationDuration(
	ctx context.Context, aggDuration metric.Float64Histogram, channel string, start time.Time,
) {
	if aggDuration == nil {
		return
	}
	aggDuration.Record(ctx, time.Since(start).Seconds(),
		metric.WithAttributes(attribute.String(aggregationChannelAttr, channel)))
}
