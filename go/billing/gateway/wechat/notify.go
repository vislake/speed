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
// documented transaction shape, the only resource shape this round decodes
// (see this file's own doc comment on eventTypeTransactionSuccess for what
// is deliberately NOT handled yet).
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

// eventTypeTransactionSuccess is the only WeChat Pay webhook event_type
// this package recognizes this round: a Native-order payment success.
// WeChat Pay also delivers "REFUND.SUCCESS"/"REFUND.ABNORMAL"/
// "REFUND.CLOSED" notifications for a refunded order (a distinct resource
// shape from transactionResource above, requiring its own decode path this
// round does not implement -- see
// go/billing/gateway/AGENTS.md's Known limitations) -- every other
// event_type is refused as ErrWebhookPayloadUnrecognized, never guessed at.
const eventTypeTransactionSuccess = "TRANSACTION.SUCCESS"

// VerifyWebhook implements billing.PaymentGateway. It performs no network
// call: VerifySignature recomputes WeChat Pay's own documented
// RSA-SHA256 signature over timestamp+"\n"+nonce+"\n"+body+"\n" (the exact
// headers WeChat Pay's own delivery carries: Wechatpay-Signature,
// Wechatpay-Timestamp, Wechatpay-Nonce) against the configured platform
// public key, and decryptResource then decrypts the envelope's
// AEAD_AES_256_GCM resource ciphertext against the configured APIv3 key --
// a second, independent integrity check beneath the outer signature (see
// decryptResource's own doc comment).
func (g *Gateway) VerifyWebhook(_ context.Context, headers map[string][]string, body []byte) (billing.NormalizedEvent, error) {
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
	if envelope.EventType != eventTypeTransactionSuccess {
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithParam("event_type", envelope.EventType)
	}

	plaintext, err := decryptResource(
		envelope.Resource.Algorithm, envelope.Resource.Nonce, envelope.Resource.AssociatedData,
		envelope.Resource.Ciphertext, g.cfg.APIv3Key,
	)
	if err != nil {
		return billing.NormalizedEvent{}, billing.ErrWebhookPayloadUnrecognized.WithCause(err)
	}

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

func firstHeader(headers map[string][]string, name string) string {
	for _, v := range headers[name] {
		return v
	}
	return ""
}
