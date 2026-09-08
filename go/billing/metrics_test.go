package billing

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/vislake/speed/go/pkgcore"
)

// setupBillingMetricsProvider installs a MeterProvider with a manual
// reader as the otel global, the same pattern authn's and metering's
// metric tests use: components constructed AFTER this call register
// their instruments onto it.
func setupBillingMetricsProvider(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	return reader
}

func collectBillingMetric(t *testing.T, reader *sdkmetric.ManualReader, name string) metricdata.Metrics {
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

func counterByAttr(t *testing.T, m metricdata.Metrics, attrs map[string]string) int64 {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %q Data = %T, want metricdata.Sum[int64]", m.Name, m.Data)
	}
	for _, point := range sum.DataPoints {
		pointAttrs := map[string]string{}
		for _, kv := range point.Attributes.ToSlice() {
			pointAttrs[string(kv.Key)] = kv.Value.AsString()
		}
		match := true
		for key, value := range attrs {
			if pointAttrs[key] != value {
				match = false
				break
			}
		}
		if match {
			return point.Value
		}
	}
	return 0
}

func histogramCount(t *testing.T, m metricdata.Metrics) uint64 {
	t.Helper()
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("metric %q Data = %T, want metricdata.Histogram[float64]", m.Name, m.Data)
	}
	var total uint64
	for _, point := range hist.DataPoints {
		total += point.Count
	}
	return total
}

func TestInvoiceRepository_TransitionAndDwellMetricsRecorded(t *testing.T) {
	reader := setupBillingMetricsProvider(t)
	repo := NewInvoiceRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	paid, err := repo.CreateInvoice(ctx, CreateInvoiceInput{
		SubscriptionID: "sub-1", Amount: Money{Cents: 4900, Currency: "USD"},
	})
	if err != nil {
		t.Fatalf("CreateInvoice: %v", err)
	}
	if _, markErr := repo.MarkPaid(ctx, paid.ID); markErr != nil {
		t.Fatalf("MarkPaid: %v", err)
	}
	voided, err := repo.CreateInvoice(ctx, CreateInvoiceInput{
		SubscriptionID: "sub-1", Amount: Money{Cents: 100, Currency: "USD"},
	})
	if err != nil {
		t.Fatalf("CreateInvoice: %v", err)
	}
	if _, voidErr := repo.Void(ctx, voided.ID); voidErr != nil {
		t.Fatalf("Void: %v", err)
	}
	// A terminal status is never rewritten; the refusal must not count.
	if _, voidErr := repo.Void(ctx, paid.ID); voidErr == nil {
		t.Fatalf("Void of a paid invoice succeeded, want the refusal")
	}

	transition := collectBillingMetric(t, reader, invoiceTransitionMetricName)
	if got := counterByAttr(t, transition, map[string]string{fromAttr: "open", toAttr: "paid"}); got != 1 {
		t.Errorf("billing.invoice.transition{from=open,to=paid} = %d, want 1", got)
	}
	if got := counterByAttr(t, transition, map[string]string{fromAttr: "open", toAttr: "void"}); got != 1 {
		t.Errorf("billing.invoice.transition{from=open,to=void} = %d, want 1", got)
	}
	// No transition ever landed on a terminal source; refused moves count
	// nothing.
	if got := counterByAttr(t, transition, map[string]string{fromAttr: "paid"}); got != 0 {
		t.Errorf("billing.invoice.transition with a terminal from = %d, want 0", got)
	}

	dwell := collectBillingMetric(t, reader, invoiceOpenDwellMetricName)
	if got := histogramCount(t, dwell); got != 2 {
		t.Errorf("billing.invoice.open_dwell count = %d, want 2 (open -> paid and open -> void)", got)
	}
}

func TestRegisterWebhookVerifyMetric_RecordsBothOutcomes(t *testing.T) {
	reader := setupBillingMetricsProvider(t)
	counter := RegisterWebhookVerifyMetric("stripe")
	ctx := context.Background()

	RecordWebhookVerify(ctx, counter, "stripe", nil)
	RecordWebhookVerify(ctx, counter, "stripe", errors.New("billing.webhook_signature_invalid"))
	RecordWebhookVerify(ctx, counter, "stripe", nil)
	// A second channel on the shared counter must stay its own series.
	RecordWebhookVerify(ctx, counter, "alipay", errors.New("billing.webhook_payload_unrecognized"))

	verify := collectBillingMetric(t, reader, WebhookVerifyMetricName)
	if got := counterByAttr(t, verify, map[string]string{channelAttr: "stripe", outcomeAttr: outcomeSucceeded}); got != 2 {
		t.Errorf("billing.webhook.verify{channel=stripe,outcome=succeeded} = %d, want 2", got)
	}
	if got := counterByAttr(t, verify, map[string]string{channelAttr: "stripe", outcomeAttr: outcomeFailed}); got != 1 {
		t.Errorf("billing.webhook.verify{channel=stripe,outcome=failed} = %d, want 1", got)
	}
	if got := counterByAttr(t, verify, map[string]string{channelAttr: "alipay", outcomeAttr: outcomeFailed}); got != 1 {
		t.Errorf("billing.webhook.verify{channel=alipay,outcome=failed} = %d, want 1", got)
	}
}

// TestRegisterWebhookVerifyMetric_NilCounterIsANoOp pins the guard every
// bare-struct-literal gateway relies on: recording with a nil counter
// must not panic (the test-only gateway constructors never register).
func TestRegisterWebhookVerifyMetric_NilCounterIsANoOp(t *testing.T) {
	RecordWebhookVerify(context.Background(), nil, "stripe", errors.New("x"))
}
