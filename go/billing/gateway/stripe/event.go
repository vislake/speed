package stripe

import (
	"encoding/json"
	"time"

	stripego "github.com/stripe/stripe-go/v82"

	"github.com/vislake/speed/go/billing"
)

// The Stripe event types this package recognizes. Every other event type
// Stripe might deliver -- and Stripe delivers many, for objects and
// lifecycle transitions this round's Checkout-Session-only flow never
// creates -- is ErrWebhookPayloadUnrecognized, never silently ignored: a
// caller asking "was this delivery understood" must get an honest no for
// anything this package cannot yet map, rather than a NormalizedEvent
// synthesized from a guess.
const (
	eventTypeCheckoutSessionCompleted   = "checkout.session.completed"
	eventTypeCheckoutSessionExpired     = "checkout.session.expired"
	eventTypeCheckoutSessionAsyncFailed = "checkout.session.async_payment_failed"
)

// normalizeEvent maps a verified stripe.Event onto a billing.NormalizedEvent.
// rawBody is the exact bytes VerifyWebhook was given -- kept as
// NormalizedEvent.RawPayload for PaymentEvent's own audit trail, never
// re-derived from event (which is already a decoded, in-memory
// representation of the same bytes).
func normalizeEvent(event stripego.Event, rawBody []byte) (billing.NormalizedEvent, error) {
	switch string(event.Type) {
	case eventTypeCheckoutSessionCompleted, eventTypeCheckoutSessionExpired, eventTypeCheckoutSessionAsyncFailed:
		return normalizeCheckoutSession(event, rawBody)
	default:
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithParam("event_type", string(event.Type))
	}
}

// metadataIdentifiers extracts and validates the tenant/subscription/invoice
// identifiers CreateCharge attaches (gateway.go's metadataTenantID/
// metadataSubscriptionID/metadataInvoiceID constants), from whichever
// channel-side object's own Metadata map carried them.
func metadataIdentifiers(metadata map[string]string) (tenantID, subscriptionID, invoiceID string, err error) {
	tenantID = metadata[metadataTenantID]
	subscriptionID = metadata[metadataSubscriptionID]
	invoiceID = metadata[metadataInvoiceID]
	if tenantID == "" || subscriptionID == "" || invoiceID == "" {
		// Missing the metadata CreateCharge always attaches: either an
		// event for a Stripe object this package never created, or a
		// genuine data-shape surprise. Refused, per VerifyWebhook's own
		// contract, rather than passed upstream with blank identifiers.
		return "", "", "", billing.ErrWebhookPayloadUnrecognized.WithParam("reason", "metadata missing tenant/subscription/invoice identifiers")
	}
	return tenantID, subscriptionID, invoiceID, nil
}

// normalizeCheckoutSession handles the three checkout.session.* events.
func normalizeCheckoutSession(event stripego.Event, rawBody []byte) (billing.NormalizedEvent, error) {
	var sess stripego.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &sess); err != nil {
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithCause(err)
	}

	tenantID, subscriptionID, invoiceID, err := metadataIdentifiers(sess.Metadata)
	if err != nil {
		return billing.NormalizedEvent{}, err
	}

	var eventType billing.NormalizedEventType
	var status billing.ChannelStatus
	amount := billing.Money{Cents: sess.AmountTotal, Currency: string(sess.Currency)}
	switch string(event.Type) {
	case eventTypeCheckoutSessionCompleted:
		// The SAME agreement point PaymentGateway.QueryStatus's own
		// sessionStatus (gateway.go) uses: a "complete" Checkout Session can
		// still carry PaymentStatusUnpaid (a deferred payment method, or a
		// genuinely delayed settlement), and reporting
		// NormalizedEventChargeSucceeded/ChannelStatusSucceeded for that case
		// -- as this package once did unconditionally -- is exactly the
		// webhook-vs-query disagreement docs/internal/06-billing-and-metering.md's
		// callbacks-cannot-be-trusted rule exists to prevent: one Stripe
		// session state must produce ONE module-side record whichever
		// channel reported it. sessionStatus's own "complete but unpaid ->
		// still pending" answer is mirrored here exactly, so a
		// PaymentEvent row this webhook path inserts lands at the SAME
		// ChannelStatus the active-polling fallback (job.go's
		// PollingService) would later re-query and find -- the row is then
		// genuinely ChannelStatusPending and picked up by that same
		// fallback, rather than sitting at a false Succeeded forever.
		if sess.PaymentStatus == stripego.CheckoutSessionPaymentStatusUnpaid {
			eventType = billing.NormalizedEventChargePending
			status = billing.ChannelStatusPending
			amount = billing.Money{}
		} else {
			eventType = billing.NormalizedEventChargeSucceeded
			status = billing.ChannelStatusSucceeded
		}
	case eventTypeCheckoutSessionExpired, eventTypeCheckoutSessionAsyncFailed:
		eventType = billing.NormalizedEventChargeFailed
		status = billing.ChannelStatusFailed
	}

	return billing.NormalizedEvent{
		EventID:          event.ID,
		Channel:          "stripe",
		ChannelReference: billing.ChannelReference(sess.ID),
		TenantID:         tenantID,
		SubscriptionID:   subscriptionID,
		InvoiceID:        invoiceID,
		Type:             eventType,
		Status:           status,
		Amount:           amount,
		OccurredAt:       time.Unix(event.Created, 0).UTC(),
		RawPayload:       rawBody,
	}, nil
}
