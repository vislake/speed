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
	var sess stripego.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &sess); err != nil {
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithCause(err)
	}

	tenantID := sess.Metadata[metadataTenantID]
	subscriptionID := sess.Metadata[metadataSubscriptionID]
	invoiceID := sess.Metadata[metadataInvoiceID]
	if tenantID == "" || subscriptionID == "" || invoiceID == "" {
		// Missing the metadata CreateCharge always attaches: either an
		// event for a Checkout Session this package never created, or a
		// genuine data-shape surprise. Refused, per VerifyWebhook's own
		// contract, rather than passed upstream with blank identifiers.
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithParam("reason", "checkout session metadata missing tenant/subscription/invoice identifiers")
	}

	var eventType billing.NormalizedEventType
	var status billing.ChannelStatus
	switch string(event.Type) {
	case eventTypeCheckoutSessionCompleted:
		eventType = billing.NormalizedEventChargeSucceeded
		status = billing.ChannelStatusSucceeded
	case eventTypeCheckoutSessionExpired, eventTypeCheckoutSessionAsyncFailed:
		eventType = billing.NormalizedEventChargeFailed
		status = billing.ChannelStatusFailed
	default:
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithParam("event_type", string(event.Type))
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
		Amount:           billing.Money{Cents: sess.AmountTotal, Currency: string(sess.Currency)},
		OccurredAt:       time.Unix(event.Created, 0).UTC(),
		RawPayload:       rawBody,
	}, nil
}
