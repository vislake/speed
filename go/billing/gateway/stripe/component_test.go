package stripe

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"

	"github.com/vislake/speed/go/billing"
)

// stripeComponentSettings returns the settings both resolution paths are
// fed from: the component's composition block and the seam registration's
// flat pkgcore.Config carry the identical key set and values, so a setting
// either face grows alone cannot pass unnoticed.
func stripeComponentSettings() pkgcore.Config {
	return pkgcore.Config{
		"api_key":        "sk_test_component",
		"webhook_secret": testWebhookSecret,
		"success_url":    "https://shop.example.test/checkout/success",
		"cancel_url":     "https://shop.example.test/checkout/cancel",
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
// component package's suite asserts: the naming convention, the module
// relation, the schema's decodability and the token shapes.
func TestComponentWellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, stripeGatewayComponent)
}

// TestComponentAssemblesThroughRegistry drives the descriptor through the
// assembly's stages the way a host would: selection from a composition
// configuration, construction, and the product read back at the contract
// type consumers use.
func TestComponentAssemblesThroughRegistry(t *testing.T) {
	ctx := context.Background()
	reg := pkgcore.NewComponentRegistry()
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{"gateway.stripe": componentBlock(stripeComponentSettings())},
	}))
	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare() error = %v, want gateway.stripe selected and validated", err)
	}
	if err := reg.Construct(ctx); err != nil {
		t.Fatalf("Construct() error = %v, want gateway.stripe constructed", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })

	gateway, err := pkgcore.Get[billing.PaymentGateway](reg)
	if err != nil {
		t.Fatalf("Get[billing.PaymentGateway] error = %v, want the component's product", err)
	}
	if _, ok := gateway.(*Gateway); !ok {
		t.Errorf("Get[billing.PaymentGateway] = %T, want *stripe.Gateway", gateway)
	}
}

// TestComponentAndSeamRegistrationAgree pins the coexistence of the two
// resolution paths under one name: both build the same implementation from
// the same settings, both declare the same capabilities, and both refuse an
// empty configuration the same way -- the seam path at Build, the component
// path at Construct.
func TestComponentAndSeamRegistrationAgree(t *testing.T) {
	ctx := context.Background()
	settings := stripeComponentSettings()

	seamGateway, seamCaps, err := billing.PaymentGatewayRegistry.Build("gateway.stripe", settings)
	if err != nil {
		t.Fatalf("PaymentGatewayRegistry.Build(gateway.stripe) error = %v", err)
	}

	reg := pkgcore.NewComponentRegistry()
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{"gateway.stripe": componentBlock(settings)},
	}))
	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := reg.Construct(ctx); err != nil {
		t.Fatalf("Construct() error = %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })

	componentGateway, err := pkgcore.Get[billing.PaymentGateway](reg)
	if err != nil {
		t.Fatalf("Get[billing.PaymentGateway] error = %v", err)
	}
	if reflect.TypeOf(componentGateway) != reflect.TypeOf(seamGateway) {
		t.Errorf("component built %T, seam registration built %T; the two faces must resolve the same implementation", componentGateway, seamGateway)
	}

	caps, err := pkgcore.ComponentCapabilities(reg, "gateway.stripe")
	if err != nil || caps != seamCaps {
		t.Errorf("ComponentCapabilities(gateway.stripe) = (%v, %v), want the seam registration's %v", caps, err, seamCaps)
	}

	if _, _, err := billing.PaymentGatewayRegistry.Build("gateway.stripe", pkgcore.Config{}); err == nil || !strings.Contains(err.Error(), "Config.APIKey") {
		t.Errorf("seam Build with an empty Config = %v, want NewGateway's required-field error", err)
	}

	emptyBlockReg := pkgcore.NewComponentRegistry()
	emptyBlockReg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{"gateway.stripe": nil},
	}))
	if err := emptyBlockReg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := emptyBlockReg.Construct(ctx); err == nil || !strings.Contains(err.Error(), "Config.APIKey") {
		t.Errorf("Construct with an empty block = %v, want NewGateway's required-field error", err)
	}
	_ = emptyBlockReg.Close(context.Background())
}
