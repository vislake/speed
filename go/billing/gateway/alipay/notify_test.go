package alipay

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/vislake/speed/go/billing"
)

// notifyNowString renders the current time in Alipay's own notify_time
// format ("yyyy-MM-dd HH:mm:ss", GMT+8 -- alipayTimeFormat/alipayLocation),
// for fixtures that must sit inside VerifyWebhook's freshness window.
func notifyNowString() string {
	return time.Now().In(alipayLocation()).Format(alipayTimeFormat)
}

// signedNotifyBody builds a realistic Alipay async-notification body
// (application/x-www-form-urlencoded), signed with priv exactly the way a
// real Alipay server signs one -- this package's own locally-constructed
// fixture (doc.go's own testing-strategy section). A fixture that does not
// set notify_time itself gets one stamped fresh into its own params map,
// since VerifyWebhook bounds every delivery's age to
// notifyTimeTolerance of now; a test exercising the stale or missing
// notify_time legs sets the parameter explicitly instead.
func signedNotifyBody(t *testing.T, priv *rsa.PrivateKey, params map[string]string) []byte {
	t.Helper()
	if _, ok := params["notify_time"]; !ok {
		params["notify_time"] = notifyNowString()
	}
	sig, err := signParams(params, priv)
	if err != nil {
		t.Fatalf("signParams: %v", err)
	}
	values := url.Values{}
	for k, v := range params {
		values.Set(k, v)
	}
	values.Set("sign", sig)
	values.Set("sign_type", "RSA2")
	return []byte(values.Encode())
}

func TestGateway_VerifyWebhook_ValidNotification(t *testing.T) {
	_, alipayPubPEM, alipayPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, alipayPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	passback, err := json.Marshal(passbackPayload{TenantID: "tenant-a", SubscriptionID: "sub-1", InvoiceID: "inv-1"})
	if err != nil {
		t.Fatalf("marshal passback: %v", err)
	}

	body := signedNotifyBody(t, alipayPriv, map[string]string{
		"notify_id":       "notify_1",
		"out_trade_no":    "ORD1",
		"trade_no":        "2026090422001",
		"trade_status":    "TRADE_SUCCESS",
		"total_amount":    "29.00",
		"passback_params": string(passback),
	})

	event, err := gw.VerifyWebhook(context.Background(), nil, body)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if event.Channel != "alipay" {
		t.Errorf("Channel = %q, want alipay", event.Channel)
	}
	if event.ChannelReference != "ORD1" {
		t.Errorf("ChannelReference = %q, want ORD1", event.ChannelReference)
	}
	if event.TenantID != "tenant-a" || event.SubscriptionID != "sub-1" || event.InvoiceID != "inv-1" {
		t.Errorf("identifiers = %+v", event)
	}
	if event.Type != billing.NormalizedEventChargeSucceeded {
		t.Errorf("Type = %q, want charge_succeeded", event.Type)
	}
	if event.Status != billing.ChannelStatusSucceeded {
		t.Errorf("Status = %q, want succeeded", event.Status)
	}
	if event.Amount.Cents != 2900 || event.Amount.Currency != "CNY" {
		t.Errorf("Amount = %+v", event.Amount)
	}
	// The EventID must be stable across a redelivery carrying a DIFFERENT
	// notify_id but the same (out_trade_no, trade_status) -- see
	// normalizeNotify's own doc comment for why.
	if event.EventID != "ORD1:TRADE_SUCCESS" {
		t.Errorf("EventID = %q, want ORD1:TRADE_SUCCESS", event.EventID)
	}
}

func TestGateway_VerifyWebhook_RedeliveryProducesSameEventID(t *testing.T) {
	_, alipayPubPEM, alipayPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, alipayPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}
	passback, _ := json.Marshal(passbackPayload{TenantID: "t", SubscriptionID: "s", InvoiceID: "i"})

	first := signedNotifyBody(t, alipayPriv, map[string]string{
		"notify_id": "attempt-1", "out_trade_no": "ORD9", "trade_status": "TRADE_SUCCESS",
		"total_amount": "10.00", "passback_params": string(passback),
	})
	second := signedNotifyBody(t, alipayPriv, map[string]string{
		"notify_id": "attempt-2", "out_trade_no": "ORD9", "trade_status": "TRADE_SUCCESS", // different notify_id, same event
		"total_amount": "10.00", "passback_params": string(passback),
	})

	e1, err := gw.VerifyWebhook(context.Background(), nil, first)
	if err != nil {
		t.Fatalf("VerifyWebhook(first): %v", err)
	}
	e2, err := gw.VerifyWebhook(context.Background(), nil, second)
	if err != nil {
		t.Fatalf("VerifyWebhook(second): %v", err)
	}
	if e1.EventID != e2.EventID {
		t.Errorf("EventID differs across redelivery: %q vs %q", e1.EventID, e2.EventID)
	}
}

func TestGateway_VerifyWebhook_InvalidSignature(t *testing.T) {
	_, alipayPubPEM, _ := generateTestKeyPair(t)
	_, _, wrongPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, alipayPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	body := signedNotifyBody(t, wrongPriv, map[string]string{
		"out_trade_no": "ORD1", "trade_status": "TRADE_SUCCESS", "total_amount": "1.00",
	})

	_, err = gw.VerifyWebhook(context.Background(), nil, body)
	if !hasCode(err, billing.ErrWebhookSignatureInvalid.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookSignatureInvalid", err)
	}
}

func TestGateway_VerifyWebhook_MissingPassback(t *testing.T) {
	_, alipayPubPEM, alipayPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, alipayPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	body := signedNotifyBody(t, alipayPriv, map[string]string{
		"out_trade_no": "ORD1", "trade_status": "TRADE_SUCCESS", "total_amount": "1.00",
	})

	_, err = gw.VerifyWebhook(context.Background(), nil, body)
	if !hasCode(err, billing.ErrWebhookPayloadUnrecognized.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookPayloadUnrecognized", err)
	}
}

// passbackFixture returns a marshalled passbackPayload every notify test
// below can embed.
func passbackFixture(t *testing.T) string {
	t.Helper()
	passback, err := json.Marshal(passbackPayload{TenantID: "tenant-a", SubscriptionID: "sub-1", InvoiceID: "inv-1"})
	if err != nil {
		t.Fatalf("marshal passback: %v", err)
	}
	return string(passback)
}

// TestGateway_VerifyWebhook_PartialRefundNotify_IsRefusedNotMistakenForThePayment
// is P1-4's regression for the partial-refund leg. Alipay keeps a
// partially-refunded trade at TRADE_SUCCESS and re-sends the TRADE_SUCCESS
// async notification for the refund (its order status only moves on a FULL
// refund), carrying the refund's own parameters -- refund_fee and
// gmt_refund -- alongside the trade's. On pre-fix code normalizeNotify
// mapped that delivery to the identical
// NormalizedEventChargeSucceeded/EventID "ORD1:TRADE_SUCCESS" the original
// payment produced, so the payment_events insert-first-dedup ledger
// swallowed the refund signal as a duplicate of the payment -- the platform
// would never learn a refund happened. This round's posture mirrors
// go/billing/gateway/wechat's own REFUND.* handling exactly: the delivery
// is refused loudly as ErrWebhookPayloadUnrecognized rather than guessed
// at, because the trade-notify vocabulary carries no per-refund-occurrence
// identifier to build a dedup-safe NormalizedEventRefunded EventID from
// (see go/billing/gateway/AGENTS.md's "refund notifications are not
// decoded" note).
func TestGateway_VerifyWebhook_PartialRefundNotify_IsRefusedNotMistakenForThePayment(t *testing.T) {
	_, alipayPubPEM, alipayPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, alipayPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	body := signedNotifyBody(t, alipayPriv, map[string]string{
		"notify_id":       "notify_refund_1",
		"out_trade_no":    "ORD1",
		"trade_no":        "2026090422001",
		"trade_status":    "TRADE_SUCCESS",
		"total_amount":    "29.00",
		"refund_fee":      "10.00",
		"gmt_refund":      "2026-09-05 10:00:00",
		"passback_params": passbackFixture(t),
	})

	_, err = gw.VerifyWebhook(context.Background(), nil, body)
	if !hasCode(err, billing.ErrWebhookPayloadUnrecognized.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookPayloadUnrecognized (a partial-refund notification must never normalize into an event byte-identical to the payment, which the dedup ledger would swallow)", err)
	}
}

// TestGateway_VerifyWebhook_FullRefundTradeClosed_MapsToRefunded is P1-4's
// regression for the full-refund leg: Alipay's own status definitions say
// TRADE_CLOSED covers two distinct fates -- an unpaid trade closed by
// timeout, and a PAID trade closed by a full refund (only a full refund
// moves the order off TRADE_SUCCESS). The two are told apart by the
// notification's own parameters: the full-refund delivery carries
// refund_fee (the refunded amount) and gmt_refund (the refund time); the
// timeout delivery carries neither. A TRADE_CLOSED notification bearing
// those refund markers therefore reports a refund of the earlier succeeded
// charge and must map to NormalizedEventRefunded/ChannelStatusRefunded --
// never to the ChannelStatusFailed a timed-out unpaid order gets. On
// pre-fix code every TRADE_CLOSED mapped to charge_failed, so a fully
// refunded charge was recorded as a failed payment and the refund signal
// was lost.
func TestGateway_VerifyWebhook_FullRefundTradeClosed_MapsToRefunded(t *testing.T) {
	_, alipayPubPEM, alipayPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, alipayPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	body := signedNotifyBody(t, alipayPriv, map[string]string{
		"notify_id":       "notify_full_refund_1",
		"out_trade_no":    "ORD1",
		"trade_no":        "2026090422001",
		"trade_status":    "TRADE_CLOSED",
		"total_amount":    "29.00",
		"refund_fee":      "29.00",
		"gmt_payment":     "2026-09-04 10:00:00",
		"gmt_refund":      "2026-09-05 10:30:00",
		"passback_params": passbackFixture(t),
	})

	event, err := gw.VerifyWebhook(context.Background(), nil, body)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if event.Type != billing.NormalizedEventRefunded {
		t.Errorf("Type = %q, want refunded (a TRADE_CLOSED notification carrying refund_fee/gmt_refund is a full refund of a paid trade)", event.Type)
	}
	if event.Status != billing.ChannelStatusRefunded {
		t.Errorf("Status = %q, want refunded", event.Status)
	}
	if event.EventID != "ORD1:TRADE_CLOSED" {
		t.Errorf("EventID = %q, want ORD1:TRADE_CLOSED (stable across redeliveries, distinct from the payment's ORD1:TRADE_SUCCESS)", event.EventID)
	}
	if event.Amount.Cents != 2900 || event.Amount.Currency != "CNY" {
		t.Errorf("Amount = %+v, want the trade's CNY 29.00", event.Amount)
	}
}

// TestGateway_VerifyWebhook_TradeClosedUnpaidTimeout_StillChargeFailed pins
// the other half of the TRADE_CLOSED distinction: a trade closed by
// timeout without ever being paid carries no refund markers (no refund_fee,
// no gmt_refund) and keeps mapping to NormalizedEventChargeFailed/
// ChannelStatusFailed, exactly as it always has -- the two TRADE_CLOSED
// fates must produce two different events, never be conflated in either
// direction.
func TestGateway_VerifyWebhook_TradeClosedUnpaidTimeout_StillChargeFailed(t *testing.T) {
	_, alipayPubPEM, alipayPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, alipayPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	body := signedNotifyBody(t, alipayPriv, map[string]string{
		"notify_id":       "notify_timeout_1",
		"out_trade_no":    "ORD1",
		"trade_no":        "2026090422001",
		"trade_status":    "TRADE_CLOSED",
		"total_amount":    "29.00",
		"gmt_close":       "2026-09-04 11:00:00",
		"passback_params": passbackFixture(t),
	})

	event, err := gw.VerifyWebhook(context.Background(), nil, body)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if event.Type != billing.NormalizedEventChargeFailed {
		t.Errorf("Type = %q, want charge_failed (a TRADE_CLOSED without refund markers is an unpaid trade closed by timeout)", event.Type)
	}
	if event.Status != billing.ChannelStatusFailed {
		t.Errorf("Status = %q, want failed", event.Status)
	}
	if event.EventID != "ORD1:TRADE_CLOSED" {
		t.Errorf("EventID = %q, want ORD1:TRADE_CLOSED", event.EventID)
	}
}

func TestGateway_VerifyWebhook_UnrecognizedTradeStatus(t *testing.T) {
	_, alipayPubPEM, alipayPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, alipayPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}
	passback, _ := json.Marshal(passbackPayload{TenantID: "t", SubscriptionID: "s", InvoiceID: "i"})

	body := signedNotifyBody(t, alipayPriv, map[string]string{
		"out_trade_no": "ORD1", "trade_status": "WAIT_BUYER_PAY", "total_amount": "1.00",
		"passback_params": string(passback),
	})

	_, err = gw.VerifyWebhook(context.Background(), nil, body)
	if !hasCode(err, billing.ErrWebhookPayloadUnrecognized.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookPayloadUnrecognized", err)
	}
}

// TestGateway_VerifyWebhook_StaleNotification_Refused is P2-21's
// regression: VerifyWebhook must bound the delivery's age the way the
// stripe and wechat legs do. Alipay's signature covers the form parameters
// (notify_time included) and stays valid forever, so on pre-fix code a
// captured notification could be replayed at arbitrary leisure and
// normalized as live. The fix refuses a delivery whose signed notify_time
// sits more than notifyTimeTolerance from now with the same
// authentication-class error a stale Stripe or WeChat delivery gets.
func TestGateway_VerifyWebhook_StaleNotification_Refused(t *testing.T) {
	_, alipayPubPEM, alipayPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, alipayPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	stale := time.Now().Add(-10 * time.Minute).In(alipayLocation()).Format(alipayTimeFormat)
	body := signedNotifyBody(t, alipayPriv, map[string]string{
		"notify_id":       "notify_stale_1",
		"notify_time":     stale,
		"out_trade_no":    "ORD1",
		"trade_no":        "2026090422001",
		"trade_status":    "TRADE_SUCCESS",
		"total_amount":    "29.00",
		"passback_params": passbackFixture(t),
	})

	_, err = gw.VerifyWebhook(context.Background(), nil, body)
	if !hasCode(err, billing.ErrWebhookSignatureInvalid.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookSignatureInvalid (a signature that verified must not let a stale delivery be replayed as live)", err)
	}
}

// TestGateway_VerifyWebhook_MissingNotifyTime_Refused pins the
// nothing-to-bound leg of the same freshness ceiling: a delivery whose
// signed params carry no notify_time at all is refused the same way a
// stale one is, since there is no delivery timestamp the check could bound
// (real Alipay async notifications always carry notify_time).
func TestGateway_VerifyWebhook_MissingNotifyTime_Refused(t *testing.T) {
	_, alipayPubPEM, alipayPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, alipayPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	body := signedNotifyBody(t, alipayPriv, map[string]string{
		"notify_id":       "notify_no_time_1",
		"notify_time":     "",
		"out_trade_no":    "ORD1",
		"trade_no":        "2026090422001",
		"trade_status":    "TRADE_SUCCESS",
		"total_amount":    "29.00",
		"passback_params": passbackFixture(t),
	})

	_, err = gw.VerifyWebhook(context.Background(), nil, body)
	if !hasCode(err, billing.ErrWebhookSignatureInvalid.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookSignatureInvalid (a delivery without notify_time has nothing to bound)", err)
	}
}
