package stripe

// Self-registration for this package's billing.PaymentGateway
// implementation, mirroring go/pki/signer/kmsaws's own register.go
// (itself mirroring go/pkgcore's split subpackages' database/sql-style
// driver-registration pattern): importing this package -- for side effect
// alone -- registers "gateway.stripe" on billing.PaymentGatewayRegistry.

import (
	"fmt"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/billing"
)

func init() {
	mustRegister(pkgcore.Registration[billing.PaymentGateway]{
		Name:         "gateway.stripe",
		Capabilities: 0,
		New:          gatewayFromConfig,
	})
}

// mustRegister adds r to billing.PaymentGatewayRegistry and panics if that
// fails. See go/pki/signer/kmsaws/register.go's identical helper for why
// this package carries its own copy rather than sharing one.
func mustRegister(r pkgcore.Registration[billing.PaymentGateway]) {
	if err := billing.PaymentGatewayRegistry.Register(r); err != nil {
		panic(fmt.Sprintf("billing/gateway/stripe: builtin implementation registration failed: %v", err))
	}
}

// gatewayFromConfig adapts a flat pkgcore.Config onto NewGateway.
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
