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
// signedNotifyBodyAt is the same fixture with an explicit
// Wechatpay-Timestamp, so a test can build a genuinely fresh delivery and
// a deliberately stale one.
func signedNotifyBody(t *testing.T, platformPriv *rsa.PrivateKey, apiV3Key []byte, eventType string, txn transactionResource) (headers map[string][]string, body []byte) {
	t.Helper()
	return signedNotifyBodyAt(t, platformPriv, apiV3Key, eventType, txn, strconv.FormatInt(time.Now().Unix(), 10))
}

func signedNotifyBodyAt(t *testing.T, platformPriv *rsa.PrivateKey, apiV3Key []byte, eventType string, txn transactionResource, timestamp string) (headers map[string][]string, body []byte) {
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

// TestGateway_VerifyWebhook_StaleTimestamp_Refused is P3-13's regression
// test: VerifyWebhook used to check nothing about the delivery's
// Wechatpay-Timestamp -- a captured, replayed notification with a fully
// valid signature was accepted no matter how old it was. WeChat Pay's own
// notification scheme (like the signature-verification scheme this
// module's stripe leg already enforces through stripe-go's own timestamp
// tolerance) expects the receiver to bound the delivery's age: a
// notification older than the freshness window is a replay and is refused
// even though its signature verifies. The fix checks the header's Unix
// timestamp against the same 300-second tolerance stripe-go's webhook
// handling applies (its DefaultTolerance), refusing a delivery outside it
// with ErrWebhookSignatureInvalid exactly like any other
// authentication-class failure. On pre-fix code this stale-but-genuinely
// signed delivery is accepted and normalized, so the test fails before the
// fix and passes after.
func TestGateway_VerifyWebhook_StaleTimestamp_Refused(t *testing.T) {
	_, platformPubPEM, platformPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, platformPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	txn := testTransaction(t, "tenant-a", "sub-1", "inv-1", "ORD1", "SUCCESS", 2900)
	stale := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	headers, body := signedNotifyBodyAt(t, platformPriv, cfg.APIv3Key, eventTypeTransactionSuccess, txn, stale)

	_, err = gw.VerifyWebhook(context.Background(), headers, body)
	if !hasCode(err, billing.ErrWebhookSignatureInvalid.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookSignatureInvalid (a stale notification is a replay and must be refused)", err)
	}
}

// TestGateway_VerifyWebhook_CurrentTimestamp_StillAccepted pins the other
// side of P3-13: a genuinely fresh delivery keeps verifying exactly as
// before, so the freshness check cannot become overzealous.
func TestGateway_VerifyWebhook_CurrentTimestamp_StillAccepted(t *testing.T) {
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
	if event.EventID != "ORD1:SUCCESS" {
		t.Errorf("EventID = %q, want ORD1:SUCCESS", event.EventID)
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
