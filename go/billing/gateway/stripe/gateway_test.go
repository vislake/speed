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
	expand         []string
}

func (f *fakeBackend) Call(method, path, _ string, params stripego.ParamsContainer, v stripego.LastResponseSetter) error {
	var idem string
	var expand []string
	if params != nil {
		if params.GetParams().IdempotencyKey != nil {
			idem = *params.GetParams().IdempotencyKey
		}
		// CheckoutSessionParams declares its own top-level Expand field,
		// shadowing (for form-encoding purposes) the generic one promoted
		// from the embedded Params -- gateway.go's own QueryStatus doc
		// comment on why has the detail. Read the field that actually gets
		// serialized, via the concrete type, rather than the promoted
		// params.GetParams().Expand, which stays empty regardless.
		if sp, ok := params.(*stripego.CheckoutSessionParams); ok {
			for _, e := range sp.Expand {
				if e != nil {
					expand = append(expand, *e)
				}
			}
		}
	}
	f.calls = append(f.calls, fakeCall{method: method, path: path, idempotencyKey: idem, expand: expand})

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

// TestGateway_QueryStatus_RequestsSubscriptionExpand proves QueryStatus
// actually asks Stripe to expand "subscription" on the Get call --
// sessionStatus's Complete/Unpaid branch depends on Subscription.Status
// being populated, which stripego.Subscription's own custom UnmarshalJSON
// only does for an expanded reference (an unexpanded one arrives as an
// ID-only stub with every other field, Status included, left zero-valued).
// Losing this Expand silently defeats the whole fix below without failing
// any status-mapping assertion on its own, since a scripted test body can
// always hand-supply a populated Subscription object regardless of what was
// actually requested -- this test is what would catch that regression.
func TestGateway_QueryStatus_RequestsSubscriptionExpand(t *testing.T) {
	backend := &fakeBackend{
		respond: func(string, string) ([]byte, error) {
			return []byte(`{"id":"cs_1","status":"complete","payment_status":"paid","amount_total":1000,"currency":"usd"}`), nil
		},
	}
	gw := newGatewayWithBackend(backend, testConfig())

	if _, _, err := gw.QueryStatus(context.Background(), "cs_1"); err != nil {
		t.Fatalf("QueryStatus: %v", err)
	}
	if len(backend.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(backend.calls))
	}
	found := false
	for _, e := range backend.calls[0].expand {
		if e == "subscription" {
			found = true
		}
	}
	if !found {
		t.Errorf("expand = %v, want it to include %q", backend.calls[0].expand, "subscription")
	}
}

// TestGateway_QueryStatus_CompleteUnpaidTerminalSubscription_ReportsFailed is
// the blocker regression test: a Stripe Checkout Session that completed via
// redirect before its async payment method later, definitively failed can
// never revert its own Status/PaymentStatus to anything expressing failure
// (sessionStatus's own doc comment) -- so a bare Complete/Unpaid session,
// re-queried at any later time, answered ChannelStatusPending FOREVER on
// pre-fix code, permanently stranding the PaymentEvent row
// PollingService.Poll is supposed to eventually resolve. Once the
// underlying Subscription reaches its own terminal "incomplete_expired"
// state -- Stripe's real outcome once the first invoice's 23-hour
// collection window closes with no successful payment -- QueryStatus must
// now report ChannelStatusFailed instead, finally converging with what a
// checkout.session.async_payment_failed webhook already recorded for the
// identical session.
func TestGateway_QueryStatus_CompleteUnpaidTerminalSubscription_ReportsFailed(t *testing.T) {
	tests := []struct {
		name             string
		subscriptionJSON string
	}{
		{"incomplete_expired", `{"id":"sub_1","status":"incomplete_expired"}`},
		{"canceled", `{"id":"sub_1","status":"canceled"}`},
		{"unpaid", `{"id":"sub_1","status":"unpaid"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &fakeBackend{
				respond: func(string, string) ([]byte, error) {
					return []byte(`{"id":"cs_1","status":"complete","payment_status":"unpaid","amount_total":2900,"currency":"usd","subscription":` + tt.subscriptionJSON + `}`), nil
				},
			}
			gw := newGatewayWithBackend(backend, testConfig())

			status, _, err := gw.QueryStatus(context.Background(), "cs_1")
			if err != nil {
				t.Fatalf("QueryStatus: %v", err)
			}
			if status != billing.ChannelStatusFailed {
				t.Errorf("status = %q, want failed -- a terminal Subscription status must resolve the row, not strand it at pending forever", status)
			}
		})
	}
}

// TestGateway_QueryStatus_CompleteUnpaidRetryingSubscription_StillPending is
// the overzealous-fix guard: while the underlying Subscription is still
// "incomplete" (Stripe's smart payment retries still in play) or has no
// Subscription expanded at all, the session may yet succeed on a later
// attempt, so QueryStatus must keep answering ChannelStatusPending exactly
// as it did before this fix -- never jump straight to Failed just because
// the session is Complete/Unpaid.
func TestGateway_QueryStatus_CompleteUnpaidRetryingSubscription_StillPending(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"subscription_incomplete", `{"id":"cs_1","status":"complete","payment_status":"unpaid","amount_total":2900,"currency":"usd","subscription":{"id":"sub_1","status":"incomplete"}}`},
		{"no_subscription_expanded", `{"id":"cs_1","status":"complete","payment_status":"unpaid","amount_total":2900,"currency":"usd"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &fakeBackend{
				respond: func(string, string) ([]byte, error) {
					return []byte(tt.body), nil
				},
			}
			gw := newGatewayWithBackend(backend, testConfig())

			status, _, err := gw.QueryStatus(context.Background(), "cs_1")
			if err != nil {
				t.Fatalf("QueryStatus: %v", err)
			}
			if status != billing.ChannelStatusPending {
				t.Errorf("status = %q, want pending", status)
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

// TestGateway_VerifyWebhook_CompletedButUnpaid_AgreesWithQueryStatus is
// P1-1's regression test: under delayed settlement (or a manually-unsettled
// completed session), a checkout.session.completed webhook whose
// payment_status is "unpaid" must NOT be recorded as
// NormalizedEventChargeSucceeded/ChannelStatusSucceeded -- the exact
// disagreement that let the webhook path report "money arrived" while
// QueryStatus, re-querying the SAME session, reports it has not. This test
// fails on the pre-fix code (which reported ChannelStatusSucceeded
// unconditionally for checkout.session.completed) and asserts the fixed
// webhook path's Status agrees with what QueryStatus.sessionStatus would
// report for the identical session shape.
func TestGateway_VerifyWebhook_CompletedButUnpaid_AgreesWithQueryStatus(t *testing.T) {
	payload := checkoutSessionCompletedUnpaidPayload(t, "evt_unpaid_1", "cs_unpaid_1", "tenant-a", "sub-1", "inv-1", 2900, "usd")
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

	// The invariant P1-1 pins: whichever channel reports it, one session in
	// one state produces one module-side status. QueryStatus's own
	// sessionStatus (gateway.go) maps Status=Complete/PaymentStatus=Unpaid
	// to ChannelStatusPending -- the webhook path must land on the exact
	// same answer, never ChannelStatusSucceeded.
	wantStatus := sessionStatus(&stripego.CheckoutSession{
		Status:        stripego.CheckoutSessionStatusComplete,
		PaymentStatus: stripego.CheckoutSessionPaymentStatusUnpaid,
	})
	if wantStatus != billing.ChannelStatusPending {
		t.Fatalf("sanity check failed: sessionStatus(complete/unpaid) = %q, want pending", wantStatus)
	}
	if event.Status != wantStatus {
		t.Errorf("webhook Status = %q, want %q (QueryStatus's own answer for the identical session state)", event.Status, wantStatus)
	}
	if event.Status == billing.ChannelStatusSucceeded {
		t.Error("webhook recorded ChannelStatusSucceeded for a completed-but-unpaid session -- money has not arrived")
	}
	if event.Type == billing.NormalizedEventChargeSucceeded {
		t.Error("webhook recorded NormalizedEventChargeSucceeded for a completed-but-unpaid session")
	}
	if event.Amount.Cents != 0 {
		t.Errorf("Amount = %+v, want zero-valued for a not-yet-settled event", event.Amount)
	}
}

// TestGateway_VerifyWebhook_CompletedAndPaid_StillSucceeds is P1-1's second
// leg: an ordinary completed-and-paid session must keep reporting success
// exactly as before the fix -- the regression this leg guards against is
// P1-1's own fix becoming overzealous and treating every completed session
// as merely pending.
func TestGateway_VerifyWebhook_CompletedAndPaid_StillSucceeds(t *testing.T) {
	payload := checkoutSessionCompletedPayload(t, "evt_paid_1", "cs_paid_1", "tenant-a", "sub-1", "inv-1", 2900, "usd")
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
// checkout.session.completed event body with payment_status "paid" -- the
// shape event.go's normalizeEvent parses. checkoutSessionCompletedUnpaidPayload
// is the sibling fixture for the completed-but-unpaid case P1-1's own
// regression test drives.
func checkoutSessionCompletedPayload(t *testing.T, eventID, sessionID, tenantID, subID, invoiceID string, amountCents int64, currency string) []byte {
	t.Helper()
	return checkoutSessionPayload(t, eventID, sessionID, tenantID, subID, invoiceID, amountCents, currency, "paid")
}

// checkoutSessionCompletedUnpaidPayload builds a checkout.session.completed
// event body whose payment_status is "unpaid" -- Stripe's own documented
// possibility for a "complete" session using a deferred payment method, or
// a genuinely delayed settlement, and the exact shape
// TestGateway_VerifyWebhook_CompletedButUnpaid_AgreesWithQueryStatus drives.
func checkoutSessionCompletedUnpaidPayload(t *testing.T, eventID, sessionID, tenantID, subID, invoiceID string, amountCents int64, currency string) []byte {
	t.Helper()
	return checkoutSessionPayload(t, eventID, sessionID, tenantID, subID, invoiceID, amountCents, currency, "unpaid")
}

func checkoutSessionPayload(t *testing.T, eventID, sessionID, tenantID, subID, invoiceID string, amountCents int64, currency, paymentStatus string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"id":      eventID,
		"type":    "checkout.session.completed",
		"created": time.Now().Unix(),
		"data": map[string]any{
			"object": map[string]any{
				"id":             sessionID,
				"status":         "complete",
				"payment_status": paymentStatus,
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

// invoiceEventPayload builds a minimal, realistic invoice.paid or
// invoice.payment_failed event body -- the shape event.go's normalizeInvoice
// parses. Mirrors the metadata cascade normalizeInvoice's own doc comment
// describes: Stripe copies a subscription's Metadata onto every invoice it
// generates for that subscription, so this fixture's Invoice object carries
// the identical tenant/subscription/invoice keys checkoutSessionPayload's
// Session object carries.
func invoiceEventPayload(t *testing.T, eventType, eventID, invoiceID, tenantID, subID, origInvoiceID string, amountCents int64, currency string) []byte {
	t.Helper()
	var amountField string
	switch eventType {
	case "invoice.paid":
		amountField = "amount_paid"
	case "invoice.payment_failed":
		amountField = "amount_due"
	default:
		t.Fatalf("invoiceEventPayload: unsupported eventType %q", eventType)
	}
	body, err := json.Marshal(map[string]any{
		"id":      eventID,
		"type":    eventType,
		"created": time.Now().Unix(),
		"data": map[string]any{
			"object": map[string]any{
				"id":        invoiceID,
				amountField: amountCents,
				"currency":  currency,
				"metadata": map[string]string{
					metadataTenantID:       tenantID,
					metadataSubscriptionID: subID,
					metadataInvoiceID:      origInvoiceID,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return body
}

// subscriptionUpdatedPayload builds a customer.subscription.updated event
// body whose Subscription object's status is status -- the shape event.go's
// normalizeSubscriptionUpdated parses.
func subscriptionUpdatedPayload(t *testing.T, eventID, subscriptionID, tenantID, subID, invoiceID, status string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"id":      eventID,
		"type":    "customer.subscription.updated",
		"created": time.Now().Unix(),
		"data": map[string]any{
			"object": map[string]any{
				"id":     subscriptionID,
				"status": status,
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

// signAndVerify signs payload with the test webhook secret and drives it
// through a real Gateway.VerifyWebhook -- the identical real, offline
// signature-verification path every other webhook test in this file uses.
func signAndVerify(t *testing.T, payload []byte) (billing.NormalizedEvent, error) {
	t.Helper()
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload:   payload,
		Secret:    testWebhookSecret,
		Timestamp: time.Now(),
	})
	gw := newGatewayWithBackend(&fakeBackend{}, testConfig())
	return gw.VerifyWebhook(context.Background(), map[string][]string{
		"Stripe-Signature": {signed.Header},
	}, signed.Payload)
}

// TestGateway_VerifyWebhook_InvoicePaid_RecognizedAsChargeSucceeded is
// P1-2's regression test for the renewal-succeeded leg: invoice.paid is the
// event that actually announces a Stripe-native subscription's later
// billing cycles (gateway.go's CreateCharge is called exactly once, at
// first activation) -- on pre-fix code this event fell to normalizeEvent's
// default case and was refused as ErrWebhookPayloadUnrecognized, silently
// dropping a real renewal.
func TestGateway_VerifyWebhook_InvoicePaid_RecognizedAsChargeSucceeded(t *testing.T) {
	payload := invoiceEventPayload(t, "invoice.paid", "evt_inv_paid_1", "in_1", "tenant-a", "sub-1", "inv-1", 2900, "usd")
	event, err := signAndVerify(t, payload)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v (want it recognized, not %v)", err, billing.ErrWebhookPayloadUnrecognized.Code)
	}
	if event.Type != billing.NormalizedEventChargeSucceeded {
		t.Errorf("Type = %q, want charge_succeeded", event.Type)
	}
	if event.Status != billing.ChannelStatusSucceeded {
		t.Errorf("Status = %q, want succeeded", event.Status)
	}
	if event.TenantID != "tenant-a" || event.SubscriptionID != "sub-1" || event.InvoiceID != "inv-1" {
		t.Errorf("identifiers = %+v", event)
	}
	if event.Amount.Cents != 2900 || event.Amount.Currency != "usd" {
		t.Errorf("Amount = %+v", event.Amount)
	}
	if event.ChannelReference != "in_1" {
		t.Errorf("ChannelReference = %q, want in_1", event.ChannelReference)
	}
}

// TestGateway_VerifyWebhook_InvoicePaymentFailed_RecognizedAsChargeFailed is
// P1-2's regression test for the renewal-failed leg: invoice.payment_failed
// is what should turn a subscription past_due -- on pre-fix code this event
// was likewise refused as ErrWebhookPayloadUnrecognized, the exact "renewal
// payment failure is completely silent to the platform" gap the audit
// named.
func TestGateway_VerifyWebhook_InvoicePaymentFailed_RecognizedAsChargeFailed(t *testing.T) {
	payload := invoiceEventPayload(t, "invoice.payment_failed", "evt_inv_failed_1", "in_2", "tenant-a", "sub-1", "inv-1", 2900, "usd")
	event, err := signAndVerify(t, payload)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v (want it recognized)", err)
	}
	if event.Type != billing.NormalizedEventChargeFailed {
		t.Errorf("Type = %q, want charge_failed", event.Type)
	}
	if event.Status != billing.ChannelStatusFailed {
		t.Errorf("Status = %q, want failed", event.Status)
	}
	if event.Amount.Cents != 2900 || event.Amount.Currency != "usd" {
		t.Errorf("Amount = %+v", event.Amount)
	}
}

// TestGateway_VerifyWebhook_SubscriptionUpdatedCanceled_RecognizedAsSubscriptionCanceled
// is P1-2's regression test for the "even cancelled" leg the audit named:
// customer.subscription.updated with status "canceled" -- on pre-fix code
// refused as ErrWebhookPayloadUnrecognized -- must now recognize and map
// onto NormalizedEventSubscriptionCanceled/ChannelStatusCanceled.
func TestGateway_VerifyWebhook_SubscriptionUpdatedCanceled_RecognizedAsSubscriptionCanceled(t *testing.T) {
	payload := subscriptionUpdatedPayload(t, "evt_sub_canceled_1", "sub_stripe_1", "tenant-a", "sub-1", "inv-1", "canceled")
	event, err := signAndVerify(t, payload)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v (want it recognized)", err)
	}
	if event.Type != billing.NormalizedEventSubscriptionCanceled {
		t.Errorf("Type = %q, want subscription_canceled", event.Type)
	}
	if event.Status != billing.ChannelStatusCanceled {
		t.Errorf("Status = %q, want canceled", event.Status)
	}
	if event.TenantID != "tenant-a" || event.SubscriptionID != "sub-1" || event.InvoiceID != "inv-1" {
		t.Errorf("identifiers = %+v", event)
	}
}

// TestGateway_VerifyWebhook_SubscriptionUpdatedActive_StillUnrecognized
// keeps the pre-existing unrecognized-event discipline alive for a
// genuinely unrelated status this module's own vocabulary was never
// designed to represent as a second, amount-less signal (normalizeSubscriptionUpdated's
// own doc comment) -- customer.subscription.updated moving to "active" (or
// any status other than "canceled") stays ErrWebhookPayloadUnrecognized,
// exactly like TestGateway_VerifyWebhook_UnrecognizedEventType already pins
// for a wholly different event type.
func TestGateway_VerifyWebhook_SubscriptionUpdatedActive_StillUnrecognized(t *testing.T) {
	payload := subscriptionUpdatedPayload(t, "evt_sub_active_1", "sub_stripe_2", "tenant-a", "sub-1", "inv-1", "active")
	_, err := signAndVerify(t, payload)
	if !hasCode(err, billing.ErrWebhookPayloadUnrecognized.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookPayloadUnrecognized", err)
	}
}
