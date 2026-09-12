package alipay

// component.go registers the "gateway.alipay" component with pkgcore's
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

// alipayGatewayConfig is the "gateway.alipay" component's configuration
// schema: one field per key the flat construction path reads, so a
// composition block spells the same settings a flat pkgcore.Config carries
// and neither shape grows a setting the other lacks. The two PEM fields are
// the PEM text itself, converted to []byte on the way to Config -- the same
// conversion gatewayFromConfig makes.
type alipayGatewayConfig struct {
	AppID              string `json:"app_id"`
	PrivateKeyPEM      string `json:"private_key_pem"`
	AlipayPublicKeyPEM string `json:"alipay_public_key_pem"`
	NotifyURL          string `json:"notify_url"`
	GatewayURL         string `json:"gateway_url"`
}

// alipayGatewayComponent is the component descriptor for "gateway.alipay":
// the Alipay channel over the configuration the component's own block
// spells out, declaring 0 capabilities. Its New funnels through
// gatewayFromConfig -- the package's one construction path -- so a
// composition block and a flat pkgcore.Config cannot diverge on defaults or
// validation: both build through NewGateway, which parses the RSA keys and
// owns the required-field checks. Constructing a Gateway dials nothing, so
// the component owns no closable resource and declares no Close.
var alipayGatewayComponent = pkgcore.Component{
	Name:         "gateway.alipay",
	Module:       "gateway",
	Provides:     []any{(*billing.PaymentGateway)(nil)},
	Capabilities: 0,
	ConfigSchema: (*alipayGatewayConfig)(nil),
	New: func(_ context.Context, _ *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
		var c alipayGatewayConfig
		if err := cfg.Decode(&c); err != nil {
			return nil, err
		}
		return gatewayFromConfig(pkgcore.Config{
			"app_id":                c.AppID,
			"private_key_pem":       c.PrivateKeyPEM,
			"alipay_public_key_pem": c.AlipayPublicKeyPEM,
			"notify_url":            c.NotifyURL,
			"gateway_url":           c.GatewayURL,
		})
	},
}

// gatewayFromConfig adapts a flat pkgcore.Config onto NewGateway -- the one
// construction path alipayGatewayComponent's New funnels through.
func gatewayFromConfig(cfg pkgcore.Config) (billing.PaymentGateway, error) {
	return NewGateway(Config{
		AppID:              cfg["app_id"],
		PrivateKeyPEM:      []byte(cfg["private_key_pem"]),
		AlipayPublicKeyPEM: []byte(cfg["alipay_public_key_pem"]),
		NotifyURL:          cfg["notify_url"],
		GatewayURL:         cfg["gateway_url"],
	})
}

func init() { pkgcore.MustRegister(alipayGatewayComponent) }
