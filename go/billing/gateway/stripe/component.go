package stripe

// component.go registers the "gateway.stripe" component with pkgcore's
// global component registration: the descriptor a composition configuration
// selects as the "gateway" module's member. It lives beside the
// implementation it adapts, the same file-locality the package's own seam
// registration (register.go) keeps. Both faces carry the identical name --
// the component and the seam registration are two resolution paths to the
// same implementation, and the registration stays the name-based path a
// Preset-shaped caller resolves through.

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/billing"
)

// stripeGatewayConfig is the "gateway.stripe" component's configuration
// schema: one field per key the seam registration documents, so a
// composition block spells the same settings a flat pkgcore.Config carries
// and neither face grows a setting the other lacks.
type stripeGatewayConfig struct {
	APIKey          string `json:"api_key"`
	WebhookSecret   string `json:"webhook_secret"`
	SuccessURL      string `json:"success_url"`
	CancelURL       string `json:"cancel_url"`
	BillingInterval string `json:"billing_interval"`
}

// stripeGatewayComponent is the component descriptor for "gateway.stripe":
// the Stripe channel over the configuration the component's own block
// spells out, declaring the same 0 capabilities the seam registration
// declares (no bit is meaningful for a payment channel). Its New funnels
// through gatewayFromConfig -- the package's one construction path -- so
// the component face and the seam face cannot diverge on defaults or
// validation: both build through NewGateway, which owns the required-field
// checks. Constructing a Gateway dials nothing (the SDK client connects per
// request), so the component owns no closable resource and declares no
// Close.
var stripeGatewayComponent = pkgcore.Component{
	Name:         "gateway.stripe",
	Module:       "gateway",
	Provides:     []any{(*billing.PaymentGateway)(nil)},
	Capabilities: 0,
	ConfigSchema: (*stripeGatewayConfig)(nil),
	New: func(_ context.Context, _ *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
		var c stripeGatewayConfig
		if err := cfg.Decode(&c); err != nil {
			return nil, err
		}
		return gatewayFromConfig(pkgcore.Config{
			"api_key":          c.APIKey,
			"webhook_secret":   c.WebhookSecret,
			"success_url":      c.SuccessURL,
			"cancel_url":       c.CancelURL,
			"billing_interval": c.BillingInterval,
		})
	},
}

func init() { pkgcore.MustRegister(stripeGatewayComponent) }
