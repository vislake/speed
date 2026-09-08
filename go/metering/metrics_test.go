package metering

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// setupMeteringMetricsProvider installs a MeterProvider with a manual
// reader as the otel global, exactly as authn's setupAuthMetricsMeterProvider
// does for its own metric tests: components constructed AFTER this call
// register their instruments onto it, and collectMeteringMetric reads
// them back after a reader.Collect.
func setupMeteringMetricsProvider(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	return reader
}

func collectMeteringMetric(t *testing.T, reader *sdkmetric.ManualReader, name string) metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	return metricdata.Metrics{}
}

func metricCounterTotal(t *testing.T, m metricdata.Metrics) int64 {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %q Data = %T, want metricdata.Sum[int64]", m.Name, m.Data)
	}
	var total int64
	for _, point := range sum.DataPoints {
		total += point.Value
	}
	return total
}

func metricCounterTotalByAttr(t *testing.T, m metricdata.Metrics, key, value string) int64 {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %q Data = %T, want metricdata.Sum[int64]", m.Name, m.Data)
	}
	for _, point := range sum.DataPoints {
		attrs := point.Attributes.ToSlice()
		for _, kv := range attrs {
			if string(kv.Key) == key && kv.Value.AsString() == value {
				return point.Value
			}
		}
	}
	return 0
}

func metricHistogramCountByAttr(t *testing.T, m metricdata.Metrics, key, value string) uint64 {
	t.Helper()
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("metric %q Data = %T, want metricdata.Histogram[float64]", m.Name, m.Data)
	}
	for _, point := range hist.DataPoints {
		attrs := point.Attributes.ToSlice()
		for _, kv := range attrs {
			if string(kv.Key) == key && kv.Value.AsString() == value {
				return point.Count
			}
		}
	}
	return 0
}

func TestAnalyticsRecorder_IngestAndDropMetricsRecorded(t *testing.T) {
	reader := setupMeteringMetricsProvider(t)
	// No aggregator and no Start: Record's enqueue path does not touch
	// the aggregator, and the overflow drop path only needs a full
	// buffer (capacity 1024).
	rec := NewAnalyticsRecorder(nil)
	ctx := context.Background()

	event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: "idem-ok", OccurredAt: time.Now()}
	for i := 0; i < defaultAnalyticsBufferSize; i++ {
		if err := rec.Record(ctx, event); err != nil {
			t.Fatalf("Record(%d): %v", i, err)
		}
	}
	// The buffer is full now: one more Record drops and counts.
	if err := rec.Record(ctx, event); err != nil {
		t.Fatalf("Record over capacity: %v", err)
	}

	ingested := collectMeteringMetric(t, reader, eventsIngestedMetricName)
	if got := metricCounterTotal(t, ingested); got != defaultAnalyticsBufferSize {
		t.Errorf("metering.events.ingested = %d, want %d", got, defaultAnalyticsBufferSize)
	}
	dropped := collectMeteringMetric(t, reader, eventsDroppedMetricName)
	if got := metricCounterTotal(t, dropped); got != 1 {
		t.Errorf("metering.events.dropped = %d, want 1", got)
	}
}

func TestAggregator_DurationHistogramRecordedForBothChannels(t *testing.T) {
	reader := setupMeteringMetricsProvider(t)
	db := newTestDB(t)
	agg := NewAggregator(NewSummaryRepository(db))
	ctx := context.Background()
	at := time.Now()

	event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 2, IdempotencyKey: "idem-a", OccurredAt: at}
	if err := agg.Ingest(ctx, event); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	event2 := UsageEvent{TenantID: "tenant-b", Feature: "ai.generation", Quantity: 3, IdempotencyKey: "idem-b", OccurredAt: at}
	if err := agg.IngestBillingGrade(ctx, event2); err != nil {
		t.Fatalf("IngestBillingGrade: %v", err)
	}

	duration := collectMeteringMetric(t, reader, aggregationDurationMetric)
	if got := metricHistogramCountByAttr(t, duration, aggregationChannelAttr, aggregationChannelAnalytics); got != 1 {
		t.Errorf("aggregation.duration{channel=analytics} count = %d, want 1", got)
	}
	if got := metricHistogramCountByAttr(t, duration, aggregationChannelAttr, aggregationChannelOutbox); got != 1 {
		t.Errorf("aggregation.duration{channel=outbox} count = %d, want 1", got)
	}
}

func TestDispatcher_DeliveryOutcomeMetricsRecorded(t *testing.T) {
	reader := setupMeteringMetricsProvider(t)
	d, _, db := newTestDispatcher(t)
	ctx := context.Background()

	event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 3, IdempotencyKey: "idem-1", OccurredAt: time.Now()}
	if _, err := Enqueue(ctx, db, event); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	delivered, err := d.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1", delivered)
	}

	delivery := collectMeteringMetric(t, reader, outboxDeliveryMetricName)
	if got := metricCounterTotalByAttr(t, delivery, outcomeAttr, outcomeSucceeded); got != 1 {
		t.Errorf("metering.outbox.delivery{outcome=succeeded} = %d, want 1", got)
	}
	if got := metricCounterTotalByAttr(t, delivery, outcomeAttr, outcomeFailed); got != 0 {
		t.Errorf("metering.outbox.delivery{outcome=failed} = %d, want 0", got)
	}
}
