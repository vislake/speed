package stripe

import (
	"context"
	"errors"
	"fmt"

	stripego "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/checkout/session"
	"github.com/stripe/stripe-go/v82/webhook"

	"github.com/vislake/speed/go/billing"
)

// metadataTenantID/metadataSubscriptionID/metadataInvoiceID are the
// Checkout Session metadata keys CreateCharge attaches
// billing.ChargeRequest's own identifiers under, and VerifyWebhook decodes
// them back out of -- see billing.NormalizedEvent's own doc comment for why
// this round-trip exists at all.
const (
	metadataTenantID       = "speed_tenant_id"
	metadataSubscriptionID = "speed_subscription_id"
	metadataInvoiceID      = "speed_invoice_id"
)

const defaultBillingInterval = "month"

// Gateway is this package's billing.PaymentGateway implementation over one
// Stripe account. See doc.go for why it is built over the SDK's per-resource
// Client types (session.Client) rather than the newer stripe.Client
// aggregate.
type Gateway struct {
	sessions session.Client
	cfg      Config
}

// NewGateway returns a Gateway over cfg. Nothing is dialed here -- every
// built-in seam's client construction in this codebase is lazy, and
// Stripe's own Backend is no exception (its first HTTP round trip happens
// on the first CreateCharge/QueryStatus call). An empty cfg.APIKey or
// cfg.WebhookSecret is rejected immediately, since neither method this
// package implements can do anything useful without them.
func NewGateway(cfg Config) (*Gateway, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("billing/gateway/stripe: NewGateway requires a non-empty Config.APIKey")
	}
	if cfg.WebhookSecret == "" {
		return nil, fmt.Errorf("billing/gateway/stripe: NewGateway requires a non-empty Config.WebhookSecret")
	}
	if cfg.SuccessURL == "" || cfg.CancelURL == "" {
		return nil, fmt.Errorf("billing/gateway/stripe: NewGateway requires non-empty Config.SuccessURL and Config.CancelURL")
	}
	if cfg.BillingInterval == "" {
		cfg.BillingInterval = defaultBillingInterval
	}

	backend := stripego.GetBackend(stripego.APIBackend)
	return &Gateway{
		sessions: session.Client{B: backend, Key: cfg.APIKey},
		cfg:      cfg,
	}, nil
}

// newGatewayWithBackend is NewGateway's test-only twin, injecting a scripted
// stripe.Backend in place of the real one GetBackend(APIBackend) returns.
// See doc.go's own note on why stripe.Backend -- explicitly documented by
// the SDK as existing "to enable mocking for during testing if needed" -- is
// this package's whole testing seam for CreateCharge/QueryStatus.
func newGatewayWithBackend(backend stripego.Backend, cfg Config) *Gateway {
	if cfg.BillingInterval == "" {
		cfg.BillingInterval = defaultBillingInterval
	}
	return &Gateway{sessions: session.Client{B: backend, Key: cfg.APIKey}, cfg: cfg}
}

// CreateCharge implements billing.PaymentGateway. For Stripe, it creates one
// Checkout Session in "subscription" mode with a single inline recurring
// price for req.Amount -- see billing.PaymentGateway.CreateCharge's own doc
// comment for why this establishes a genuine, ongoing Stripe subscription
// rather than a one-time charge: Stripe generates every later cycle's own
// invoice and payment on its own once the payer completes this Session, and
// a caller does not call CreateCharge again for the same
// billing.Subscription's later cycles.
func (g *Gateway) CreateCharge(ctx context.Context, req billing.ChargeRequest) (billing.ChargeHandle, error) {
	currency := req.Amount.Currency
	params := &stripego.CheckoutSessionParams{
		Mode:       stripego.String(string(stripego.CheckoutSessionModeSubscription)),
		SuccessURL: stripego.String(g.cfg.SuccessURL),
		CancelURL:  stripego.String(g.cfg.CancelURL),
		LineItems: []*stripego.CheckoutSessionLineItemParams{
			{
				Quantity: stripego.Int64(1),
				PriceData: &stripego.CheckoutSessionLineItemPriceDataParams{
					Currency:   stripego.String(currency),
					UnitAmount: stripego.Int64(req.Amount.Cents),
					Recurring: &stripego.CheckoutSessionLineItemPriceDataRecurringParams{
						Interval: stripego.String(g.cfg.BillingInterval),
					},
					ProductData: &stripego.CheckoutSessionLineItemPriceDataProductDataParams{
						Name: stripego.String(chargeDescription(req)),
					},
				},
			},
		},
		Metadata: map[string]string{
			metadataTenantID:       req.TenantID,
			metadataSubscriptionID: req.SubscriptionID,
			metadataInvoiceID:      req.InvoiceID,
		},
	}
	params.Context = ctx
	if req.IdempotencyKey != "" {
		params.SetIdempotencyKey(req.IdempotencyKey)
	}

	//nolint:staticcheck // SA1019: session.Client is deliberately used over
	// stripe.Client for its injectable stripe.Backend -- see doc.go's own
	// SDK-choice section for why this package's whole testing strategy
	// depends on it.
	sess, err := g.sessions.New(params)
	if err != nil {
		return billing.ChargeHandle{}, fmt.Errorf("billing/gateway/stripe: create checkout session: %w", err)
	}
	return billing.ChargeHandle{
		ChannelReference: billing.ChannelReference(sess.ID),
		RedirectURL:      sess.URL,
	}, nil
}

// chargeDescription falls back to a generic label when req.Description is
// empty -- Stripe's own product_data.name is required whenever price_data
// is used, so this package must never send an empty string.
func chargeDescription(req billing.ChargeRequest) string {
	if req.Description != "" {
		return req.Description
	}
	return "Subscription"
}

// VerifyWebhook implements billing.PaymentGateway. It performs no network
// call: webhook.ConstructEventWithOptions (Stripe's own SDK function)
// recomputes the expected HMAC-SHA256 signature over the timestamped
// payload and compares it, constant-time, against every signature the
// Stripe-Signature header carries -- see doc.go's own testing-strategy
// section for how this is exercised offline against a real, SDK-generated
// fixture.
//
// IgnoreAPIVersionMismatch is deliberately set: the plain webhook.ConstructEvent
// also refuses an event whose own api_version does not match the exact
// Stripe API version this pinned SDK release was generated against --
// correct in principle, but a real merchant account's webhook endpoint is
// ordinarily pinned to whatever API version the account itself was created
// under (frequently much older than this package's pinned SDK), not to
// whatever version stripe-go vNN.N.N happens to expect. Refusing every
// delivery over that mismatch would make VerifyWebhook fail closed for
// essentially any real account unless its version happened to line up
// exactly with this dependency's release cadence, which is a fragility this
// package does not accept -- the HMAC signature itself, never the
// api_version field, is what actually proves the delivery is genuine.
func (g *Gateway) VerifyWebhook(_ context.Context, headers map[string][]string, body []byte) (billing.NormalizedEvent, error) {
	sigHeader := firstHeader(headers, "Stripe-Signature")
	if sigHeader == "" {
		return billing.NormalizedEvent{}, billing.ErrWebhookSignatureInvalid.WithParam("reason", "missing Stripe-Signature header")
	}

	event, err := webhook.ConstructEventWithOptions(body, sigHeader, g.cfg.WebhookSecret, webhook.ConstructEventOptions{
		IgnoreAPIVersionMismatch: true,
	})
	if err != nil {
		return billing.NormalizedEvent{}, billing.ErrWebhookSignatureInvalid.WithCause(err)
	}

	return normalizeEvent(event, body)
}

// firstHeader returns the first value of the named header, case-sensitively
// -- callers of VerifyWebhook are expected to pass headers exactly as
// received (net/http.Header's own canonical casing), matching Stripe's own
// examples.
func firstHeader(headers map[string][]string, name string) string {
	for _, v := range headers[name] {
		return v
	}
	return ""
}

// QueryStatus implements billing.PaymentGateway: retrieves the Checkout
// Session named by ref and maps its Status/PaymentStatus to a
// billing.ChannelStatus -- the authoritative re-query
// docs/internal/06-billing-and-metering.md's callbacks-cannot-be-trusted
// rule requires, never trusting a webhook body's own numbers.
func (g *Gateway) QueryStatus(ctx context.Context, ref billing.ChannelReference) (billing.ChannelStatus, billing.Money, error) {
	params := &stripego.CheckoutSessionParams{}
	params.Context = ctx
	// Expand "subscription" so sessionStatus can distinguish a Complete/
	// Unpaid session still awaiting an async payment method's settlement
	// from one whose underlying Subscription has already reached a
	// terminal failure state -- see sessionStatus's own doc comment for why
	// the session's own Status/PaymentStatus pair alone can never make that
	// distinction. Without this, Subscription arrives as an ID-only stub
	// (stripego.Subscription's own custom UnmarshalJSON leaves every other
	// field zero-valued for an unexpanded reference), and sessionStatus's
	// switch below would never see a populated Status to match against.
	//
	// Set directly on CheckoutSessionParams's own Expand field, never
	// through the embedded Params.AddExpand: stripe-go v82 declares its own
	// top-level `Expand []*string` on CheckoutSessionParams (shadowing the
	// embedded Params.Expand for form encoding), and AddExpand's own doc
	// comment says as much -- "AddExpand on the Params embedded struct is
	// deprecated ... use Expand in the surrounding struct instead". Calling
	// AddExpand here would silently mutate a field the request never
	// serializes.
	params.Expand = []*string{stripego.String("subscription")}
	//nolint:staticcheck // SA1019: see the identical justification on
	// CreateCharge's own g.sessions.New call above.
	sess, err := g.sessions.Get(string(ref), params)
	if err != nil {
		if isNotFound(err) {
			return "", billing.Money{}, billing.ErrChannelReferenceNotFound.WithParam("channel_reference", string(ref))
		}
		return "", billing.Money{}, fmt.Errorf("billing/gateway/stripe: get checkout session %q: %w", ref, err)
	}

	return sessionStatus(sess), billing.Money{Cents: sess.AmountTotal, Currency: string(sess.Currency)}, nil
}

// sessionStatus maps a stripe.CheckoutSession's Status/PaymentStatus pair
// onto billing.ChannelStatus.
//
// # The Complete/Unpaid case cannot be resolved from Status/PaymentStatus alone
//
// A "complete" session using an asynchronous payment method (e.g. a bank
// debit) can sit at PaymentStatusUnpaid for a while as the payment settles --
// the ordinary in-flight case the branch below still answers Pending for.
// But an async payment method's LATER, definitive failure (Stripe's own
// checkout.session.async_payment_failed webhook -- event.go's
// eventTypeCheckoutSessionAsyncFailed) never changes the session's own
// Status or PaymentStatus: stripe-go v82's CheckoutSessionStatus is only
// {open, complete, expired} and CheckoutSessionPaymentStatus is only {paid,
// unpaid, no_payment_required} -- neither enum has a "failed" value, so a
// session that completed via redirect before its async payment later failed
// does not revert to "expired". Without a second signal, QueryStatus
// re-queried at any later time would answer Pending forever for that
// session -- exactly the permanently-unresolvable row
// PollingService.Poll's active-polling fallback (job.go) must never produce,
// since re-querying a stuck row and finding a resolution is that
// mechanism's entire job.
//
// The underlying Subscription's own status -- expanded by QueryStatus above
// -- is the second signal: this package's CreateCharge only ever creates
// "subscription"-mode sessions with the default charge_automatically
// collection method, under which a first-cycle payment failure moves the
// Subscription to "incomplete" while Stripe's smart retries are still in
// play, then to the terminal "incomplete_expired" once its 23-hour
// collection window closes with no successful payment (Stripe voids the
// still-unpaid invoice at that point) -- or, more directly, "canceled" if
// the subscription is canceled outright before ever activating. Either
// terminal state is a genuine, permanent failure this method can now
// report, eventually converging with whatever async_payment_failed already
// recorded on the webhook side; "unpaid" is included defensively for the
// send_invoice collection method this package's CreateCharge never
// requests, but which behaves identically for this purpose if it ever were.
// Until the Subscription reaches one of these terminal states, the payment
// may still succeed on a later retry, so Pending remains the honest answer
// -- the identical discipline event.go's eventTypeSubscriptionUpdated
// handling already applies to a live customer.subscription.updated webhook.
func sessionStatus(sess *stripego.CheckoutSession) billing.ChannelStatus {
	switch sess.Status {
	case stripego.CheckoutSessionStatusExpired:
		return billing.ChannelStatusFailed
	case stripego.CheckoutSessionStatusComplete:
		if sess.PaymentStatus == stripego.CheckoutSessionPaymentStatusUnpaid {
			if sub := sess.Subscription; sub != nil {
				switch sub.Status {
				case stripego.SubscriptionStatusIncompleteExpired,
					stripego.SubscriptionStatusCanceled,
					stripego.SubscriptionStatusUnpaid:
					return billing.ChannelStatusFailed
				}
			}
			// Still settling (or the terminal states above do not apply
			// yet): treat conservatively as still pending rather than
			// reporting success for money that has not actually moved.
			return billing.ChannelStatusPending
		}
		return billing.ChannelStatusSucceeded
	default: // stripego.CheckoutSessionStatusOpen
		return billing.ChannelStatusPending
	}
}

// isNotFound reports whether err is Stripe's own "no such checkout session"
// answer -- an *stripego.Error with Type InvalidRequestError and Code
// resource_missing, per Stripe's documented error taxonomy. errors.As
// (rather than a direct type assertion) is used so this still recognizes
// the error if the SDK, or a future edit here, ever wraps it.
func isNotFound(err error) bool {
	var stripeErr *stripego.Error
	return errors.As(err, &stripeErr) && stripeErr.Code == stripego.ErrorCodeResourceMissing
}

// compile-time check that *Gateway satisfies billing.PaymentGateway.
var _ billing.PaymentGateway = (*Gateway)(nil)
