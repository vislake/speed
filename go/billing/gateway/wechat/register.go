package wechat

// Self-registration for this package's billing.PaymentGateway
// implementation, mirroring go/billing/gateway/stripe's and .../alipay's
// own register.go: importing this package -- for side effect alone --
// registers "gateway.wechat" on billing.PaymentGatewayRegistry.

import (
	"fmt"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/billing"
)

func init() {
	mustRegister(pkgcore.Registration[billing.PaymentGateway]{
		Name:         "gateway.wechat",
		Capabilities: 0,
		New:          gatewayFromConfig,
	})
}

// mustRegister adds r to billing.PaymentGatewayRegistry and panics if that
// fails. See go/billing/gateway/stripe/register.go's identical helper for
// why this package carries its own copy rather than sharing one.
func mustRegister(r pkgcore.Registration[billing.PaymentGateway]) {
	if err := billing.PaymentGatewayRegistry.Register(r); err != nil {
		panic(fmt.Sprintf("billing/gateway/wechat: builtin implementation registration failed: %v", err))
	}
}

// gatewayFromConfig adapts a flat pkgcore.Config onto NewGateway.
// "mch_id", "app_id", "mch_cert_serial_no", "mch_private_key_pem",
// "api_v3_key" and "notify_url" have no safe default and are rejected by
// NewGateway when empty or malformed; "platform_public_key_pem" is
// likewise required (see doc.go's "Known limitation: static platform
// certificate"); "gateway_url" is optional (Config.gatewayURL's own
// default applies).
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
