package wechat

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/vislake/speed/go/billing"
)

// signedNotifyBody builds a realistic WeChat Pay webhook envelope, its
// resource AEAD_AES_256_GCM-encrypted with apiV3Key and the whole body
// RSA-SHA256-signed with platformPriv -- this package's own
// locally-constructed fixture (doc.go's own testing-strategy section).
func signedNotifyBody(t *testing.T, platformPriv *rsa.PrivateKey, apiV3Key []byte, eventType string, txn transactionResource) (headers map[string][]string, body []byte) {
	t.Helper()
	plaintext, err := json.Marshal(txn)
	if err != nil {
		t.Fatalf("marshal transaction resource: %v", err)
	}
	const associatedData = "transaction"
	nonce, ciphertextB64 := encryptResourceForTest(t, plaintext, associatedData, apiV3Key)

	envelope := notifyEnvelope{
		ID:           "evt-1",
		CreateTime:   time.Now().Format(time.RFC3339),
		ResourceType: "encrypt-resource",
		EventType:    eventType,
	}
	envelope.Resource.Algorithm = algorithmAEADAES256GCM
	envelope.Resource.Ciphertext = ciphertextB64
	envelope.Resource.Nonce = nonce
	envelope.Resource.AssociatedData = associatedData

	body, err = json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	sigNonce := "notify-nonce"
	sig, err := signRequest(notifySignMessage(timestamp, sigNonce, string(body)), platformPriv)
	if err != nil {
		t.Fatalf("sign notify body: %v", err)
	}

	return map[string][]string{
		"Wechatpay-Signature": {sig},
		"Wechatpay-Timestamp": {timestamp},
		"Wechatpay-Nonce":     {sigNonce},
	}, body
}

func testTransaction(t *testing.T, tenantID, subID, invoiceID, outTradeNo, tradeState string, amountCents int64) transactionResource {
	t.Helper()
	attach, err := json.Marshal(attachPayload{TenantID: tenantID, SubscriptionID: subID, InvoiceID: invoiceID})
	if err != nil {
		t.Fatalf("marshal attach: %v", err)
	}
	return transactionResource{
		OutTradeNo:  outTradeNo,
		TradeState:  tradeState,
		Attach:      string(attach),
		SuccessTime: time.Now().Format(time.RFC3339),
		Amount: struct {
			Total    int64  `json:"total"`
			Currency string `json:"currency"`
		}{Total: amountCents, Currency: "CNY"},
	}
}

func TestGateway_VerifyWebhook_ValidNotification(t *testing.T) {
	_, platformPubPEM, platformPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, platformPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	txn := testTransaction(t, "tenant-a", "sub-1", "inv-1", "ORD1", "SUCCESS", 2900)
	headers, body := signedNotifyBody(t, platformPriv, cfg.APIv3Key, eventTypeTransactionSuccess, txn)

	event, err := gw.VerifyWebhook(context.Background(), headers, body)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if event.Channel != "wechat" {
		t.Errorf("Channel = %q, want wechat", event.Channel)
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
	if event.EventID != "ORD1:SUCCESS" {
		t.Errorf("EventID = %q, want ORD1:SUCCESS", event.EventID)
	}
}

func TestGateway_VerifyWebhook_InvalidSignature(t *testing.T) {
	_, platformPubPEM, _ := generateTestKeyPair(t)
	_, _, wrongPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, platformPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	txn := testTransaction(t, "t", "s", "i", "ORD1", "SUCCESS", 100)
	headers, body := signedNotifyBody(t, wrongPriv, cfg.APIv3Key, eventTypeTransactionSuccess, txn)

	_, err = gw.VerifyWebhook(context.Background(), headers, body)
	if !hasCode(err, billing.ErrWebhookSignatureInvalid.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookSignatureInvalid", err)
	}
}

func TestGateway_VerifyWebhook_WrongAPIv3Key(t *testing.T) {
	_, platformPubPEM, platformPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, platformPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	txn := testTransaction(t, "t", "s", "i", "ORD1", "SUCCESS", 100)
	wrongKey := []byte("11111111111111111111111111111111")[:32]
	headers, body := signedNotifyBody(t, platformPriv, wrongKey, eventTypeTransactionSuccess, txn) // encrypted under a DIFFERENT key than cfg.APIv3Key

	_, err = gw.VerifyWebhook(context.Background(), headers, body)
	if !hasCode(err, billing.ErrWebhookPayloadUnrecognized.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookPayloadUnrecognized", err)
	}
}

func TestGateway_VerifyWebhook_MissingSignatureHeaders(t *testing.T) {
	_, platformPubPEM, _ := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, platformPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	_, err = gw.VerifyWebhook(context.Background(), map[string][]string{}, []byte(`{}`))
	if !hasCode(err, billing.ErrWebhookSignatureInvalid.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookSignatureInvalid", err)
	}
}

func TestGateway_VerifyWebhook_UnrecognizedEventType(t *testing.T) {
	_, platformPubPEM, platformPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, platformPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	txn := testTransaction(t, "t", "s", "i", "ORD1", "SUCCESS", 100)
	headers, body := signedNotifyBody(t, platformPriv, cfg.APIv3Key, "REFUND.SUCCESS", txn)

	_, err = gw.VerifyWebhook(context.Background(), headers, body)
	if !hasCode(err, billing.ErrWebhookPayloadUnrecognized.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookPayloadUnrecognized", err)
	}
}

func TestGateway_VerifyWebhook_MissingAttach(t *testing.T) {
	_, platformPubPEM, platformPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, platformPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	txn := testTransaction(t, "", "", "", "ORD1", "SUCCESS", 100)
	txn.Attach = ""
	headers, body := signedNotifyBody(t, platformPriv, cfg.APIv3Key, eventTypeTransactionSuccess, txn)

	_, err = gw.VerifyWebhook(context.Background(), headers, body)
	if !hasCode(err, billing.ErrWebhookPayloadUnrecognized.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookPayloadUnrecognized", err)
	}
}
