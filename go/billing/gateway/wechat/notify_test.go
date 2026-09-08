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
// a deliberately stale one. signedRefundNotifyBody is the refund-shaped
// twin (a refundResource under a REFUND.* event type, encrypted with the
// associated data "refund" the way a real refund notification's envelope
// carries its own), and signedRefundNotifyBodyAt its explicit-timestamp
// variant. All four share signedNotifyBodyCore below; the event type is
// the only thing that routes a fixture toward one resource shape or the
// other, exactly as it does in VerifyWebhook itself.
func signedNotifyBody(t *testing.T, platformPriv *rsa.PrivateKey, apiV3Key []byte, eventType string, txn transactionResource) (headers map[string][]string, body []byte) {
	t.Helper()
	return signedNotifyBodyAt(t, platformPriv, apiV3Key, eventType, txn, strconv.FormatInt(time.Now().Unix(), 10))
}

func signedNotifyBodyAt(t *testing.T, platformPriv *rsa.PrivateKey, apiV3Key []byte, eventType string, txn transactionResource, timestamp string) (headers map[string][]string, body []byte) {
	t.Helper()
	return signedNotifyBodyCore(t, platformPriv, apiV3Key, eventType, txn, "transaction", timestamp)
}

func signedRefundNotifyBody(t *testing.T, platformPriv *rsa.PrivateKey, apiV3Key []byte, eventType string, refund refundResource) (headers map[string][]string, body []byte) {
	t.Helper()
	return signedRefundNotifyBodyAt(t, platformPriv, apiV3Key, eventType, refund, strconv.FormatInt(time.Now().Unix(), 10))
}

func signedRefundNotifyBodyAt(t *testing.T, platformPriv *rsa.PrivateKey, apiV3Key []byte, eventType string, refund refundResource, timestamp string) (headers map[string][]string, body []byte) {
	t.Helper()
	return signedNotifyBodyCore(t, platformPriv, apiV3Key, eventType, refund, "refund", timestamp)
}

func signedNotifyBodyCore(t *testing.T, platformPriv *rsa.PrivateKey, apiV3Key []byte, eventType string, resource any, associatedData, timestamp string) (headers map[string][]string, body []byte) {
	t.Helper()
	plaintext, err := json.Marshal(resource)
	if err != nil {
		t.Fatalf("marshal resource: %v", err)
	}
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

// TestGateway_VerifyWebhook_UnrecognizedEventType pins the event-type
// dispatch's refusal half with an event type this package genuinely does
// not recognize (WeChat Pay's PAYSCORE family, say): refused as
// ErrWebhookPayloadUnrecognized on the event_type alone, before any
// decryption is attempted.
func TestGateway_VerifyWebhook_UnrecognizedEventType(t *testing.T) {
	_, platformPubPEM, platformPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, platformPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	txn := testTransaction(t, "t", "s", "i", "ORD1", "SUCCESS", 100)
	headers, body := signedNotifyBody(t, platformPriv, cfg.APIv3Key, "PAYSCORE.USER_OPEN_SERVICE", txn)

	_, err = gw.VerifyWebhook(context.Background(), headers, body)
	if !hasCode(err, billing.ErrWebhookPayloadUnrecognized.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookPayloadUnrecognized", err)
	}
}

// TestGateway_VerifyWebhook_RefundEventTypeWithTransactionResource_Refused
// pins the resource-shape half of the dispatch: an event type names the
// resource shape to decode, and a REFUND.SUCCESS envelope carrying a
// transaction-shaped resource (no out_refund_no/refund_status) is refused
// as unrecognized -- the envelope/resource pairing the channel never sends
// must not be guessed at. This is the pre-refund-decode behaviour's own
// shape (the old test signed a REFUND.SUCCESS envelope around a
// transaction resource and expected exactly this refusal), preserved now
// that a properly refund-shaped resource decodes.
func TestGateway_VerifyWebhook_RefundEventTypeWithTransactionResource_Refused(t *testing.T) {
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

// testRefund builds a realistic decrypted refund notification resource.
// outRefundNo is the merchant-chosen refund number, outTradeNo the
// refunded order's own, refundStatus one of SUCCESS/ABNORMAL/CLOSED, and
// refundCents the refunded amount (amount.refund), with totalCents the
// original order amount -- equal for a full refund, larger for a partial
// one.
func testRefund(t *testing.T, outTradeNo, outRefundNo, refundStatus string, totalCents, refundCents int64, successTime string) refundResource {
	t.Helper()
	if successTime == "" {
		successTime = time.Now().Format(time.RFC3339)
	}
	return refundResource{
		OutTradeNo:   outTradeNo,
		OutRefundNo:  outRefundNo,
		RefundStatus: refundStatus,
		SuccessTime:  successTime,
		Amount: struct {
			Total       int64 `json:"total"`
			Refund      int64 `json:"refund"`
			PayerTotal  int64 `json:"payer_total"`
			PayerRefund int64 `json:"payer_refund"`
		}{Total: totalCents, Refund: refundCents, PayerTotal: totalCents, PayerRefund: refundCents},
	}
}

// TestGateway_VerifyWebhook_RefundSuccess_DecodesToRefundedEvent is the
// refund decode's happy path: a REFUND.SUCCESS notification carrying a
// properly refund-shaped resource normalizes into
// NormalizedEventRefunded/ChannelStatusRefunded -- the vocabulary's one
// truthful shape for "a previously succeeded charge was refunded" -- with
// the dedup-safe per-refund EventID (out_refund_no:refund_status, stable
// across WeChat Pay's redeliveries of this same notification and distinct
// from the payment's own out_trade_no:SUCCESS id), the refunded trade as
// ChannelReference (the agreement point with QueryStatus, which reports
// ChannelStatusRefunded for the same order), the refunded amount in
// amount.refund, and the empty identifier fields the payload's own shape
// dictates (no attach exists on WeChat's refund object). Fails on the
// pre-decode code, where every REFUND.* event type was refused as
// ErrWebhookPayloadUnrecognized.
func TestGateway_VerifyWebhook_RefundSuccess_DecodesToRefundedEvent(t *testing.T) {
	_, platformPubPEM, platformPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, platformPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	successTime := "2026-09-01T10:20:30+08:00"
	refund := testRefund(t, "ORD1", "R1", "SUCCESS", 2900, 2900, successTime)
	headers, body := signedRefundNotifyBody(t, platformPriv, cfg.APIv3Key, "REFUND.SUCCESS", refund)

	event, err := gw.VerifyWebhook(context.Background(), headers, body)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if event.Channel != "wechat" {
		t.Errorf("Channel = %q, want wechat", event.Channel)
	}
	if event.Type != billing.NormalizedEventRefunded {
		t.Errorf("Type = %q, want refunded", event.Type)
	}
	if event.Status != billing.ChannelStatusRefunded {
		t.Errorf("Status = %q, want refunded", event.Status)
	}
	if event.EventID != "R1:SUCCESS" {
		t.Errorf("EventID = %q, want R1:SUCCESS (stable across redeliveries, distinct from the payment's own ORD1:SUCCESS)", event.EventID)
	}
	if event.ChannelReference != "ORD1" {
		t.Errorf("ChannelReference = %q, want ORD1 (the refunded trade, agreeing with QueryStatus's own answer for it)", event.ChannelReference)
	}
	if event.Amount.Cents != 2900 || event.Amount.Currency != "CNY" {
		t.Errorf("Amount = %+v, want the refunded amount 2900 CNY", event.Amount)
	}
	if event.TenantID != "" || event.SubscriptionID != "" || event.InvoiceID != "" {
		t.Errorf("identifiers = %q/%q/%q, want all empty (WeChat's refund object carries no attach or metadata field; see billing.NormalizedEvent's doc comment)", event.TenantID, event.SubscriptionID, event.InvoiceID)
	}
	wantTime, err := time.Parse(time.RFC3339, successTime)
	if err != nil {
		t.Fatalf("parse fixture success_time: %v", err)
	}
	if !event.OccurredAt.Equal(wantTime.UTC()) {
		t.Errorf("OccurredAt = %v, want the resource's own success_time %v", event.OccurredAt, wantTime.UTC())
	}
}

// TestGateway_VerifyWebhook_RefundSuccess_PartialRefundAmountsCollapse pins
// the partial-refund half of the collapse NormalizedEventRefunded/
// ChannelStatusRefunded document: a partial refund (amount.refund below
// amount.total) is still a refunded event -- the vocabulary collapses
// partial and full refunds to one value -- and NormalizedEvent.Amount
// carries the exact refunded amount for the caller that needs it.
func TestGateway_VerifyWebhook_RefundSuccess_PartialRefundAmountsCollapse(t *testing.T) {
	_, platformPubPEM, platformPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, platformPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	refund := testRefund(t, "ORD1", "R1", "SUCCESS", 2900, 900, "")
	headers, body := signedRefundNotifyBody(t, platformPriv, cfg.APIv3Key, "REFUND.SUCCESS", refund)

	event, err := gw.VerifyWebhook(context.Background(), headers, body)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if event.Type != billing.NormalizedEventRefunded || event.Status != billing.ChannelStatusRefunded {
		t.Errorf("event = %q/%q, want refunded/refunded (partial and full refunds collapse to the one value)", event.Type, event.Status)
	}
	if event.Amount.Cents != 900 {
		t.Errorf("Amount.Cents = %d, want 900 (the exact refunded amount, what the collapse's own doc comment sends a caller to NormalizedEvent.Amount for)", event.Amount.Cents)
	}
}

// TestGateway_VerifyWebhook_RefundAbnormalAndClosed_Refused pins the
// refusal half of the refund vocabulary: REFUND.ABNORMAL and REFUND.CLOSED
// report a refund that did NOT complete -- no money was returned, the
// original charge stands -- and the vocabulary has no truthful shape for
// that (NormalizedEventRefunded means "was refunded"), so both are refused
// as ErrWebhookPayloadUnrecognized with the state named, never mapped onto
// a refunded event that never happened. The merchant acts through WeChat
// Pay's own refund machinery; the loud refusal is the signal to look the
// refund up.
func TestGateway_VerifyWebhook_RefundAbnormalAndClosed_Refused(t *testing.T) {
	for _, status := range []string{"ABNORMAL", "CLOSED"} {
		t.Run(status, func(t *testing.T) {
			_, platformPubPEM, platformPriv := generateTestKeyPair(t)
			cfg := testGatewayConfig(t, platformPubPEM)
			gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
			if err != nil {
				t.Fatalf("newGatewayWithClient: %v", err)
			}

			refund := testRefund(t, "ORD1", "R1", status, 2900, 2900, "")
			headers, body := signedRefundNotifyBody(t, platformPriv, cfg.APIv3Key, "REFUND."+status, refund)

			_, err = gw.VerifyWebhook(context.Background(), headers, body)
			if !hasCode(err, billing.ErrWebhookPayloadUnrecognized.Code) {
				t.Errorf("err = %v, want billing.ErrWebhookPayloadUnrecognized for a REFUND.%s notification", err, status)
			}
		})
	}
}

// TestGateway_VerifyWebhook_RefundStatusEventTypeMismatch_Refused pins the
// agreement check: WeChat Pay names each refund notification after the
// refund_status inside its own resource, so an envelope whose event_type
// and resource disagree is a payload the channel never sends -- refused as
// unrecognized, never decoded (a REFUND.ABNORMAL envelope carrying a
// SUCCESS resource must not normalize into a refunded event).
func TestGateway_VerifyWebhook_RefundStatusEventTypeMismatch_Refused(t *testing.T) {
	_, platformPubPEM, platformPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, platformPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	refund := testRefund(t, "ORD1", "R1", "ABNORMAL", 2900, 2900, "")
	headers, body := signedRefundNotifyBody(t, platformPriv, cfg.APIv3Key, "REFUND.SUCCESS", refund)

	_, err = gw.VerifyWebhook(context.Background(), headers, body)
	if !hasCode(err, billing.ErrWebhookPayloadUnrecognized.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookPayloadUnrecognized for an event_type/refund_status mismatch", err)
	}
}

// TestGateway_VerifyWebhook_RefundMissingIdentifiers_Refused pins the
// decode's own required-fields gate: a REFUND.SUCCESS resource without the
// merchant-side identifiers the decode builds its key and reference from
// (out_refund_no, out_trade_no) is refused as unrecognized rather than
// normalized into an event with a blank EventID or ChannelReference.
func TestGateway_VerifyWebhook_RefundMissingIdentifiers_Refused(t *testing.T) {
	_, platformPubPEM, platformPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, platformPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	refund := testRefund(t, "", "R1", "SUCCESS", 2900, 2900, "")
	headers, body := signedRefundNotifyBody(t, platformPriv, cfg.APIv3Key, "REFUND.SUCCESS", refund)

	_, err = gw.VerifyWebhook(context.Background(), headers, body)
	if !hasCode(err, billing.ErrWebhookPayloadUnrecognized.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookPayloadUnrecognized for a refund resource missing out_trade_no", err)
	}
}

// TestGateway_VerifyWebhook_RefundSuccess_RedeliveryKeepsEventID pins the
// dedupe contract for redeliveries: WeChat Pay re-sends the same refund
// notification until it is acknowledged, so the decoded EventID must be a
// pure function of the payload -- two deliveries of one refund decode to
// the same EventID, and the insert-first-dedup ledger records the refund
// once.
func TestGateway_VerifyWebhook_RefundSuccess_RedeliveryKeepsEventID(t *testing.T) {
	_, platformPubPEM, platformPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, platformPubPEM)
	gw, err := newGatewayWithClient(&fakeDoer{}, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	refund := testRefund(t, "ORD1", "R1", "SUCCESS", 2900, 2900, "")

	firstHeaders, firstBody := signedRefundNotifyBody(t, platformPriv, cfg.APIv3Key, "REFUND.SUCCESS", refund)
	secondHeaders, secondBody := signedRefundNotifyBody(t, platformPriv, cfg.APIv3Key, "REFUND.SUCCESS", refund)

	first, err := gw.VerifyWebhook(context.Background(), firstHeaders, firstBody)
	if err != nil {
		t.Fatalf("VerifyWebhook (first delivery): %v", err)
	}
	second, err := gw.VerifyWebhook(context.Background(), secondHeaders, secondBody)
	if err != nil {
		t.Fatalf("VerifyWebhook (redelivery): %v", err)
	}
	if first.EventID != second.EventID {
		t.Errorf("redelivery EventID = %q, want the first delivery's %q (the dedup ledger must record one refund)", second.EventID, first.EventID)
	}
	if first.EventID == "ORD1:SUCCESS" {
		t.Errorf("EventID = %q, want it distinct from the payment's own (out_trade_no, trade_state) id", first.EventID)
	}
}
