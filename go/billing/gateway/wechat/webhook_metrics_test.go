package wechat

import (
	"context"
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
// header-less delivery records failed, both under channel=wechat.
func TestVerifyWebhook_OutcomeMetricRecorded(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))

	_, platformPubPEM, platformPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, platformPubPEM)
	gw, err := NewGateway(cfg)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	txn := testTransaction(t, "tenant-a", "sub-1", "inv-1", "ORD1", "SUCCESS", 2900)
	headers, body := signedNotifyBody(t, platformPriv, cfg.APIv3Key, eventTypeTransactionSuccess, txn)
	if _, err := gw.VerifyWebhook(context.Background(), headers, body); err != nil {
		t.Fatalf("VerifyWebhook (genuine): %v", err)
	}
	if _, err := gw.VerifyWebhook(context.Background(), map[string][]string{}, body); err == nil {
		t.Fatalf("VerifyWebhook (missing signature headers) succeeded, want the refusal")
	}

	verify := collectProviderMetric(t, reader, billing.WebhookVerifyMetricName)
	if got := providerCounterByAttr(t, verify, map[string]string{"channel": "wechat", "outcome": "succeeded"}); got != 1 {
		t.Errorf("billing.webhook.verify{channel=wechat,outcome=succeeded} = %d, want 1", got)
	}
	if got := providerCounterByAttr(t, verify, map[string]string{"channel": "wechat", "outcome": "failed"}); got != 1 {
		t.Errorf("billing.webhook.verify{channel=wechat,outcome=failed} = %d, want 1", got)
	}
}
