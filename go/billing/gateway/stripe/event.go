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

	// eventTypeInvoicePaid and eventTypeInvoicePaymentFailed are the two
	// events that actually announce a Stripe-native subscription's LATER
	// billing cycles: once CreateCharge's one Checkout Session establishes
	// the recurring subscription (gateway.go's own CreateCharge doc
	// comment), every subsequent cycle's own invoice is generated and
	// collected entirely on Stripe's side -- never a second
	// checkout.session.completed, which fires exactly once, for the first
	// cycle's own Checkout Session. A renewal that fails at Stripe without
	// this package recognizing invoice.payment_failed is exactly the
	// "renewal fails silently" gap this package's own earlier revision left
	// open.
	eventTypeInvoicePaid          = "invoice.paid"
	eventTypeInvoicePaymentFailed = "invoice.payment_failed"

	// eventTypeSubscriptionUpdated is customer.subscription.updated, fired
	// whenever the underlying Stripe Subscription's own Status field
	// changes. Only recognized for a transition INTO
	// stripego.SubscriptionStatusCanceled -- see
	// normalizeSubscriptionUpdated's own doc comment for why every other
	// Stripe subscription status is refused rather than forced onto a
	// billing.SubscriptionStatus this module's own model was never designed
	// to represent.
	eventTypeSubscriptionUpdated = "customer.subscription.updated"
)

// normalizeEvent maps a verified stripe.Event onto a billing.NormalizedEvent,
// dispatching by event.Type to the object shape each recognized event
// actually carries: a stripe.CheckoutSession for the three checkout events,
// a stripe.Invoice for the two invoice events, a stripe.Subscription for
// customer.subscription.updated. rawBody is the exact bytes VerifyWebhook
// was given -- kept as NormalizedEvent.RawPayload for PaymentEvent's own
// audit trail, never re-derived from event (which is already a decoded,
// in-memory representation of the same bytes).
func normalizeEvent(event stripego.Event, rawBody []byte) (billing.NormalizedEvent, error) {
	switch string(event.Type) {
	case eventTypeCheckoutSessionCompleted, eventTypeCheckoutSessionExpired, eventTypeCheckoutSessionAsyncFailed:
		return normalizeCheckoutSession(event, rawBody)
	case eventTypeInvoicePaid, eventTypeInvoicePaymentFailed:
		return normalizeInvoice(event, rawBody)
	case eventTypeSubscriptionUpdated:
		return normalizeSubscriptionUpdated(event, rawBody)
	default:
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithParam("event_type", string(event.Type))
	}
}

// metadataIdentifiers extracts and validates the tenant/subscription/invoice
// identifiers CreateCharge attaches (gateway.go's metadataTenantID/
// metadataSubscriptionID/metadataInvoiceID constants), from whichever
// channel-side object's own Metadata map carried them. Shared by every event
// type this package recognizes so the "missing required identifiers"
// refusal is byte-identical no matter which Stripe object this round's
// events concern.
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

// normalizeInvoice handles invoice.paid and invoice.payment_failed --
// Stripe's own announcement of a subscription's later billing cycles (see
// eventTypeInvoicePaid's own doc comment above).
//
// The correlation identifiers are read from the invoice's PARENT snapshot,
// never from the Invoice object's own metadata field. An invoice a
// subscription generated carries the subscription's metadata -- set from
// CreateCharge's own subscription_data.metadata (gateway.go) when the
// subscription was first created -- as an immutable copy under
// parent.subscription_details.metadata, captured at the invoice's
// finalization. That is the real, documented delivery shape (stripe-go
// v82.5.1's own field comment on InvoiceParentSubscriptionDetails.Metadata:
// "Set of key-value pairs defined as subscription metadata when an invoice
// is created. Becomes an immutable snapshot of the subscription metadata at
// the time of invoice finalization"); Stripe does not copy the keys onto
// the invoice's own metadata field, so reading inv.Metadata -- as an
// earlier revision of this function did, with a unit fixture hand-seeded to
// match -- sees nothing on a real delivery and refuses every renewal event
// (P1-3). An invoice whose parent is missing, is not a subscription, or
// whose snapshot carries no speed_* keys is refused exactly like any other
// uncorrelatable event: the only invoices this package's subscription-mode
// Checkout flow ever produces are subscription invoices of the platform's
// own subscriptions, so there is no legitimate invoice.paid delivery
// without the snapshot keys.
//
// NormalizedEvent.InvoiceID here is a KNOWN LIMITATION worth stating
// plainly rather than leaving implicit: the parent snapshot carries forward
// the id CreateCharge attached to the FIRST cycle's own billing.Invoice,
// verbatim, on every later cycle's Stripe-generated invoice -- this package
// has no way to mint a fresh billing.Invoice id for a cycle it was never
// asked to create one for (CreateCharge is called exactly once, at
// Subscription activation; docs/internal/06's own
// domestic-plus-international dual payment mode write-up and this package's
// own doc.go explain why). A later round's live webhook processing loop
// (go/billing/AGENTS.md's own Known limitations records that no such loop
// exists yet) is where a genuine per-cycle billing.Invoice would need to be
// created before this identifier round-trips meaningfully past cycle one.
func normalizeInvoice(event stripego.Event, rawBody []byte) (billing.NormalizedEvent, error) {
	var inv stripego.Invoice
	if err := json.Unmarshal(event.Data.Raw, &inv); err != nil {
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithCause(err)
	}

	var metadata map[string]string
	if parent := inv.Parent; parent != nil && parent.SubscriptionDetails != nil {
		metadata = parent.SubscriptionDetails.Metadata
	}
	tenantID, subscriptionID, invoiceID, err := metadataIdentifiers(metadata)
	if err != nil {
		return billing.NormalizedEvent{}, err
	}

	var eventType billing.NormalizedEventType
	var status billing.ChannelStatus
	var amountCents int64
	switch string(event.Type) {
	case eventTypeInvoicePaid:
		eventType = billing.NormalizedEventChargeSucceeded
		status = billing.ChannelStatusSucceeded
		amountCents = inv.AmountPaid
	case eventTypeInvoicePaymentFailed:
		eventType = billing.NormalizedEventChargeFailed
		status = billing.ChannelStatusFailed
		// AmountDue, not AmountPaid: a failed attempt collected nothing, but
		// the amount the attempt was FOR is still meaningful, mirroring
		// normalizeCheckoutSession's own eventTypeCheckoutSessionExpired/
		// eventTypeCheckoutSessionAsyncFailed branch, which reports
		// sess.AmountTotal (the amount that would have been charged) rather
		// than a zero-valued Money.
		amountCents = inv.AmountDue
	}

	return billing.NormalizedEvent{
		EventID:          event.ID,
		Channel:          "stripe",
		ChannelReference: billing.ChannelReference(inv.ID),
		TenantID:         tenantID,
		SubscriptionID:   subscriptionID,
		InvoiceID:        invoiceID,
		Type:             eventType,
		Status:           status,
		Amount:           billing.Money{Cents: amountCents, Currency: string(inv.Currency)},
		OccurredAt:       time.Unix(event.Created, 0).UTC(),
		RawPayload:       rawBody,
	}, nil
}

// normalizeSubscriptionUpdated handles customer.subscription.updated,
// recognized ONLY for a transition into stripego.SubscriptionStatusCanceled
// -- mapping onto the one entry in billing's own vocabulary built
// specifically for this (NormalizedEventSubscriptionCanceled/
// ChannelStatusCanceled). Every other Stripe subscription status
// (Active, PastDue, Trialing, Incomplete, IncompleteExpired, Paused,
// Unpaid) is refused as ErrWebhookPayloadUnrecognized rather than forced
// onto billing.SubscriptionStatus's own four-value vocabulary
// (Created/Active/PastDue/Canceled): the Active/PastDue-shaped transitions
// this event ALSO fires for are already announced, with a real settled
// amount attached, by invoice.paid/invoice.payment_failed above -- mapping
// them a second time here, with no amount to attach at all (a
// stripe.Subscription carries no Money of its own), would be inventing a
// second, amount-less signal for the same underlying fact rather than
// honestly representing something new.
func normalizeSubscriptionUpdated(event stripego.Event, rawBody []byte) (billing.NormalizedEvent, error) {
	var sub stripego.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithCause(err)
	}

	if sub.Status != stripego.SubscriptionStatusCanceled {
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithParam("reason", "customer.subscription.updated status not recognized").WithParam("status", string(sub.Status))
	}

	tenantID, subscriptionID, invoiceID, err := metadataIdentifiers(sub.Metadata)
	if err != nil {
		return billing.NormalizedEvent{}, err
	}

	return billing.NormalizedEvent{
		EventID:          event.ID,
		Channel:          "stripe",
		ChannelReference: billing.ChannelReference(sub.ID),
		TenantID:         tenantID,
		SubscriptionID:   subscriptionID,
		InvoiceID:        invoiceID,
		Type:             billing.NormalizedEventSubscriptionCanceled,
		Status:           billing.ChannelStatusCanceled,
		// No Amount: a subscription-status transition carries no Money of
		// its own, exactly like NormalizedEventSubscriptionCanceled's own
		// doc comment (gateway.go) already states.
		OccurredAt: time.Unix(event.Created, 0).UTC(),
		RawPayload: rawBody,
	}, nil
}
