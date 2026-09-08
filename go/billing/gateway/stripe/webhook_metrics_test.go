package stripe

import (
	"context"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82/webhook"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/vislake/speed/go/billing"
)

// collectProviderMetric / providerCounterByAttr are this package's own
// copies of the billing-root test helpers: provider packages sit in
// their own test packages, so the helpers are duplicated per provider
// rather than exported from the root's test file.
func collectProviderMetric(t *testing.T, reader *sdkmetric.ManualReader, name string) metricdata.Metrics {
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

func providerCounterByAttr(t *testing.T, m metricdata.Metrics, attrs map[string]string) int64 {
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

// TestVerifyWebhook_OutcomeMetricRecorded drives the billing.webhook.verify
// counter through the real VerifyWebhook implementation: a genuinely
// signed delivery records succeeded, a refused one (missing signature
// header) records failed, both under the channel=stripe series. The
// metric registration happens in NewGateway, never in the test-only
// constructor -- this test therefore builds the gateway through the real
// constructor, whose webhook path needs no network.
func TestVerifyWebhook_OutcomeMetricRecorded(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))

	gw, err := NewGateway(testConfig())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	payload := checkoutSessionCompletedPayload(t, "evt_m1", "cs_m1", "tenant-a", "sub-1", "inv-1", 2900, "usd")
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload:   payload,
		Secret:    testWebhookSecret,
		Timestamp: time.Now(),
	})
	headers := map[string][]string{"Stripe-Signature": {signed.Header}}
	if _, err := gw.VerifyWebhook(context.Background(), headers, signed.Payload); err != nil {
		t.Fatalf("VerifyWebhook (genuine): %v", err)
	}
	if _, err := gw.VerifyWebhook(context.Background(), map[string][]string{}, payload); err == nil {
		t.Fatalf("VerifyWebhook (missing signature header) succeeded, want the refusal")
	}

	verify := collectProviderMetric(t, reader, billing.WebhookVerifyMetricName)
	if got := providerCounterByAttr(t, verify, map[string]string{"channel": "stripe", "outcome": "succeeded"}); got != 1 {
		t.Errorf("billing.webhook.verify{channel=stripe,outcome=succeeded} = %d, want 1", got)
	}
	if got := providerCounterByAttr(t, verify, map[string]string{"channel": "stripe", "outcome": "failed"}); got != 1 {
		t.Errorf("billing.webhook.verify{channel=stripe,outcome=failed} = %d, want 1", got)
	}
}
