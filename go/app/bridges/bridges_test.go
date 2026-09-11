package bridges

import (
	"context"
	"errors"
	"testing"

	aigateway "github.com/vislake/speed/go/ai-gateway"
	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/metering"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// stubEntitlements is a billing.Entitlements stub: it answers one fixed
// error for every check.
type stubEntitlements struct{ err error }

func (s stubEntitlements) Check(context.Context, string, int64) (billing.Decision, error) {
	return billing.Decision{}, s.err
}

// capturingRecorder is a metering.Recorder stand-in recording the events
// the bridge hands it.
type capturingRecorder struct {
	events []metering.UsageEvent
	err    error
}

func (r *capturingRecorder) Record(_ context.Context, event metering.UsageEvent) error {
	r.events = append(r.events, event)
	return r.err
}

// TestEntitlements_PropagatesTheCheckError pins the adapter's error half: a
// check failure crosses the seam as itself, never as a synthesized decision.
func TestEntitlements_PropagatesTheCheckError(t *testing.T) {
	sentinel := errors.New("bridges test: the check refused")
	check := Entitlements(stubEntitlements{err: sentinel})
	if _, err := check(context.Background(), "model:smile", 1); !errors.Is(err, sentinel) {
		t.Fatalf("Entitlements check error = %v, want the underlying refusal", err)
	}
}

// TestUsageRecorder_MapsEveryFieldAcrossTheSeam pins the closure's whole
// job: each ai-gateway event field reaches the metering event field it
// belongs to.
func TestUsageRecorder_MapsEveryFieldAcrossTheSeam(t *testing.T) {
	recorder := &capturingRecorder{}
	bridge := UsageRecorder(recorder)

	in := aigateway.UsageEvent{
		TenantID:       "tenant-1",
		Feature:        "ai.chat_tokens",
		Quantity:       42,
		IdempotencyKey: "call-1",
		Metadata:       map[string]string{"model": "gpt-4o-mini"},
	}
	if err := bridge.Record(context.Background(), in); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(recorder.events) != 1 {
		t.Fatalf("the recorder received %d events, want 1", len(recorder.events))
	}
	got := recorder.events[0]
	if got.TenantID != in.TenantID || got.Feature != in.Feature || got.Quantity != in.Quantity ||
		got.IdempotencyKey != in.IdempotencyKey {
		t.Fatalf("mapped event %+v does not match the input %+v", got, in)
	}
	if len(got.Metadata) != len(in.Metadata) || got.Metadata["model"] != in.Metadata["model"] {
		t.Fatalf("mapped event metadata %+v does not match the input %+v", got.Metadata, in.Metadata)
	}
}

// TestUsageRecorder_PropagatesTheRecordersError pins that the bridge adds
// no policy of its own: the recorder's answer is the bridge's answer.
func TestUsageRecorder_PropagatesTheRecordersError(t *testing.T) {
	wantErr := errors.New("recorder down")
	bridge := UsageRecorder(&capturingRecorder{err: wantErr})
	if err := bridge.Record(context.Background(), aigateway.UsageEvent{}); !errors.Is(err, wantErr) {
		t.Fatalf("Record returned %v, want the recorder's own %v", err, wantErr)
	}
}

// TestFeatureGates_FailClosedWithoutAnAttachedConfigService pins the
// gates' pre-Attach direction: both adapters read through the config
// module's lazy handle, and a read before Attach reports the config
// module's own not-attached refusal -- never a panic and never a
// fabricated answer. (The post-Attach read is config's own tested
// contract; these adapters delegate to it unchanged.)
func TestFeatureGates_FailClosedWithoutAnAttachedConfigService(t *testing.T) {
	var handle *config.Handle
	if enabled, err := OrgFeatureGate(handle).IsEnabled(context.Background(), "org.something"); err == nil || enabled {
		t.Fatalf("OrgFeatureGate on a nil handle answered (%v, %v), want a not-attached error", enabled, err)
	} else if !apperr.HasCode(err, config.ErrServiceNotAttached.Code) {
		t.Fatalf("OrgFeatureGate error %v does not carry config.ErrServiceNotAttached", err)
	}
	if enabled, err := AuthnFeatureGate(handle).IsEnabled(context.Background(), "authn.something"); err == nil || enabled {
		t.Fatalf("AuthnFeatureGate on a nil handle answered (%v, %v), want a not-attached error", enabled, err)
	} else if !apperr.HasCode(err, config.ErrServiceNotAttached.Code) {
		t.Fatalf("AuthnFeatureGate error %v does not carry config.ErrServiceNotAttached", err)
	}
}
