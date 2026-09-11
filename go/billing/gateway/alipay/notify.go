package alipay

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/vislake/speed/go/billing"
)

// notifyTimeTolerance is how far a delivery's notify_time parameter may sit
// from the current time, in either direction, and still be accepted -- the
// same 300-second window stripe-go's own webhook handling applies to Stripe
// deliveries (webhook.DefaultTolerance, enforced by this module's stripe
// leg through ConstructEventWithOptions) and go/billing/gateway/wechat
// applies to its own Wechatpay-Timestamp header
// (notifyTimestampTolerance), each bounding the delivery's age so a
// captured notification -- whose signature stays valid forever -- cannot be
// replayed at arbitrary leisure.
//
// Alipay async notifications carry no signature-bearing HTTP header: the
// RSA2 signature covers the form parameters themselves (notify_time
// included, sign.go's VerifySignature), so the freshness ceiling reads the
// signed notify_time parameter. notify_time is the time Alipay generated
// THIS delivery attempt -- each attempt carries its own notify_id, and the
// trade's own event times ride in the gmt_* parameters -- so bounding it
// cannot age out legitimate redeliveries the way bounding gmt_payment
// would. A delivery refused here is refused with the same
// authentication-class error a stale Stripe or WeChat delivery gets in
// this module's other legs: the signature itself verified, but the
// delivery is not accepted as live. The symmetric window also absorbs
// ordinary clock skew between this host and Alipay's notify servers.
const notifyTimeTolerance = 300 * time.Second

// VerifyWebhook implements billing.PaymentGateway. Alipay's async
// notification is a plain application/x-www-form-urlencoded POST body, not
// a JSON envelope, and carries no signature-bearing HTTP header -- the
// signature lives in the body's own "sign" parameter, verified against
// every other parameter's canonical form (sign.go's VerifySignature,
// implementing https://opendocs.alipay.com/common/02kdnc's documented RSA2
// scheme exactly). headers is accepted only for interface-shape parity
// with billing.PaymentGateway; this implementation reads nothing from it.
// After the signature verifies, the delivery's own signed notify_time
// parameter is bounded to notifyTimeTolerance of now before the payload is
// normalized -- the same delivery-freshness ceiling the stripe and wechat
// legs enforce (see notifyTimeTolerance's own doc comment).
func (g *Gateway) VerifyWebhook(ctx context.Context, _ map[string][]string, body []byte) (ev billing.NormalizedEvent, verifyErr error) {
	// billing.webhook.verify for this channel (billing root's
	// metrics.go): the outcome is derived from the returned error's
	// nilness, so a refusal added in a future branch counts itself
	// without a new record site. The context parameter is named here
	// for the recording call.
	defer func() {
		billing.RecordWebhookVerify(ctx, g.webhookVerify, "alipay", verifyErr)
	}()
	params, err := decodeFormValues(body)
	if err != nil {
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithCause(err)
	}

	if err := VerifySignature(params, g.keys.pub); err != nil {
		return billing.NormalizedEvent{}, billing.ErrWebhookSignatureInvalid.WithCause(err)
	}

	// Freshness: the delivery's own notify_time must sit within
	// notifyTimeTolerance of now, in either direction (see that constant's
	// doc comment for why notify_time -- signed above, unlike any HTTP
	// header Alipay carries -- is the field to bound, and why the check
	// runs only after the signature itself verified). An absent or
	// unparseable notify_time is refused the same way a stale one is:
	// there is nothing to bound.
	notifyTimeStr := params["notify_time"]
	if notifyTimeStr == "" {
		return billing.NormalizedEvent{}, billing.ErrWebhookSignatureInvalid.
			WithParam("reason", "notify_time missing: nothing to bound the delivery's age")
	}
	notifiedAt, parseErr := time.ParseInLocation(alipayTimeFormat, notifyTimeStr, alipayLocation())
	if parseErr != nil {
		return billing.NormalizedEvent{}, billing.ErrWebhookSignatureInvalid.
			WithParam("reason", "notify_time is not a valid Alipay timestamp").
			WithCause(parseErr)
	}
	skew := time.Since(notifiedAt)
	if skew > notifyTimeTolerance || skew < -notifyTimeTolerance {
		return billing.NormalizedEvent{}, billing.ErrWebhookSignatureInvalid.
			WithParam("reason", "notify_time is outside the accepted freshness window").
			WithParam("skew_seconds", strconv.FormatInt(int64(skew/time.Second), 10))
	}

	return normalizeNotify(params, body)
}

// notifyCarriesRefund reports whether a verified Alipay notify parameter
// set describes a REFUND of an already-paid trade, told apart from the
// trade's own payment by the notification's own parameters: an async
// notification carrying refund information includes refund_fee (the
// refunded amount, a decimal yuan string) and gmt_refund (the refund
// time). Alipay's status definitions
// (https://opendocs.alipay.com/open/194/103296, the same source
// tradeStatusToChannelStatus's doc comment cites) record that a paid
// trade only leaves TRADE_SUCCESS on a FULL refund -- which lands as a
// TRADE_CLOSED notification carrying those refund fields, where an unpaid
// trade closed by timeout lands as a TRADE_CLOSED notification carrying
// none -- and that a PARTIAL refund keeps the trade at TRADE_SUCCESS and
// re-sends the TRADE_SUCCESS notification, refund fields included, for
// every refund. refund_fee == "0.00" and an absent gmt_refund therefore
// mean no refund happened; a refund_fee that does not parse as a positive
// amount is treated as no refund rather than guessed at (the refunds this
// predicate cares about are always positive sums).
func notifyCarriesRefund(params map[string]string) bool {
	if params["gmt_refund"] != "" {
		return true
	}
	// The refund_fee leg is the SHARED detection with the poll path
	// (gateway.go's carriesRefund): an async notification and an
	// alipay.trade.query response mark a refund with the identical
	// decimal-yuan refund_fee parameter, so the two payload shapes must
	// never drift apart on what counts as a refund.
	return carriesRefund(params["refund_fee"])
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

	refund := notifyCarriesRefund(params)

	var eventType billing.NormalizedEventType
	var status billing.ChannelStatus
	switch tradeStatus {
	case "TRADE_SUCCESS", "TRADE_FINISHED":
		if refund {
			// A PARTIAL refund: the trade stays at TRADE_SUCCESS, and Alipay
			// re-sends the TRADE_SUCCESS notification for the refund,
			// refund_fee/gmt_refund included (see notifyCarriesRefund's own
			// doc comment). Mapping it to the same
			// NormalizedEventChargeSucceeded/EventID the original payment
			// produced would hand the payment_events insert-first-dedup
			// ledger a byte-identical duplicate of the payment, silently
			// swallowing the refund signal. This package's own posture
			// therefore mirrors the refusal go/billing/gateway/wechat
			// applies to the refund signals ITS vocabulary cannot represent
			// (REFUND.ABNORMAL/REFUND.CLOSED -- see that leg's decode and
			// the gateway AGENTS.md's refund-decode record): refused loudly
			// as ErrWebhookPayloadUnrecognized, never guessed at -- the
			// trade-notify vocabulary carries no per-refund-occurrence
			// identifier to build a dedup-safe NormalizedEventRefunded
			// EventID from, the occurrence identity (out_refund_no) that
			// makes wechat's own REFUND.* notifications decodable being
			// exactly what Alipay's trade notifications lack.
			return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.
				WithParam("reason", "TRADE_SUCCESS notification carries refund fields (refund_fee/gmt_refund): a partial-refund notification, which this round does not decode").
				WithParam("trade_status", tradeStatus)
		}
		eventType = billing.NormalizedEventChargeSucceeded
		status = billing.ChannelStatusSucceeded
	case "TRADE_CLOSED":
		if refund {
			// A FULL refund of an already-paid trade: per Alipay's own
			// status definitions, TRADE_CLOSED covers both an unpaid trade
			// closed by timeout and a paid trade closed by a full refund --
			// the refund fields on this delivery are what tell the two
			// apart (see notifyCarriesRefund's own doc comment). This is a
			// genuine refund of the earlier succeeded charge:
			// NormalizedEventRefunded/ChannelStatusRefunded, never the
			// ChannelStatusFailed an unpaid timeout gets. The EventID stays
			// the stable (out_trade_no, TRADE_CLOSED) pair -- distinct from
			// the payment's own (out_trade_no, TRADE_SUCCESS), so the dedup
			// ledger records both, and stable across Alipay's redeliveries
			// of this same status transition.
			eventType = billing.NormalizedEventRefunded
			status = billing.ChannelStatusRefunded
		} else {
			eventType = billing.NormalizedEventChargeFailed
			status = billing.ChannelStatusFailed
		}
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
	// The event's own timestamp: gmt_refund for a refund notification (the
	// refund happened at gmt_refund, not at the original gmt_payment), the
	// payment time otherwise. Best-effort exactly as before -- an
	// unparseable value falls back to now().
	gmtKey := "gmt_payment"
	if eventType == billing.NormalizedEventRefunded {
		gmtKey = "gmt_refund"
	}
	if gmtStr := params[gmtKey]; gmtStr != "" {
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
		// one dedup row the insert-first-dedup rule requires. A full-refund
		// TRADE_CLOSED and an unpaid-timeout
		// TRADE_CLOSED are DIFFERENT events about the same trade and
		// therefore share this id space only with redeliveries of
		// themselves (see the TRADE_CLOSED branch above for why the two
		// fates still normalize differently).
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
