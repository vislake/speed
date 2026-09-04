package stripe

// Config configures a Gateway over one Stripe account.
type Config struct {
	// APIKey is the Stripe secret key ("sk_..." or "rk_...") every request
	// this package makes authenticates with. Required.
	APIKey string

	// WebhookSecret is the signing secret ("whsec_...") for the Stripe
	// webhook endpoint destination this Gateway verifies deliveries
	// against. Required for VerifyWebhook; CreateCharge and QueryStatus do
	// not use it.
	WebhookSecret string

	// SuccessURL and CancelURL are where Stripe Checkout redirects the
	// payer after the collection completes or is abandoned. Both required:
	// Stripe's own Checkout Session API refuses a subscription-mode session
	// created without a success_url, and refuses to omit both from a URL
	// the payer can return to.
	SuccessURL string
	CancelURL  string

	// BillingInterval is the recurring interval CreateCharge's inline
	// price uses -- "month" or "year", matching billing.BillingInterval's
	// own vocabulary (never billing.BillingIntervalOneTime, which has no
	// meaning for Stripe's own native-subscription leg -- see
	// PaymentGateway.CreateCharge's own doc comment for why Stripe's
	// CreateCharge always establishes a recurring subscription). Defaults
	// to "month" when left empty.
	BillingInterval string
}
