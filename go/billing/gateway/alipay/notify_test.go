package alipay

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"net/url"
	"testing"

	"github.com/vislake/speed/go/billing"
)

// signedNotifyBody builds a realistic Alipay async-notification body
// (application/x-www-form-urlencoded), signed with priv exactly the way a
// real Alipay server signs one -- this package's own locally-constructed
// fixture (doc.go's own testing-strategy section).
func signedNotifyBody(t *testing.T, priv *rsa.PrivateKey, params map[string]string) []byte {
	t.Helper()
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
