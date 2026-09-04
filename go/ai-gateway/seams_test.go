package aigateway

import (
	"context"
	"errors"
	"testing"
)

func TestEntitlementsFunc_Check_CallsUnderlyingFunc(t *testing.T) {
	var gotFeature string
	var gotRequested int64
	f := EntitlementsFunc(func(_ context.Context, featureKey string, requested int64) (Decision, error) {
		gotFeature = featureKey
		gotRequested = requested
		return Decision{Allowed: true, Reason: "ok"}, nil
	})

	decision, err := f.Check(context.Background(), "model:chat:default", 1)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if gotFeature != "model:chat:default" || gotRequested != 1 {
		t.Fatalf("Check forwarded (%q, %d), want (%q, %d)", gotFeature, gotRequested, "model:chat:default", 1)
	}
	if !decision.Allowed || decision.Reason != "ok" {
		t.Fatalf("Check returned %+v, want the underlying func's Decision", decision)
	}
}

func TestEntitlementsFunc_Check_ForwardsError(t *testing.T) {
	wantErr := errors.New("boom")
	f := EntitlementsFunc(func(context.Context, string, int64) (Decision, error) { return Decision{}, wantErr })
	if _, err := f.Check(context.Background(), "model:chat:default", 1); !errors.Is(err, wantErr) {
		t.Fatalf("Check err = %v, want %v", err, wantErr)
	}
}

func TestUsageRecorderFunc_Record_CallsUnderlyingFunc(t *testing.T) {
	var got UsageEvent
	f := UsageRecorderFunc(func(_ context.Context, event UsageEvent) error {
		got = event
		return nil
	})

	event := UsageEvent{TenantID: "tenant-acme", Feature: usageFeatureChatTokens, Quantity: 11}
	if err := f.Record(context.Background(), event); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// UsageEvent carries a map field (Metadata), so it is not a comparable
	// type -- compare the scalar fields this test actually set.
	if got.TenantID != event.TenantID || got.Feature != event.Feature || got.Quantity != event.Quantity {
		t.Fatalf("Record forwarded %+v, want %+v", got, event)
	}
}

func TestUsageRecorderFunc_Record_ForwardsError(t *testing.T) {
	wantErr := errors.New("boom")
	f := UsageRecorderFunc(func(context.Context, UsageEvent) error { return wantErr })
	if err := f.Record(context.Background(), UsageEvent{}); !errors.Is(err, wantErr) {
		t.Fatalf("Record err = %v, want %v", err, wantErr)
	}
}
