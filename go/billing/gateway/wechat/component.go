package wechat

// component.go registers the "gateway.wechat" component with pkgcore's
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

// wechatGatewayConfig is the "gateway.wechat" component's configuration
// schema: one field per key the seam registration documents, so a
// composition block spells the same settings a flat pkgcore.Config carries
// and neither face grows a setting the other lacks. The PEM and APIv3 key
// fields are the literal text, converted to []byte on the way to Config --
// the same conversion the seam registration makes.
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
// spells out, declaring the same 0 capabilities the seam registration
// declares. Its New funnels through gatewayFromConfig -- the package's one
// construction path -- so the component face and the seam face cannot
// diverge on defaults or validation: both build through NewGateway, which
// parses the merchant keys, checks the APIv3 key length and owns the
// required-field checks. Constructing a Gateway dials nothing, so the
// component owns no closable resource and declares no Close.
var wechatGatewayComponent = pkgcore.Component{
	Name:         "gateway.wechat",
	Module:       "gateway",
	Provides:     []any{(*billing.PaymentGateway)(nil)},
	Capabilities: 0,
	ConfigSchema: (*wechatGatewayConfig)(nil),
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

func init() { pkgcore.MustRegister(wechatGatewayComponent) }
