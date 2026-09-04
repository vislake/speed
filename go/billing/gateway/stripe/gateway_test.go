package stripe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	stripego "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/webhook"

	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

const testWebhookSecret = "whsec_test_secret_0123456789"

// fakeBackend is a scripted stripego.Backend -- the exact seam the SDK's
// own doc comment names as existing "to enable mocking for during testing
// if needed" (doc.go's own rationale). It never dials a real network
// connection; respond decides what every Call returns.
type fakeBackend struct {
	calls   []fakeCall
	respond func(method, path string) (body []byte, err error)
}

type fakeCall struct {
	method, path   string
	idempotencyKey string
}

func (f *fakeBackend) Call(method, path, _ string, params stripego.ParamsContainer, v stripego.LastResponseSetter) error {
	var idem string
	if params != nil && params.GetParams().IdempotencyKey != nil {
		idem = *params.GetParams().IdempotencyKey
	}
	f.calls = append(f.calls, fakeCall{method: method, path: path, idempotencyKey: idem})

	body, err := f.respond(method, path)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

func (f *fakeBackend) CallStreaming(string, string, string, stripego.ParamsContainer, stripego.StreamingLastResponseSetter) error {
	return errors.New("fakeBackend: CallStreaming not implemented")
}

func (f *fakeBackend) CallRaw(string, string, string, []byte, *stripego.Params, stripego.LastResponseSetter) error {
	return errors.New("fakeBackend: CallRaw not implemented")
}

func (f *fakeBackend) CallMultipart(string, string, string, string, *bytes.Buffer, *stripego.Params, stripego.LastResponseSetter) error {
	return errors.New("fakeBackend: CallMultipart not implemented")
}

func (f *fakeBackend) SetMaxNetworkRetries(int64) {}

func testConfig() Config {
	return Config{
		APIKey:        "sk_test_fake",
		WebhookSecret: testWebhookSecret,
		SuccessURL:    "https://example.test/success",
		CancelURL:     "https://example.test/cancel",
	}
}

func TestGateway_CreateCharge_SendsExpectedRequest(t *testing.T) {
	backend := &fakeBackend{
		respond: func(method, path string) ([]byte, error) {
			if method != "POST" || path != "/v1/checkout/sessions" {
				t.Fatalf("unexpected call: %s %s", method, path)
			}
			return []byte(`{"id":"cs_test_123","url":"https://checkout.stripe.com/pay/cs_test_123"}`), nil
		},
	}
	gw := newGatewayWithBackend(backend, testConfig())

	handle, err := gw.CreateCharge(context.Background(), billing.ChargeRequest{
		TenantID:       "tenant-a",
		SubscriptionID: "sub-1",
		InvoiceID:      "inv-1",
		Amount:         billing.Money{Cents: 2900, Currency: "usd"},
		Description:    "Pro plan",
		IdempotencyKey: "idem-1",
	})
	if err != nil {
		t.Fatalf("CreateCharge: %v", err)
	}
	if handle.ChannelReference != "cs_test_123" {
		t.Errorf("ChannelReference = %q, want cs_test_123", handle.ChannelReference)
	}
	if handle.RedirectURL != "https://checkout.stripe.com/pay/cs_test_123" {
		t.Errorf("RedirectURL = %q", handle.RedirectURL)
	}
	if len(backend.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(backend.calls))
	}
	if backend.calls[0].idempotencyKey != "idem-1" {
		t.Errorf("idempotencyKey = %q, want idem-1", backend.calls[0].idempotencyKey)
	}
}

func TestGateway_QueryStatus_MapsSessionStatus(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus billing.ChannelStatus
	}{
		{"open", `{"id":"cs_1","status":"open","payment_status":"unpaid","amount_total":1000,"currency":"usd"}`, billing.ChannelStatusPending},
		{"complete_paid", `{"id":"cs_1","status":"complete","payment_status":"paid","amount_total":1000,"currency":"usd"}`, billing.ChannelStatusSucceeded},
		{"expired", `{"id":"cs_1","status":"expired","payment_status":"unpaid","amount_total":1000,"currency":"usd"}`, billing.ChannelStatusFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &fakeBackend{
				respond: func(method, path string) ([]byte, error) {
					if method != "GET" || path != "/v1/checkout/sessions/cs_1" {
						t.Fatalf("unexpected call: %s %s", method, path)
					}
					return []byte(tt.body), nil
				},
			}
			gw := newGatewayWithBackend(backend, testConfig())

			status, amount, err := gw.QueryStatus(context.Background(), "cs_1")
			if err != nil {
				t.Fatalf("QueryStatus: %v", err)
			}
			if status != tt.wantStatus {
				t.Errorf("status = %q, want %q", status, tt.wantStatus)
			}
			if amount.Cents != 1000 || amount.Currency != "usd" {
				t.Errorf("amount = %+v", amount)
			}
		})
	}
}

func TestGateway_QueryStatus_NotFound(t *testing.T) {
	backend := &fakeBackend{
		respond: func(string, string) ([]byte, error) {
			return nil, &stripego.Error{Code: stripego.ErrorCodeResourceMissing, Type: stripego.ErrorTypeInvalidRequest, Msg: "No such checkout session"}
		},
	}
	gw := newGatewayWithBackend(backend, testConfig())

	_, _, err := gw.QueryStatus(context.Background(), "cs_missing")
	if !hasCode(err, billing.ErrChannelReferenceNotFound.Code) {
		t.Errorf("err = %v, want billing.ErrChannelReferenceNotFound", err)
	}
}

// TestGateway_VerifyWebhook_ValidSignature drives a REAL, offline signature
// verification: webhook.GenerateTestSignedPayload is Stripe's own SDK
// helper ("for mocking webhook events"), producing a genuine
// HMAC-SHA256-signed Stripe-Signature header over a real payload -- no
// network call, no fixture hand-computed outside the SDK's own signing
// code.
func TestGateway_VerifyWebhook_ValidSignature(t *testing.T) {
	payload := checkoutSessionCompletedPayload(t, "evt_test_1", "cs_test_1", "tenant-a", "sub-1", "inv-1", 2900, "usd")
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload:   payload,
		Secret:    testWebhookSecret,
		Timestamp: time.Now(),
	})

	gw := newGatewayWithBackend(&fakeBackend{}, testConfig())
	event, err := gw.VerifyWebhook(context.Background(), map[string][]string{
		"Stripe-Signature": {signed.Header},
	}, signed.Payload)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}

	if event.EventID != "evt_test_1" {
		t.Errorf("EventID = %q, want evt_test_1", event.EventID)
	}
	if event.Channel != "stripe" {
		t.Errorf("Channel = %q, want stripe", event.Channel)
	}
	if event.ChannelReference != "cs_test_1" {
		t.Errorf("ChannelReference = %q, want cs_test_1", event.ChannelReference)
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
	if event.Amount.Cents != 2900 || event.Amount.Currency != "usd" {
		t.Errorf("Amount = %+v", event.Amount)
	}
}

// TestGateway_VerifyWebhook_InvalidSignature proves a tampered body is
// refused -- the real attack this verification exists to stop: anyone who
// can reach the endpoint sending a forged event.
func TestGateway_VerifyWebhook_InvalidSignature(t *testing.T) {
	payload := checkoutSessionCompletedPayload(t, "evt_test_2", "cs_test_2", "tenant-a", "sub-1", "inv-1", 500, "usd")
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload:   payload,
		Secret:    testWebhookSecret,
		Timestamp: time.Now(),
	})
	tampered := append([]byte(nil), signed.Payload...)
	tampered = append(tampered, '!') // mutate the signed body after signing

	gw := newGatewayWithBackend(&fakeBackend{}, testConfig())
	_, err := gw.VerifyWebhook(context.Background(), map[string][]string{
		"Stripe-Signature": {signed.Header},
	}, tampered)
	if !hasCode(err, billing.ErrWebhookSignatureInvalid.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookSignatureInvalid", err)
	}
}

func TestGateway_VerifyWebhook_MissingSignatureHeader(t *testing.T) {
	gw := newGatewayWithBackend(&fakeBackend{}, testConfig())
	_, err := gw.VerifyWebhook(context.Background(), map[string][]string{}, []byte(`{}`))
	if !hasCode(err, billing.ErrWebhookSignatureInvalid.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookSignatureInvalid", err)
	}
}

func TestGateway_VerifyWebhook_UnrecognizedEventType(t *testing.T) {
	payload := []byte(`{"id":"evt_test_3","type":"customer.created","created":1700000000,"data":{"object":{"id":"cus_1"}}}`)
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload:   payload,
		Secret:    testWebhookSecret,
		Timestamp: time.Now(),
	})

	gw := newGatewayWithBackend(&fakeBackend{}, testConfig())
	_, err := gw.VerifyWebhook(context.Background(), map[string][]string{
		"Stripe-Signature": {signed.Header},
	}, signed.Payload)
	if !hasCode(err, billing.ErrWebhookPayloadUnrecognized.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookPayloadUnrecognized", err)
	}
}

func TestGateway_VerifyWebhook_MissingMetadata(t *testing.T) {
	payload := []byte(`{"id":"evt_test_4","type":"checkout.session.completed","created":1700000000,"data":{"object":{"id":"cs_1","amount_total":100,"currency":"usd","metadata":{}}}}`)
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload:   payload,
		Secret:    testWebhookSecret,
		Timestamp: time.Now(),
	})

	gw := newGatewayWithBackend(&fakeBackend{}, testConfig())
	_, err := gw.VerifyWebhook(context.Background(), map[string][]string{
		"Stripe-Signature": {signed.Header},
	}, signed.Payload)
	if !hasCode(err, billing.ErrWebhookPayloadUnrecognized.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookPayloadUnrecognized", err)
	}
}

func TestNewGateway_RequiresConfig(t *testing.T) {
	if _, err := NewGateway(Config{}); err == nil {
		t.Error("NewGateway(Config{}) = nil error, want an error")
	}
}

// checkoutSessionCompletedPayload builds a minimal, realistic
// checkout.session.completed event body -- the shape event.go's
// normalizeEvent parses.
func checkoutSessionCompletedPayload(t *testing.T, eventID, sessionID, tenantID, subID, invoiceID string, amountCents int64, currency string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"id":      eventID,
		"type":    "checkout.session.completed",
		"created": time.Now().Unix(),
		"data": map[string]any{
			"object": map[string]any{
				"id":             sessionID,
				"status":         "complete",
				"payment_status": "paid",
				"amount_total":   amountCents,
				"currency":       currency,
				"metadata": map[string]string{
					metadataTenantID:       tenantID,
					metadataSubscriptionID: subID,
					metadataInvoiceID:      invoiceID,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return body
}

// hasCode mirrors billing's own unexported hasCode helper (errors.go) --
// this package cannot import an unexported symbol, so it carries the
// identical, small comparison against apperr.As.
func hasCode(err error, code string) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == code
}
