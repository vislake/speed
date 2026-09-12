package alipay

import (
	"context"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"

	"github.com/vislake/speed/go/billing"
)

// alipayComponentSettings returns the settings the component's composition
// block carries: the same key set and values the flat construction path
// reads, so a setting one shape grows alone cannot pass unnoticed.
func alipayComponentSettings(t *testing.T) pkgcore.Config {
	t.Helper()
	privPEM, pubPEM, _ := generateTestKeyPair(t)
	return pkgcore.Config{
		"app_id":                "2021000000000000",
		"private_key_pem":       string(privPEM),
		"alipay_public_key_pem": string(pubPEM),
		"notify_url":            "https://shop.example.test/webhooks/alipay",
	}
}

// componentBlock converts a flat Config into the map shape a composition
// block carries: the same keys, the same values.
func componentBlock(cfg pkgcore.Config) map[string]any {
	block := make(map[string]any, len(cfg))
	for key, value := range cfg {
		block[key] = value
	}
	return block
}

// TestComponentWellFormed runs the component descriptor contract every
// component package's suite asserts.
func TestComponentWellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, alipayGatewayComponent)
}

// TestComponentAssemblesThroughRegistry drives the descriptor through the
// assembly's stages the way a host would: selection from a composition
// configuration, construction, and the product read back at the contract
// type consumers use.
func TestComponentAssemblesThroughRegistry(t *testing.T) {
	ctx := context.Background()
	reg := pkgcore.NewComponentRegistry()
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{"gateway.alipay": componentBlock(alipayComponentSettings(t))},
	}))
	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare() error = %v, want gateway.alipay selected and validated", err)
	}
	if err := reg.Construct(ctx); err != nil {
		t.Fatalf("Construct() error = %v, want gateway.alipay constructed", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })

	gateway, err := pkgcore.Get[billing.PaymentGateway](reg)
	if err != nil {
		t.Fatalf("Get[billing.PaymentGateway] error = %v, want the component's product", err)
	}
	if _, ok := gateway.(*Gateway); !ok {
		t.Errorf("Get[billing.PaymentGateway] = %T, want *alipay.Gateway", gateway)
	}
}

// TestComponentConstructsAndRefusesEmptyConfig pins the component's one
// construction path under its own name: gateway.alipay builds the package's
// own gateway from its settings, declares the zero capability, and refuses
// an empty configuration with NewGateway's required-field error.
func TestComponentConstructsAndRefusesEmptyConfig(t *testing.T) {
	ctx := context.Background()
	settings := alipayComponentSettings(t)

	reg := pkgcore.NewComponentRegistry()
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{"gateway.alipay": componentBlock(settings)},
	}))
	if prepareErr := reg.Prepare(ctx); prepareErr != nil {
		t.Fatalf("Prepare() error = %v", prepareErr)
	}
	if constructErr := reg.Construct(ctx); constructErr != nil {
		t.Fatalf("Construct() error = %v", constructErr)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })

	componentGateway, err := pkgcore.Get[billing.PaymentGateway](reg)
	if err != nil {
		t.Fatalf("Get[billing.PaymentGateway] error = %v", err)
	}
	if _, ok := componentGateway.(*Gateway); !ok {
		t.Errorf("Get[billing.PaymentGateway] = %T, want the package's own *alipay.Gateway", componentGateway)
	}

	caps, err := pkgcore.ComponentCapabilities(reg, "gateway.alipay")
	if err != nil || caps != 0 {
		t.Errorf("ComponentCapabilities(gateway.alipay) = (%v, %v), want the declared zero capability", caps, err)
	}

	emptyBlockReg := pkgcore.NewComponentRegistry()
	emptyBlockReg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{"gateway.alipay": nil},
	}))
	if err := emptyBlockReg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := emptyBlockReg.Construct(ctx); err == nil || !strings.Contains(err.Error(), "Config.AppID") {
		t.Errorf("Construct with an empty block = %v, want NewGateway's required-field error", err)
	}
	_ = emptyBlockReg.Close(context.Background())
}
