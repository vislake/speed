package stripe

// component.go registers the "gateway.stripe" component with pkgcore's
// global component registration: the descriptor a composition configuration
// selects as the "gateway" module's member. It lives beside the
// implementation it adapts, and its New funnels through gatewayFromConfig
// below -- this package's one construction path -- so a composition block
// and a flat pkgcore.Config cannot diverge on validation.

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/billing"
)

// stripeGatewayConfig is the "gateway.stripe" component's configuration
// schema: one field per key the flat construction path reads, so a
// composition block spells the same settings a flat pkgcore.Config carries
// and neither shape grows a setting the other lacks.
type stripeGatewayConfig struct {
	APIKey          string `json:"api_key"`
	WebhookSecret   string `json:"webhook_secret"`
	SuccessURL      string `json:"success_url"`
	CancelURL       string `json:"cancel_url"`
	BillingInterval string `json:"billing_interval"`
}

// stripeGatewayComponent is the component descriptor for "gateway.stripe":
// the Stripe channel over the configuration the component's own block
// spells out, declaring 0 capabilities (no bit is meaningful for a payment
// channel). It contributes a member of the "gateway" catalog
// (ProvidesMember), so several payment channels coexist as selected members
// read by name rather than as one bound gateway. Its New funnels through
// gatewayFromConfig -- the package's one construction path -- so a
// composition block and a flat pkgcore.Config cannot diverge on defaults or
// validation: both build through NewGateway, which owns the required-field
// checks. Constructing a Gateway dials nothing (the SDK client connects per
// request), so the component owns no closable resource and declares no
// Close.
var stripeGatewayComponent = pkgcore.Component{
	Name:           "gateway.stripe",
	Module:         "gateway",
	ProvidesMember: []any{(*billing.PaymentGateway)(nil)},
	Capabilities:   0,
	ConfigSchema:   (*stripeGatewayConfig)(nil),
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

// gatewayFromConfig adapts a flat pkgcore.Config onto NewGateway -- the one
// construction path stripeGatewayComponent's New funnels through.
// "api_key" and "webhook_secret" have no safe default and are rejected by
// NewGateway itself when empty; "success_url" and "cancel_url" are
// likewise required. "billing_interval" is optional (NewGateway's own
// default applies).
func gatewayFromConfig(cfg pkgcore.Config) (billing.PaymentGateway, error) {
	return NewGateway(Config{
		APIKey:          cfg["api_key"],
		WebhookSecret:   cfg["webhook_secret"],
		SuccessURL:      cfg["success_url"],
		CancelURL:       cfg["cancel_url"],
		BillingInterval: cfg["billing_interval"],
	})
}

func init() { pkgcore.MustRegister(stripeGatewayComponent) }
