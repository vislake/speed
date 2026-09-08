package aigateway

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/vislake/speed/go/pkgcore"
)

// setupAIGatewayMetricsProvider installs a MeterProvider with a manual
// reader as the otel global, the same pattern the metering/billing
// metric tests use: Gateways constructed AFTER this call register their
// instruments onto it.
func setupAIGatewayMetricsProvider(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	return reader
}

func collectAIGatewayMetric(t *testing.T, reader *sdkmetric.ManualReader, name string) metricdata.Metrics {
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

func gatewayCounterByAttr(t *testing.T, m metricdata.Metrics, attrs map[string]string) int64 {
	t.Helper()
	// An instrument with no recorded points is absent from the
	// collection -- for a counter that IS zero, the honest answer.
	if m.Name == "" {
		return 0
	}
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

func gatewayHistogramCount(t *testing.T, m metricdata.Metrics) uint64 {
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

func TestGateway_Chat_CallMetricsRecordedPerProvider(t *testing.T) {
	reader := setupAIGatewayMetricsProvider(t)
	provider := &fakeChatProvider{chatResp: ChatResponse{Message: ChatMessage{Role: RoleAssistant, Content: "ok"}, Usage: Usage{PromptTokens: 1, CompletionTokens: 1}}}
	g := gatewayTestFixture(t, provider)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := g.Chat(ctx, chatReq()); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	// A refused call that never reaches the provider (unrouted model)
	// must not count as a provider invocation.
	if _, err := g.Chat(ctx, ChatRequest{Model: "chat:unrouted", Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}}}); err == nil {
		t.Fatalf("Chat with an unrouted model succeeded, want the refusal")
	}

	calls := collectAIGatewayMetric(t, reader, providerCallsMetricName)
	want := map[string]string{providerAttr: fakeProviderName, kindAttr: providerCallChat}
	if got := gatewayCounterByAttr(t, calls, want); got != 1 {
		t.Errorf("aigateway.provider.calls{provider=chat.fake-test-provider,kind=chat} = %d, want 1", got)
	}
	errors := collectAIGatewayMetric(t, reader, providerErrorsMetricName)
	if got := gatewayCounterByAttr(t, errors, want); got != 0 {
		t.Errorf("aigateway.provider.errors{provider=chat.fake-test-provider,kind=chat} = %d, want 0", got)
	}
	duration := collectAIGatewayMetric(t, reader, providerDurationMetricName)
	if got := gatewayHistogramCount(t, duration); got != 1 {
		t.Errorf("aigateway.provider.duration count = %d, want 1", got)
	}
}

func TestGateway_Chat_ProviderErrorCountsAgainstTheProvider(t *testing.T) {
	reader := setupAIGatewayMetricsProvider(t)
	provider := &fakeChatProvider{chatErr: errors.New("vendor down")}
	g := gatewayTestFixture(t, provider)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := g.Chat(ctx, chatReq()); err == nil {
		t.Fatalf("Chat succeeded, want the provider error")
	}

	errors := collectAIGatewayMetric(t, reader, providerErrorsMetricName)
	want := map[string]string{providerAttr: fakeProviderName, kindAttr: providerCallChat}
	if got := gatewayCounterByAttr(t, errors, want); got != 1 {
		t.Errorf("aigateway.provider.errors{provider=chat.fake-test-provider,kind=chat} = %d, want 1", got)
	}
}

func TestGateway_ChatStream_TerminalErrorCountsAgainstTheProvider(t *testing.T) {
	reader := setupAIGatewayMetricsProvider(t)
	provider := &fakeChatProvider{streamOut: []ChatChunk{{Err: errors.New("stream died")}}}
	g := gatewayTestFixture(t, provider)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	ch, err := g.ChatStream(ctx, chatReq())
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	for chunk := range ch {
		if chunk.Err == nil {
			t.Fatalf("stream delivered a non-terminal chunk, want the terminal error")
		}
	}

	// The terminal-error chunk's Err is consumed by relayStream AFTER
	// the chunk is forwarded, so collect only once the channel closed
	// (the range above already did).
	errors := collectAIGatewayMetric(t, reader, providerErrorsMetricName)
	want := map[string]string{providerAttr: fakeProviderName, kindAttr: providerCallChatStream}
	if got := gatewayCounterByAttr(t, errors, want); got != 1 {
		t.Errorf("aigateway.provider.errors{provider=chat.fake-test-provider,kind=chat_stream} = %d, want 1", got)
	}
}

func TestGateway_Chat_RateLimitHitCounts(t *testing.T) {
	reader := setupAIGatewayMetricsProvider(t)
	provider := &fakeChatProvider{chatResp: ChatResponse{Message: ChatMessage{Role: RoleAssistant, Content: "ok"}, Usage: Usage{PromptTokens: 1, CompletionTokens: 1}}}
	g := gatewayTestFixture(t, provider)
	g.limiter = scriptedLimiter{allowed: false}
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if _, err := g.Chat(ctx, chatReq()); err == nil {
		t.Fatalf("Chat succeeded, want ErrRateLimited")
	}

	rateLimited := collectAIGatewayMetric(t, reader, rateLimitedMetricName)
	if got := gatewayCounterByAttr(t, rateLimited, map[string]string{}); got != 1 {
		t.Errorf("aigateway.rate_limited = %d, want 1", got)
	}
	// A refused call never reaches the provider.
	calls := collectAIGatewayMetric(t, reader, providerCallsMetricName)
	if got := gatewayCounterByAttr(t, calls, map[string]string{providerAttr: fakeProviderName}); got != 0 {
		t.Errorf("aigateway.provider.calls after a rate-limit refusal = %d, want 0", got)
	}
}
