package stripe_test

// Runnable documentation for the Stripe-backed billing.PaymentGateway,
// compiled and executed by `go test`. None of the Examples below reach a
// real Stripe account -- see doc.go's own "no integration leg" section.
// ExampleNewGateway and Example demonstrate construction and
// billing.PaymentGatewayRegistry usage; ExampleGateway_VerifyWebhook is the
// package's runnable, doc-rendered proof of the real thing --
// webhook.GenerateTestSignedPayload (Stripe's own SDK helper) produces a
// genuine HMAC-SHA256-signed Stripe-Signature header over a real payload,
// verified and normalized with no network call.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	stripewebhook "github.com/stripe/stripe-go/v82/webhook"

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

// ExampleGateway_VerifyWebhook signs a checkout.session.completed fixture
// with Stripe's own test-signing helper, verifies it through a real Gateway
// and prints the resulting billing.NormalizedEvent -- the signature
// verification plus payload normalization the VerifyWebhook doc comment on
// billing.PaymentGateway describes.
func ExampleGateway_VerifyWebhook() {
	const webhookSecret = "whsec_example_doc_secret"

	gw, err := stripe.NewGateway(stripe.Config{
		APIKey:        "sk_test_example",
		WebhookSecret: webhookSecret,
		SuccessURL:    "https://example.test/billing/success",
		CancelURL:     "https://example.test/billing/cancel",
	})
	if err != nil {
		fmt.Println("new gateway:", err)
		return
	}

	// The metadata keys mirror the speed_tenant_id/speed_subscription_id/
	// speed_invoice_id CreateCharge attaches to a real Checkout Session --
	// see this package's own gateway.go metadata* constants.
	payload, err := json.Marshal(map[string]any{
		"id":      "evt_example_1",
		"type":    "checkout.session.completed",
		"created": time.Now().Unix(),
		"data": map[string]any{
			"object": map[string]any{
				"id":             "cs_example_1",
				"status":         "complete",
				"payment_status": "paid",
				"amount_total":   2900,
				"currency":       "usd",
				"metadata": map[string]string{
					"speed_tenant_id":       "tenant-a",
					"speed_subscription_id": "sub-1",
					"speed_invoice_id":      "inv-1",
				},
			},
		},
	})
	if err != nil {
		fmt.Println("marshal fixture:", err)
		return
	}

	signed := stripewebhook.GenerateTestSignedPayload(&stripewebhook.UnsignedPayload{
		Payload:   payload,
		Secret:    webhookSecret,
		Timestamp: time.Now(),
	})

	event, err := gw.VerifyWebhook(context.Background(), map[string][]string{
		"Stripe-Signature": {signed.Header},
	}, signed.Payload)
	if err != nil {
		fmt.Println("verify webhook:", err)
		return
	}

	fmt.Println("event id:", event.EventID)
	fmt.Println("channel:", event.Channel)
	fmt.Println("channel reference:", event.ChannelReference)
	fmt.Println("type:", event.Type)
	fmt.Println("status:", event.Status)
	fmt.Println("amount:", event.Amount.Cents, event.Amount.Currency)
	fmt.Println("tenant:", event.TenantID, "subscription:", event.SubscriptionID, "invoice:", event.InvoiceID)
	// Output:
	// event id: evt_example_1
	// channel: stripe
	// channel reference: cs_example_1
	// type: charge_succeeded
	// status: succeeded
	// amount: 2900 usd
	// tenant: tenant-a subscription: sub-1 invoice: inv-1
}
