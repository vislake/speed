package wechat

// component.go registers the "gateway.wechat" component with pkgcore's
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

// wechatGatewayConfig is the "gateway.wechat" component's configuration
// schema: one field per key the flat construction path reads, so a
// composition block spells the same settings a flat pkgcore.Config carries
// and neither shape grows a setting the other lacks. The PEM and APIv3 key
// fields are the literal text, converted to []byte on the way to Config --
// the same conversion gatewayFromConfig makes.
type wechatGatewayConfig struct {
	MchID                string `json:"mch_id"`
	AppID                string `json:"app_id"`
	MchCertSerialNo      string `json:"mch_cert_serial_no"`
	MchPrivateKeyPEM     string `json:"mch_private_key_pem"`
	APIv3Key             string `json:"api_v3_key"`
	PlatformPublicKeyPEM string `json:"platform_public_key_pem"`
	NotifyURL            string `json:"notify_url"`
	GatewayURL           string `json:"gateway_url"`
}

// wechatGatewayComponent is the component descriptor for "gateway.wechat":
// the WeChat Pay channel over the configuration the component's own block
// spells out, declaring 0 capabilities. It contributes a member of the
// "gateway" catalog (ProvidesMember), so several payment channels coexist
// as selected members read by name rather than as one bound gateway. Its
// New funnels through gatewayFromConfig -- the package's one construction
// path -- so a composition block and a flat pkgcore.Config cannot diverge
// on defaults or validation: both build through NewGateway, which parses
// the merchant keys, checks the APIv3 key length and owns the required-field
// checks. Constructing a Gateway dials nothing, so the component owns no
// closable resource and declares no Close.
var wechatGatewayComponent = pkgcore.Component{
	Name:           "gateway.wechat",
	Module:         "gateway",
	ProvidesMember: []any{(*billing.PaymentGateway)(nil)},
	Capabilities:   0,
	ConfigSchema:   (*wechatGatewayConfig)(nil),
	New: func(_ context.Context, _ *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
		var c wechatGatewayConfig
		if err := cfg.Decode(&c); err != nil {
			return nil, err
		}
		return gatewayFromConfig(pkgcore.Config{
			"mch_id":                  c.MchID,
			"app_id":                  c.AppID,
			"mch_cert_serial_no":      c.MchCertSerialNo,
			"mch_private_key_pem":     c.MchPrivateKeyPEM,
			"api_v3_key":              c.APIv3Key,
			"platform_public_key_pem": c.PlatformPublicKeyPEM,
			"notify_url":              c.NotifyURL,
			"gateway_url":             c.GatewayURL,
		})
	},
}

// gatewayFromConfig adapts a flat pkgcore.Config onto NewGateway -- the one
// construction path wechatGatewayComponent's New funnels through.
func gatewayFromConfig(cfg pkgcore.Config) (billing.PaymentGateway, error) {
	return NewGateway(Config{
		MchID:                cfg["mch_id"],
		AppID:                cfg["app_id"],
		MchCertSerialNo:      cfg["mch_cert_serial_no"],
		MchPrivateKeyPEM:     []byte(cfg["mch_private_key_pem"]),
		APIv3Key:             []byte(cfg["api_v3_key"]),
		PlatformPublicKeyPEM: []byte(cfg["platform_public_key_pem"]),
		NotifyURL:            cfg["notify_url"],
		GatewayURL:           cfg["gateway_url"],
	})
}

func init() { pkgcore.MustRegister(wechatGatewayComponent) }
