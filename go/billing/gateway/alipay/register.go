package alipay

// Self-registration for this package's billing.PaymentGateway
// implementation, mirroring go/billing/gateway/stripe's own register.go and,
// through it, go/pki/signer/kmsaws's identical database/sql-style
// driver-registration pattern: importing this package -- for side effect
// alone -- registers "gateway.alipay" on billing.PaymentGatewayRegistry.

import (
	"fmt"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/billing"
)

func init() {
	mustRegister(pkgcore.Registration[billing.PaymentGateway]{
		Name:         "gateway.alipay",
		Capabilities: 0,
		New:          gatewayFromConfig,
	})
}

// mustRegister adds r to billing.PaymentGatewayRegistry and panics if that
// fails. See go/billing/gateway/stripe/register.go's identical helper for
// why this package carries its own copy rather than sharing one.
func mustRegister(r pkgcore.Registration[billing.PaymentGateway]) {
	if err := billing.PaymentGatewayRegistry.Register(r); err != nil {
		panic(fmt.Sprintf("billing/gateway/alipay: builtin implementation registration failed: %v", err))
	}
}

// gatewayFromConfig adapts a flat pkgcore.Config onto NewGateway.
// "app_id", "private_key_pem", "alipay_public_key_pem" and "notify_url"
// have no safe default and are rejected by NewGateway/Config.parseKeys
// when empty or malformed; "gateway_url" is optional (Config.gatewayURL's
// own default applies).
func gatewayFromConfig(cfg pkgcore.Config) (billing.PaymentGateway, error) {
	return NewGateway(Config{
		AppID:              cfg["app_id"],
		PrivateKeyPEM:      []byte(cfg["private_key_pem"]),
		AlipayPublicKeyPEM: []byte(cfg["alipay_public_key_pem"]),
		NotifyURL:          cfg["notify_url"],
		GatewayURL:         cfg["gateway_url"],
	})
}
