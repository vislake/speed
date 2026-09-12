package billing

import (
	"context"
	"errors"
	"testing"
)

// stubGateway is a minimal PaymentGateway proving wiring paths that only
// need a value (WithGateways' map, and similar) -- the real providers'
// behavior lives in go/billing/gateway/<provider>. Tests compare it by
// pointer identity, so it carries no fields.
type stubGateway struct{}

func (g *stubGateway) CreateCharge(context.Context, ChargeRequest) (ChargeHandle, error) {
	return ChargeHandle{}, errors.New("stubGateway: not implemented")
}

func (g *stubGateway) VerifyWebhook(context.Context, map[string][]string, []byte) (NormalizedEvent, error) {
	return NormalizedEvent{}, errors.New("stubGateway: not implemented")
}

func (g *stubGateway) QueryStatus(context.Context, ChannelReference) (ChannelStatus, Money, error) {
	return "", Money{}, errors.New("stubGateway: not implemented")
}

var _ PaymentGateway = (*stubGateway)(nil)

func TestChannelStatus_ClosedVocabulary(t *testing.T) {
	// A compile-time-adjacent sanity check that the four declared values
	// are all distinct strings -- a copy/paste duplicate here would make
	// two genuinely different outcomes indistinguishable to any caller
	// comparing ChannelStatus values.
	values := []ChannelStatus{
		ChannelStatusPending, ChannelStatusSucceeded, ChannelStatusFailed,
		ChannelStatusCanceled, ChannelStatusRefunded,
	}
	seen := make(map[ChannelStatus]bool, len(values))
	for _, v := range values {
		if seen[v] {
			t.Errorf("duplicate ChannelStatus value %q", v)
		}
		seen[v] = true
	}
}

func TestNormalizedEventType_ClosedVocabulary(t *testing.T) {
	values := []NormalizedEventType{
		NormalizedEventChargeSucceeded, NormalizedEventChargeFailed,
		NormalizedEventSubscriptionCanceled, NormalizedEventRefunded,
		NormalizedEventChargePending,
	}
	seen := make(map[NormalizedEventType]bool, len(values))
	for _, v := range values {
		if seen[v] {
			t.Errorf("duplicate NormalizedEventType value %q", v)
		}
		seen[v] = true
	}
}
