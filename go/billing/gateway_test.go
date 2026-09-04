package billing

import (
	"context"
	"errors"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// fakeRegistryGateway is a minimal PaymentGateway used only to prove
// PaymentGatewayRegistry's own Register/Build round trip -- the real
// providers (stripe/alipay/wechat) each carry their own, much fuller test
// suite in go/billing/gateway/<provider>.
type fakeRegistryGateway struct{ name string }

func (g *fakeRegistryGateway) CreateCharge(context.Context, ChargeRequest) (ChargeHandle, error) {
	return ChargeHandle{}, errors.New("fakeRegistryGateway: not implemented")
}

func (g *fakeRegistryGateway) VerifyWebhook(context.Context, map[string][]string, []byte) (NormalizedEvent, error) {
	return NormalizedEvent{}, errors.New("fakeRegistryGateway: not implemented")
}

func (g *fakeRegistryGateway) QueryStatus(context.Context, ChannelReference) (ChannelStatus, Money, error) {
	return "", Money{}, errors.New("fakeRegistryGateway: not implemented")
}

var _ PaymentGateway = (*fakeRegistryGateway)(nil)

// TestPaymentGatewayRegistry_RegisterAndBuild proves the registry's own
// Register/Build round trip, mirroring go/pki's identical proof for
// SignerRegistry (signer_registry_test.go) -- a name registered here is
// buildable by that exact name, with the capability the registration
// declared echoed back.
func TestPaymentGatewayRegistry_RegisterAndBuild(t *testing.T) {
	const name = "gateway.test-fake-for-registry-round-trip"
	err := PaymentGatewayRegistry.Register(pkgcore.Registration[PaymentGateway]{
		Name:         name,
		Capabilities: 0,
		New: func(cfg pkgcore.Config) (PaymentGateway, error) {
			return &fakeRegistryGateway{name: cfg["name"]}, nil
		},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	gw, caps, err := PaymentGatewayRegistry.Build(name, pkgcore.Config{"name": "probe"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if gw == nil {
		t.Fatal("Build returned a nil PaymentGateway")
	}
	if caps != 0 {
		t.Errorf("Build capabilities = %v, want none", caps)
	}
	fake, ok := gw.(*fakeRegistryGateway)
	if !ok || fake.name != "probe" {
		t.Errorf("Build returned %+v, want a *fakeRegistryGateway carrying the given Config", gw)
	}
}

func TestPaymentGatewayRegistry_Build_UnknownName(t *testing.T) {
	_, _, err := PaymentGatewayRegistry.Build("gateway.does-not-exist", pkgcore.Config{})
	if !errors.Is(err, pkgcore.ErrUnknownImplementation) {
		t.Errorf("Build(unknown name) error = %v, want it to wrap pkgcore.ErrUnknownImplementation", err)
	}
}

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
	}
	seen := make(map[NormalizedEventType]bool, len(values))
	for _, v := range values {
		if seen[v] {
			t.Errorf("duplicate NormalizedEventType value %q", v)
		}
		seen[v] = true
	}
}
