package wechat

import (
	"context"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"

	"github.com/vislake/speed/go/billing"
)

// wechatComponentSettings returns the settings the component's composition
// block carries: the same key set and values the flat construction path
// reads, so a setting one shape grows alone cannot pass unnoticed.
func wechatComponentSettings(t *testing.T) pkgcore.Config {
	t.Helper()
	privPEM, pubPEM, _ := generateTestKeyPair(t)
	return pkgcore.Config{
		"mch_id":                  "1900000001",
		"app_id":                  "wx0000000000000000",
		"mch_cert_serial_no":      "0123456789ABCDEF",
		"mch_private_key_pem":     string(privPEM),
		"api_v3_key":              string(testAPIv3Key()),
		"platform_public_key_pem": string(pubPEM),
		"notify_url":              "https://shop.example.test/webhooks/wechat",
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
	componenttest.AssertWellFormed(t, wechatGatewayComponent)
}

// TestComponentAssemblesThroughRegistry drives the descriptor through the
// assembly's stages the way a host would: selection from a composition
// configuration, construction, and the member read back by name at the
// contract type consumers use.
func TestComponentAssemblesThroughRegistry(t *testing.T) {
	ctx := context.Background()
	reg := pkgcore.NewComponentRegistry()
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{"gateway.wechat": componentBlock(wechatComponentSettings(t))},
	}))
	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare() error = %v, want gateway.wechat selected and validated", err)
	}
	if err := reg.Construct(ctx); err != nil {
		t.Fatalf("Construct() error = %v, want gateway.wechat constructed", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })

	members := pkgcore.Members[billing.PaymentGateway](reg)
	if len(members) != 1 || members[0].Name != "gateway.wechat" {
		t.Fatalf("Members[billing.PaymentGateway] = %+v, want exactly the gateway.wechat member", members)
	}
	if _, ok := members[0].Value.(*Gateway); !ok {
		t.Errorf("Members[billing.PaymentGateway] = %T, want *wechat.Gateway", members[0].Value)
	}
}

// TestComponentConstructsAndRefusesEmptyConfig pins the component's one
// construction path under its own name: gateway.wechat builds the package's
// own gateway from its settings, declares the zero capability, and refuses
// an empty configuration with NewGateway's required-field error.
func TestComponentConstructsAndRefusesEmptyConfig(t *testing.T) {
	ctx := context.Background()
	settings := wechatComponentSettings(t)

	reg := pkgcore.NewComponentRegistry()
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{"gateway.wechat": componentBlock(settings)},
	}))
	if prepareErr := reg.Prepare(ctx); prepareErr != nil {
		t.Fatalf("Prepare() error = %v", prepareErr)
	}
	if constructErr := reg.Construct(ctx); constructErr != nil {
		t.Fatalf("Construct() error = %v", constructErr)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })

	members := pkgcore.Members[billing.PaymentGateway](reg)
	if len(members) != 1 {
		t.Fatalf("Members[billing.PaymentGateway] = %+v, want the single gateway.wechat member", members)
	}
	if _, ok := members[0].Value.(*Gateway); !ok {
		t.Errorf("Members[billing.PaymentGateway] = %T, want the package's own *wechat.Gateway", members[0].Value)
	}

	caps, err := pkgcore.ComponentCapabilities(reg, "gateway.wechat")
	if err != nil || caps != 0 {
		t.Errorf("ComponentCapabilities(gateway.wechat) = (%v, %v), want the declared zero capability", caps, err)
	}

	emptyBlockReg := pkgcore.NewComponentRegistry()
	emptyBlockReg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{"gateway.wechat": nil},
	}))
	if err := emptyBlockReg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := emptyBlockReg.Construct(ctx); err == nil || !strings.Contains(err.Error(), "Config.MchID") {
		t.Errorf("Construct with an empty block = %v, want NewGateway's required-field error", err)
	}
	_ = emptyBlockReg.Close(context.Background())
}
