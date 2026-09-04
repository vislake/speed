package alipay

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/vislake/speed/go/billing"
)

// VerifyWebhook implements billing.PaymentGateway. Alipay's async
// notification is a plain application/x-www-form-urlencoded POST body, not
// a JSON envelope, and carries no signature-bearing HTTP header -- the
// signature lives in the body's own "sign" parameter, verified against
// every other parameter's canonical form (sign.go's VerifySignature,
// implementing https://opendocs.alipay.com/common/02kdnc's documented RSA2
// scheme exactly). headers is accepted only for interface-shape parity
// with billing.PaymentGateway; this implementation reads nothing from it.
func (g *Gateway) VerifyWebhook(_ context.Context, _ map[string][]string, body []byte) (billing.NormalizedEvent, error) {
	params, err := decodeFormValues(body)
	if err != nil {
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithCause(err)
	}

	if err := VerifySignature(params, g.keys.pub); err != nil {
		return billing.NormalizedEvent{}, billing.ErrWebhookSignatureInvalid.WithCause(err)
	}

	return normalizeNotify(params, body)
}

// normalizeNotify maps a verified Alipay notify parameter set onto a
// billing.NormalizedEvent.
func normalizeNotify(params map[string]string, rawBody []byte) (billing.NormalizedEvent, error) {
	outTradeNo := params["out_trade_no"]
	tradeStatus := params["trade_status"]
	if outTradeNo == "" || tradeStatus == "" {
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithParam("reason", "missing out_trade_no or trade_status")
	}

	var passback passbackPayload
	if raw := params["passback_params"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &passback); err != nil {
			return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithParam("reason", "passback_params is not valid JSON")
		}
	}
	if passback.TenantID == "" || passback.SubscriptionID == "" || passback.InvoiceID == "" {
		// Missing the identifiers CreateCharge always attaches -- either a
		// notification for an order this package never created, or a
		// genuine data-shape surprise. Refused, per VerifyWebhook's own
		// contract, rather than passed upstream with blank identifiers.
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithParam("reason", "passback_params missing tenant/subscription/invoice identifiers")
	}

	var eventType billing.NormalizedEventType
	var status billing.ChannelStatus
	switch tradeStatus {
	case "TRADE_SUCCESS", "TRADE_FINISHED":
		eventType = billing.NormalizedEventChargeSucceeded
		status = billing.ChannelStatusSucceeded
	case "TRADE_CLOSED":
		eventType = billing.NormalizedEventChargeFailed
		status = billing.ChannelStatusFailed
	default:
		// WAIT_BUYER_PAY and any other value this package does not expect
		// to ever be notified about (Alipay's own docs say only these four
		// statuses are ever delivered as async notifications, and
		// WAIT_BUYER_PAY never is) -- refused as unrecognized rather than
		// guessed at.
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithParam("trade_status", tradeStatus)
	}

	amountStr := params["total_amount"]
	var amount billing.Money
	if amountStr != "" {
		cents, err := parseAmount(amountStr)
		if err != nil {
			return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithCause(err)
		}
		amount = billing.Money{Cents: cents, Currency: "CNY"}
	}

	occurredAt := time.Now().UTC()
	if gmtStr := params["gmt_payment"]; gmtStr != "" {
		if t, err := time.ParseInLocation(alipayTimeFormat, gmtStr, alipayLocation()); err == nil {
			occurredAt = t.UTC()
		}
	}

	return billing.NormalizedEvent{
		// Alipay's own notify_id is unique per DELIVERY ATTEMPT, not per
		// underlying trade event -- a redelivery of the same event carries
		// a DIFFERENT notify_id, which would defeat payment_events'
		// insert-first-dedup rule entirely if used as EventID. The stable
		// key across redeliveries of the same event is the (out_trade_no,
		// trade_status) pair itself: Alipay redelivers the identical
		// status transition verbatim until acknowledged, so this
		// synthesized id collapses every redelivery of one event into the
		// one dedup row docs/internal/06-billing-and-metering.md's rule
		// requires.
		EventID:          fmt.Sprintf("%s:%s", outTradeNo, tradeStatus),
		Channel:          "alipay",
		ChannelReference: billing.ChannelReference(outTradeNo),
		TenantID:         passback.TenantID,
		SubscriptionID:   passback.SubscriptionID,
		InvoiceID:        passback.InvoiceID,
		Type:             eventType,
		Status:           status,
		Amount:           amount,
		OccurredAt:       occurredAt,
		RawPayload:       rawBody,
	}, nil
}
