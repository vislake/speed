package stripe_test

// Runnable documentation for the Stripe-backed billing.PaymentGateway,
// compiled and executed by `go test`. Neither Example below reaches a real
// Stripe account -- see doc.go's own "no integration leg" section. Both
// demonstrate construction and billing.PaymentGatewayRegistry usage only.

import (
	"fmt"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/billing/gateway/stripe"
)

// ExampleNewGateway shows constructing a Gateway directly, the escape hatch
// for a caller that wants to wire it with billing.WithGateways rather than
// going through billing.PaymentGatewayRegistry. Nothing is dialed here --
// the underlying Stripe client issues no request until the first
// CreateCharge/QueryStatus call -- so this Example never needs reachable
// Stripe credentials to construct successfully.
func ExampleNewGateway() {
	gw, err := stripe.NewGateway(stripe.Config{
		APIKey:        "sk_test_example",
		WebhookSecret: "whsec_example",
		SuccessURL:    "https://example.test/billing/success",
		CancelURL:     "https://example.test/billing/cancel",
	})
	if err != nil {
		fmt.Println("new gateway:", err)
		return
	}
	//nolint:staticcheck // QF1011: the assertion doubles as written doc that
	// NewGateway satisfies billing.PaymentGateway.
	var _ billing.PaymentGateway = gw

	fmt.Println("gateway wired; the first CreateCharge/QueryStatus call contacts Stripe")
	// Output:
	// gateway wired; the first CreateCharge/QueryStatus call contacts Stripe
}

// Example demonstrates the package's self-registration: importing it for
// side effect makes "gateway.stripe" build through
// billing.PaymentGatewayRegistry.
func Example() {
	cfg := pkgcore.Config{
		"api_key":        "sk_test_example",
		"webhook_secret": "whsec_example",
		"success_url":    "https://example.test/billing/success",
		"cancel_url":     "https://example.test/billing/cancel",
	}
	gw, caps, err := billing.PaymentGatewayRegistry.Build("gateway.stripe", cfg)
	fmt.Println("gateway.stripe:", err, gw != nil, caps)

	// Output:
	// gateway.stripe: <nil> true none
}
