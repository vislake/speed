package wechat

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/vislake/speed/go/billing"
)

// notifyTimestampTolerance is how far a delivery's Wechatpay-Timestamp may
// sit from the current time, in either direction, and still be accepted --
// the same 300-second window stripe-go's own webhook handling applies to
// Stripe deliveries (webhook.DefaultTolerance, enforced by this module's
// stripe leg through ConstructEventWithOptions), mirrored here per WeChat
// Pay's own notification guidance, which likewise tells receivers to bound
// the delivery's age so a captured notification cannot be replayed at
// arbitrary leisure. The symmetric window also absorbs ordinary clock skew
// between this host and WeChat Pay's signing servers.
const notifyTimestampTolerance = 300 * time.Second

// notifyEnvelope is WeChat Pay's own webhook delivery envelope --
// https://pay.weixin.qq.com/doc/v3/merchant/4012791862's documented shape.
// The "resource" field's ciphertext is opaque until decryptResource
// decrypts it against Config.APIv3Key.
type notifyEnvelope struct {
	ID           string `json:"id"`
	CreateTime   string `json:"create_time"`
	ResourceType string `json:"resource_type"`
	EventType    string `json:"event_type"`
	Resource     struct {
		Algorithm      string `json:"algorithm"`
		Ciphertext     string `json:"ciphertext"`
		Nonce          string `json:"nonce"`
		AssociatedData string `json:"associated_data"`
	} `json:"resource"`
}

// transactionResource is the decrypted plaintext of a "TRANSACTION.*"
// event's resource -- https://pay.weixin.qq.com/doc/v3/merchant/4012791863's
// documented transaction shape, decoded by decodeTransactionResource for
// the one transaction event type this package recognizes.
type transactionResource struct {
	OutTradeNo  string `json:"out_trade_no"`
	TradeState  string `json:"trade_state"`
	Attach      string `json:"attach"`
	SuccessTime string `json:"success_time"`
	Amount      struct {
		Total    int64  `json:"total"`
		Currency string `json:"currency"`
	} `json:"amount"`
}

// eventTypeTransactionSuccess is the WeChat Pay webhook event_type for a
// Native-order payment success -- the one transaction event type this
// package recognizes, decoded through decodeTransactionResource.
const eventTypeTransactionSuccess = "TRANSACTION.SUCCESS"

// refundResource is the decrypted plaintext of a "REFUND.*" event's
// resource -- https://pay.weixin.qq.com/doc/v3/merchant/4012791865's
// documented refund-result-notification shape, a distinct resource from
// transactionResource: the refund object carries the refund's own
// identifiers (out_refund_no, refund_id), the refunded order's
// out_trade_no, the refund's status and the money fields of the refund.
// Note what it deliberately does NOT carry: no attach field and no
// merchant-metadata equivalent of any kind (WeChat Pay's refund object
// defines none -- the merchant's refund-creation API has no attach
// parameter either), which is why a decoded refund event's TenantID/
// SubscriptionID/InvoiceID are empty BY NATURE of the payload -- see
// billing.NormalizedEvent's own doc comment (go/billing/gateway.go) for
// the recorded exception this decode path relies on.
type refundResource struct {
	OutTradeNo   string `json:"out_trade_no"`
	OutRefundNo  string `json:"out_refund_no"`
	RefundStatus string `json:"refund_status"`
	SuccessTime  string `json:"success_time"`
	Amount       struct {
		Total       int64 `json:"total"`
		Refund      int64 `json:"refund"`
		PayerTotal  int64 `json:"payer_total"`
		PayerRefund int64 `json:"payer_refund"`
	} `json:"amount"`
}

// The three WeChat Pay refund-result notification event types
// (https://pay.weixin.qq.com/doc/v3/merchant/4012791865): WeChat Pay
// delivers one of them for a refunded order, each carrying the same
// refundResource shape above with its own refund_status inside. Only
// REFUND.SUCCESS decodes into a billing.NormalizedEvent -- a refund that
// actually happened maps onto NormalizedEventRefunded; the other two
// report a refund that did NOT complete (see decodeRefundResource for the
// full argument) and are refused loudly rather than guessed at, exactly
// like every other payload this package cannot normalize truthfully.
const (
	eventTypeRefundSuccess  = "REFUND.SUCCESS"
	eventTypeRefundAbnormal = "REFUND.ABNORMAL"
	eventTypeRefundClosed   = "REFUND.CLOSED"
)

// refundStatusSuffix maps each recognized REFUND.* event type to the
// refund_status its decrypted resource must carry: WeChat Pay names a
// refund-result notification after the very status inside it, so a
// mismatch between the envelope's event_type and the resource's
// refund_status is a payload the channel never sends -- refused rather
// than decoded (decodeRefundResource's agreement check).
func refundStatusSuffix(eventType string) string {
	return eventType[len("REFUND."):]
}

// VerifyWebhook implements billing.PaymentGateway. It performs no network
// call: VerifySignature recomputes WeChat Pay's own documented
// RSA-SHA256 signature over timestamp+"\n"+nonce+"\n"+body+"\n" (the exact
// headers WeChat Pay's own delivery carries: Wechatpay-Signature,
// Wechatpay-Timestamp, Wechatpay-Nonce) against the configured platform
// public key, and decryptResource then decrypts the envelope's
// AEAD_AES_256_GCM resource ciphertext against the configured APIv3 key --
// a second, independent integrity check beneath the outer signature (see
// decryptResource's own doc comment).
func (g *Gateway) VerifyWebhook(ctx context.Context, headers map[string][]string, body []byte) (ev billing.NormalizedEvent, verifyErr error) {
	// billing.webhook.verify for this channel (billing root's
	// metrics.go): the outcome is derived from the returned error's
	// nilness, so a refusal added in a future branch counts itself
	// without a new record site. The previously unnamed _ context
	// gains its name here for the recording call.
	defer func() {
		billing.RecordWebhookVerify(ctx, g.webhookVerify, "wechat", verifyErr)
	}()
	sig := firstHeader(headers, "Wechatpay-Signature")
	timestamp := firstHeader(headers, "Wechatpay-Timestamp")
	nonce := firstHeader(headers, "Wechatpay-Nonce")
	if sig == "" || timestamp == "" || nonce == "" {
		return billing.NormalizedEvent{}, billing.ErrWebhookSignatureInvalid.WithParam("reason", "missing Wechatpay-Signature/Wechatpay-Timestamp/Wechatpay-Nonce header")
	}
	if err := VerifySignature(timestamp, nonce, body, sig, g.keys.platform); err != nil {
		return billing.NormalizedEvent{}, billing.ErrWebhookSignatureInvalid.WithCause(err)
	}

	// Freshness: the delivery's own timestamp must sit within
	// notifyTimestampTolerance of now, in either direction. A signature
	// proves WHO sent the body, never WHEN; without this bound a captured
	// notification (whose signature stays valid forever) could be replayed
	// at arbitrary leisure. A stale delivery is refused with the same
	// authentication-class error a stale Stripe delivery gets in this
	// module's stripe leg (whose SDK-level tolerance check surfaces through
	// ErrWebhookSignatureInvalid too) -- the signature itself verified, but
	// the delivery is not accepted as live. An unparseable timestamp is
	// refused the same way: there is nothing to bound.
	ts, tsErr := strconv.ParseInt(timestamp, 10, 64)
	if tsErr != nil {
		return billing.NormalizedEvent{}, billing.ErrWebhookSignatureInvalid.WithParam("reason", "Wechatpay-Timestamp is not a valid Unix timestamp").WithCause(tsErr)
	}
	deliveredAt := time.Unix(ts, 0)
	skew := time.Since(deliveredAt)
	if skew > notifyTimestampTolerance || skew < -notifyTimestampTolerance {
		return billing.NormalizedEvent{}, billing.ErrWebhookSignatureInvalid.
			WithParam("reason", "Wechatpay-Timestamp is outside the accepted freshness window").
			WithParam("skew_seconds", strconv.FormatInt(int64(skew/time.Second), 10))
	}

	var envelope notifyEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithCause(err)
	}

	// The event_type names the resource shape to decode: a TRANSACTION.*
	// notification's resource is a transaction object, a REFUND.*
	// notification's is a refund object (refundResource). Anything else is
	// refused on the event_type alone, before any decryption is attempted
	// -- the same "refused as ErrWebhookPayloadUnrecognized, never guessed
	// at" posture this package always applied to unrecognized types.
	var isRefund bool
	switch envelope.EventType {
	case eventTypeTransactionSuccess:
	case eventTypeRefundSuccess, eventTypeRefundAbnormal, eventTypeRefundClosed:
		isRefund = true
	default:
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithParam("event_type", envelope.EventType)
	}

	plaintext, err := decryptResource(
		envelope.Resource.Algorithm, envelope.Resource.Nonce, envelope.Resource.AssociatedData,
		envelope.Resource.Ciphertext, g.cfg.APIv3Key,
	)
	if err != nil {
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithCause(err)
	}

	if isRefund {
		return decodeRefundResource(envelope.EventType, plaintext, body)
	}
	return decodeTransactionResource(plaintext, body)
}

// decodeTransactionResource maps a decrypted TRANSACTION.SUCCESS resource
// onto a billing.NormalizedEvent -- the payment-success decode this package
// always shipped. The resource carries the merchant's own attach field
// (attachPayload), which is what identifies the event's tenant,
// subscription and invoice; a transaction-shaped payload missing those
// identifiers is refused rather than normalized with blanks (see
// billing.NormalizedEvent's own doc comment).
func decodeTransactionResource(plaintext, body []byte) (billing.NormalizedEvent, error) {
	var txn transactionResource
	if err := json.Unmarshal(plaintext, &txn); err != nil {
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithCause(err)
	}

	var attach attachPayload
	if txn.Attach != "" {
		if err := json.Unmarshal([]byte(txn.Attach), &attach); err != nil {
			return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithParam("reason", "attach is not valid JSON")
		}
	}
	if attach.TenantID == "" || attach.SubscriptionID == "" || attach.InvoiceID == "" {
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithParam("reason", "attach missing tenant/subscription/invoice identifiers")
	}

	status := tradeStateToChannelStatus(txn.TradeState)
	var eventType billing.NormalizedEventType
	switch status {
	case billing.ChannelStatusSucceeded:
		eventType = billing.NormalizedEventChargeSucceeded
	case billing.ChannelStatusFailed:
		eventType = billing.NormalizedEventChargeFailed
	default:
		// A TRANSACTION.SUCCESS event whose decrypted trade_state is
		// somehow not SUCCESS/CLOSED/REVOKED/PAYERROR is a genuine
		// surprise this package does not expect and refuses rather than
		// guessing a classification.
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithParam("trade_state", txn.TradeState)
	}

	occurredAt := time.Now().UTC()
	if txn.SuccessTime != "" {
		if t, err := time.Parse(time.RFC3339, txn.SuccessTime); err == nil {
			occurredAt = t.UTC()
		}
	}

	return billing.NormalizedEvent{
		// WeChat Pay's own envelope "id" is unique per delivery attempt,
		// the identical redelivery caveat go/billing/gateway/alipay's own
		// normalizeNotify documents for Alipay's notify_id -- the stable
		// key across redeliveries of the same event is (out_trade_no,
		// trade_state), mirrored here for the identical reason.
		EventID:          txn.OutTradeNo + ":" + txn.TradeState,
		Channel:          "wechat",
		ChannelReference: billing.ChannelReference(txn.OutTradeNo),
		TenantID:         attach.TenantID,
		SubscriptionID:   attach.SubscriptionID,
		InvoiceID:        attach.InvoiceID,
		Type:             eventType,
		Status:           status,
		Amount:           billing.Money{Cents: txn.Amount.Total, Currency: txn.Amount.Currency},
		OccurredAt:       occurredAt,
		RawPayload:       body,
	}, nil
}

// decodeRefundResource maps a decrypted REFUND.* resource onto a
// billing.NormalizedEvent. Only REFUND.SUCCESS decodes: a refund that
// actually happened is the vocabulary's NormalizedEventRefunded/
// ChannelStatusRefunded -- collapsed over partial and full refunds exactly
// as those types' own doc comments prescribe, with NormalizedEvent.Amount
// carrying the refunded amount (amount.refund, the field WeChat Pay's own
// refund notification defines for it). The envelope's event_type and the
// resource's refund_status must agree (WeChat Pay names each refund
// notification after the status inside it), and the two merchant-side
// identifiers the decode builds its key and reference from must be
// present; anything else is refused as ErrWebhookPayloadUnrecognized,
// never guessed at. The event's TenantID/SubscriptionID/InvoiceID are
// empty by nature: WeChat Pay's refund object carries no attach or
// merchant-metadata field of any kind (see refundResource's own doc
// comment and billing.NormalizedEvent's in go/billing/gateway.go), so the
// identifiers the transaction decode reads off its attach have no source
// here -- a caller that needs attribution resolves the event's
// ChannelReference (the refunded trade's out_trade_no) against its own
// records.
func decodeRefundResource(eventType string, plaintext, body []byte) (billing.NormalizedEvent, error) {
	var refund refundResource
	if err := json.Unmarshal(plaintext, &refund); err != nil {
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithCause(err)
	}

	if refund.RefundStatus != refundStatusSuffix(eventType) {
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.
			WithParam("reason", "refund_status does not match the notification's event_type").
			WithParam("event_type", eventType).
			WithParam("refund_status", refund.RefundStatus)
	}
	if refund.OutTradeNo == "" || refund.OutRefundNo == "" {
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.
			WithParam("reason", "refund notification missing out_trade_no/out_refund_no")
	}

	// REFUND.ABNORMAL and REFUND.CLOSED report a refund that did NOT
	// complete: the money was not returned to the payer, the original
	// charge stands, and the merchant must act through WeChat Pay's own
	// refund query/re-issue machinery (a refund marked abnormal has, for
	// example, failed at the payer's bank). The normalized vocabulary has
	// exactly one refund event -- NormalizedEventRefunded, "reports that a
	// previously succeeded charge was refunded" -- and mapping either of
	// these onto it would report a refund that never happened. They are
	// refused loudly, with the state named, exactly like the payloads this
	// package cannot normalize truthfully (compare alipay's refusal of a
	// partial-refund TRADE_SUCCESS notification); the refusal is the
	// merchant's signal to look the refund up.
	if refund.RefundStatus != "SUCCESS" {
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.
			WithParam("reason", "a REFUND.* notification whose refund_status reports a refund that did not complete has no truthful normalized shape; the charge stands and the refund must be handled through WeChat Pay's refund query/re-issue machinery").
			WithParam("event_type", eventType).
			WithParam("refund_status", refund.RefundStatus)
	}

	occurredAt := time.Now().UTC()
	if refund.SuccessTime != "" {
		if t, err := time.Parse(time.RFC3339, refund.SuccessTime); err == nil {
			occurredAt = t.UTC()
		}
	}

	return billing.NormalizedEvent{
		// The dedup-safe per-refund key: (out_refund_no, refund_status) is
		// stable across WeChat Pay's redeliveries of this same refund
		// notification, distinct from the payment's own
		// (out_trade_no, trade_state) EventID -- so the insert-first-dedup
		// ledger records both the payment and its refund -- and distinct
		// per refund occurrence, since each refund of an order carries its
		// own out_refund_no (the per-refund-occurrence identity alipay's
		// partial-refund refusal notes its own trade-notify vocabulary
		// lacks).
		EventID:          refund.OutRefundNo + ":" + refund.RefundStatus,
		Channel:          "wechat",
		ChannelReference: billing.ChannelReference(refund.OutTradeNo),
		Type:             billing.NormalizedEventRefunded,
		Status:           billing.ChannelStatusRefunded,
		// The refunded amount, not the order's total: WeChat Pay's refund
		// notification defines amount.refund as the refund's own money.
		// The resource carries no currency field -- the notification's
		// amount object defines only the four integer fields above, and a
		// WeChat Pay Native refund settles in CNY by definition (the
		// requireCNY gate on the creation side is this same order's
		// currency claim) -- so CNY is the one honest answer, mirroring
		// how the transaction decode reads its own currency field when the
		// channel echoes one.
		Amount:     billing.Money{Cents: refund.Amount.Refund, Currency: "CNY"},
		OccurredAt: occurredAt,
		RawPayload: body,
	}, nil
}

func firstHeader(headers map[string][]string, name string) string {
	for _, v := range headers[name] {
		return v
	}
	return ""
}
