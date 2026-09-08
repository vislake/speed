package alipay

import (
	"context"
	"encoding/json"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/vislake/speed/go/billing"
)

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

// TestVerifyWebhook_OutcomeMetricRecorded drives billing.webhook.verify
// through the real VerifyWebhook implementation and the real
// constructor (whose metric registration the test-only constructor
// skips): one signed genuine notification records succeeded, one
// unsigned body records failed, both under channel=alipay.
func TestVerifyWebhook_OutcomeMetricRecorded(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))

	_, alipayPubPEM, alipayPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, alipayPubPEM)
	gw, err := NewGateway(cfg)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	passback, err := json.Marshal(passbackPayload{TenantID: "tenant-a", SubscriptionID: "sub-1", InvoiceID: "inv-1"})
	if err != nil {
		t.Fatalf("marshal passback: %v", err)
	}
	body := signedNotifyBody(t, alipayPriv, map[string]string{
		"notify_id":       "notify_m1",
		"out_trade_no":    "ORD1",
		"trade_no":        "2026090422001",
		"trade_status":    "TRADE_SUCCESS",
		"total_amount":    "29.00",
		"passback_params": string(passback),
	})
	if _, err := gw.VerifyWebhook(context.Background(), nil, body); err != nil {
		t.Fatalf("VerifyWebhook (genuine): %v", err)
	}
	if _, err := gw.VerifyWebhook(context.Background(), nil, []byte("not a form body")); err == nil {
		t.Fatalf("VerifyWebhook (garbage body) succeeded, want the refusal")
	}

	verify := collectProviderMetric(t, reader, billing.WebhookVerifyMetricName)
	if got := providerCounterByAttr(t, verify, map[string]string{"channel": "alipay", "outcome": "succeeded"}); got != 1 {
		t.Errorf("billing.webhook.verify{channel=alipay,outcome=succeeded} = %d, want 1", got)
	}
	if got := providerCounterByAttr(t, verify, map[string]string{"channel": "alipay", "outcome": "failed"}); got != 1 {
		t.Errorf("billing.webhook.verify{channel=alipay,outcome=failed} = %d, want 1", got)
	}
}
