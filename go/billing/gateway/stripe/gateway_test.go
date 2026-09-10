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
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

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
	// subscriptionMetadata is the metadata CreateCharge attached under the
	// session's subscription_data block -- the request parameter that
	// becomes the SUBSCRIPTION's own metadata when the session completes
	// (gateway.go's CreateCharge doc comment), and therefore what
	// subscription lifecycle events (customer.subscription.updated) and
	// their invoices' parent snapshots actually carry. Captured here so a
	// test can assert CreateCharge really sent it.
	subscriptionMetadata map[string]string
}

func (f *fakeBackend) Call(method, path, _ string, params stripego.ParamsContainer, v stripego.LastResponseSetter) error {
	var idem string
	var expand []string
	var subMetadata map[string]string
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
			if sp.SubscriptionData != nil {
				subMetadata = sp.SubscriptionData.Metadata
			}
		}
	}
	f.calls = append(f.calls, fakeCall{method: method, path: path, idempotencyKey: idem, expand: expand, subscriptionMetadata: subMetadata})

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
	// The correlation identifiers must ride in subscription_data's own
	// metadata, not only the session's. The session-level metadata is read
	// back by the checkout.session.* events this package recognizes, but it
	// is NOT copied onto the Subscription Stripe creates when the session
	// completes -- only subscription_data.metadata is applied to that
	// Subscription (the Create Session API's own subscription_data
	// description: "A subset of parameters to be passed to subscription
	// creation"). Without it, every later lifecycle object -- the
	// subscription itself, and each renewal invoice's parent snapshot --
	// carries no speed_* keys, and customer.subscription.updated /
	// invoice.paid / invoice.payment_failed deliveries are refused as
	// unrecognized.
	wantSubMeta := map[string]string{
		metadataTenantID:       "tenant-a",
		metadataSubscriptionID: "sub-1",
		metadataInvoiceID:      "inv-1",
	}
	got := backend.calls[0].subscriptionMetadata
	if len(got) != len(wantSubMeta) {
		t.Fatalf("subscription_data.metadata = %v, want %v", got, wantSubMeta)
	}
	for k, v := range wantSubMeta {
		if got[k] != v {
			t.Errorf("subscription_data.metadata[%q] = %q, want %q", k, got[k], v)
		}
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
// Losing this Expand silently defeats the terminal-subscription resolution
// the next test drives, without failing any status-mapping assertion on
// its own -- a scripted test body can always hand-supply a populated
// Subscription object regardless of what was actually requested; this test
// is what catches that regression.
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

// TestGateway_QueryStatus_CompleteUnpaidTerminalSubscription_ReportsFailed
// pins the terminal-subscription resolution: a Stripe Checkout Session that
// completed via redirect before its async payment method later, definitively
// failed can never revert its own Status/PaymentStatus to anything
// expressing failure (sessionStatus's own doc comment) -- so a bare
// Complete/Unpaid session, re-queried at any later time, must not answer
// ChannelStatusPending forever, permanently stranding the PaymentEvent row
// PollingService.Poll is supposed to eventually resolve. Once the
// underlying Subscription reaches its own terminal "incomplete_expired"
// state -- Stripe's real outcome once the first invoice's 23-hour
// collection window closes with no successful payment -- QueryStatus must
// report ChannelStatusFailed instead, converging with what a
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

// TestGateway_QueryStatus_CompleteUnpaidRetryingSubscription_StillPending
// is the boundary guard for the terminal-subscription resolution above:
// while the underlying Subscription is still "incomplete" (Stripe's smart
// payment retries still in play) or has no Subscription expanded at all,
// the session may yet succeed on a later attempt, so QueryStatus must keep
// answering ChannelStatusPending -- never jump straight to Failed just
// because the session is Complete/Unpaid.
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
	if !apperr.HasCode(err, billing.ErrChannelReferenceNotFound.Code) {
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

// TestGateway_VerifyWebhook_CompletedButUnpaid_AgreesWithQueryStatus pins
// the webhook/query agreement: under delayed settlement (or a
// manually-unsettled completed session), a checkout.session.completed
// webhook whose payment_status is "unpaid" must NOT be recorded as
// NormalizedEventChargeSucceeded/ChannelStatusSucceeded -- the
// disagreement that would let the webhook path report "money arrived"
// while QueryStatus, re-querying the SAME session, reports it has not. The
// webhook path's Status must agree with what QueryStatus.sessionStatus
// would report for the identical session shape.
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

	// The agreement invariant: whichever channel reports it, one session in
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

// TestGateway_VerifyWebhook_CompletedAndPaid_StillSucceeds pins the second
// half of the agreement: an ordinary completed-and-paid session must keep
// reporting success -- the completed-but-unpaid handling must not become
// overzealous and treat every completed session as merely pending.
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
	if !apperr.HasCode(err, billing.ErrWebhookSignatureInvalid.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookSignatureInvalid", err)
	}
}

func TestGateway_VerifyWebhook_MissingSignatureHeader(t *testing.T) {
	gw := newGatewayWithBackend(&fakeBackend{}, testConfig())
	_, err := gw.VerifyWebhook(context.Background(), map[string][]string{}, []byte(`{}`))
	if !apperr.HasCode(err, billing.ErrWebhookSignatureInvalid.Code) {
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
	if !apperr.HasCode(err, billing.ErrWebhookPayloadUnrecognized.Code) {
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
	if !apperr.HasCode(err, billing.ErrWebhookPayloadUnrecognized.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookPayloadUnrecognized", err)
	}
}

// TestGateway_VerifyWebhook_EventWithoutData_RefusedNotCrashed pins the
// nil-data guard: stripe-go's Event.Data is a POINTER (nil when a
// delivery's JSON carries no "data" object), and normalizeEvent's dispatch
// functions must not dereference it unconditionally -- a signed, recognized
// event type whose payload lacks data would otherwise crash the process
// with a nil-pointer panic instead of refusing the delivery. normalizeEvent
// guards up front: an event with no data object (or an empty one) is
// ErrWebhookPayloadUnrecognized like any other undecodable payload, never
// a crash.
func TestGateway_VerifyWebhook_EventWithoutData_RefusedNotCrashed(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"id":      "evt_no_data_1",
		"type":    "checkout.session.completed",
		"created": time.Now().Unix(),
		// deliberately NO "data" object
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload:   payload,
		Secret:    testWebhookSecret,
		Timestamp: time.Now(),
	})

	gw := newGatewayWithBackend(&fakeBackend{}, testConfig())
	_, err = gw.VerifyWebhook(context.Background(), map[string][]string{
		"Stripe-Signature": {signed.Header},
	}, signed.Payload)
	if !apperr.HasCode(err, billing.ErrWebhookPayloadUnrecognized.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookPayloadUnrecognized (a Data-less event must be refused cleanly, never crash)", err)
	}
}

func TestNewGateway_RequiresConfig(t *testing.T) {
	if _, err := NewGateway(Config{}); err == nil {
		t.Error("NewGateway(Config{}) = nil error, want an error")
	}
}

// checkoutSessionCompletedPayload builds a minimal, realistic
// checkout.session.completed event body with payment_status "paid" -- the
// shape event.go's normalizeEvent parses.
// checkoutSessionCompletedUnpaidPayload is the sibling fixture for the
// completed-but-unpaid case the webhook/query agreement test drives.
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

// invoiceEventPayload builds a minimal, realistic invoice.paid or
// invoice.payment_failed event body -- the shape event.go's normalizeInvoice
// parses. This fixture models the REAL Stripe delivery shape for an invoice
// a subscription generated (the only invoices this package's
// subscription-mode Checkout flow ever produces): the Invoice object's own
// top-level metadata carries NO speed_* keys -- Stripe does not copy a
// subscription's metadata onto the invoice's own metadata field -- and the
// correlation keys live under parent.subscription_details.metadata, the
// immutable snapshot of the subscription's metadata taken at the invoice's
// finalization (stripe-go v82.5.1's own field comment on
// InvoiceParentSubscriptionDetails.Metadata: "defined as subscription
// metadata when an invoice is created. Becomes an immutable snapshot of the
// subscription metadata at the time of invoice finalization"). A fixture
// that hand-seeds the keys onto the invoice's own metadata field models a
// shape real Stripe deliveries do not have -- which is exactly why code
// reading that field cannot see the identifiers on real events.
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
				// The invoice's own metadata is genuinely empty in the real
				// flow; see the doc comment above.
				"metadata": map[string]string{},
				"parent": map[string]any{
					"type": "subscription_details",
					"subscription_details": map[string]any{
						"subscription": "sub_stripe_1",
						"metadata": map[string]string{
							metadataTenantID:       tenantID,
							metadataSubscriptionID: subID,
							metadataInvoiceID:      origInvoiceID,
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return body
}

// invoiceEventPayloadWithInvoiceLevelMetadata builds an invoice.paid event
// whose identifiers sit ONLY on the Invoice object's own metadata field --
// the hand-seeded shape real Stripe subscription-invoice deliveries do not
// carry (see invoiceEventPayload's own doc comment). Used by
// TestGateway_VerifyWebhook_InvoicePaid_InvoiceLevelMetadataOnly_StillUnrecognized
// to pin that normalizeInvoice reads the parent snapshot, never the
// invoice's own metadata.
func invoiceEventPayloadWithInvoiceLevelMetadata(t *testing.T, eventID, invoiceID, tenantID, subID, origInvoiceID string, amountCents int64, currency string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"id":      eventID,
		"type":    "invoice.paid",
		"created": time.Now().Unix(),
		"data": map[string]any{
			"object": map[string]any{
				"id":          invoiceID,
				"amount_paid": amountCents,
				"currency":    currency,
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

// TestGateway_VerifyWebhook_InvoicePaid_RecognizedAsChargeSucceeded pins
// the renewal-succeeded leg: invoice.paid is the event that actually
// announces a Stripe-native subscription's later billing cycles (gateway.go's
// CreateCharge is called exactly once, at first activation). Its fixture
// carries the identifiers where real Stripe deliveries carry them --
// parent.subscription_details.metadata, the immutable snapshot of the
// subscription's metadata at finalization -- and never on the invoice's own
// metadata field. Reading inv.Metadata instead would make this fixture's
// identifiers invisible and refuse the event as
// ErrWebhookPayloadUnrecognized, silently dropping a real renewal.
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

// TestGateway_VerifyWebhook_InvoicePaymentFailed_RecognizedAsChargeFailed
// pins the renewal-failed leg: invoice.payment_failed is what should turn a
// subscription past_due. Reading the identifiers off the invoice's own
// (really empty) metadata field would refuse this event as
// ErrWebhookPayloadUnrecognized too -- the exact "renewal payment failure
// is completely silent to the platform" gap.
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

// TestGateway_VerifyWebhook_InvoicePaid_InvoiceLevelMetadataOnly_StillUnrecognized
// pins the correlation source: normalizeInvoice reads the identifiers from
// the invoice's parent snapshot (parent.subscription_details.metadata),
// never from the Invoice object's own metadata field. An invoice.paid
// delivery whose identifiers sit only on inv.Metadata -- the hand-seeded
// fixture shape real Stripe subscription-invoice deliveries do not produce
// -- stays ErrWebhookPayloadUnrecognized, exactly like any other event
// this package cannot correlate: reading inv.Metadata would recognize the
// wrong source, which is precisely why real deliveries (whose identifiers
// live in the parent snapshot instead) must not be expected there.
func TestGateway_VerifyWebhook_InvoicePaid_InvoiceLevelMetadataOnly_StillUnrecognized(t *testing.T) {
	payload := invoiceEventPayloadWithInvoiceLevelMetadata(t, "evt_inv_invmeta_1", "in_3", "tenant-a", "sub-1", "inv-1", 2900, "usd")
	_, err := signAndVerify(t, payload)
	if !apperr.HasCode(err, billing.ErrWebhookPayloadUnrecognized.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookPayloadUnrecognized (invoice-level metadata is not where real deliveries carry the identifiers)", err)
	}
}

// TestGateway_VerifyWebhook_SubscriptionUpdatedCanceled_RecognizedAsSubscriptionCanceled
// pins the "even cancelled" leg: customer.subscription.updated with status
// "canceled" must be recognized and mapped onto
// NormalizedEventSubscriptionCanceled/ChannelStatusCanceled -- never
// refused as ErrWebhookPayloadUnrecognized, which would drop the
// cancellation signal entirely.
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
// keeps the unrecognized-event discipline for a genuinely unrelated status
// this module's own vocabulary is not designed to represent as a second,
// amount-less signal (normalizeSubscriptionUpdated's own doc comment) --
// customer.subscription.updated moving to "active" (or any status other
// than "canceled") stays ErrWebhookPayloadUnrecognized, exactly like
// TestGateway_VerifyWebhook_UnrecognizedEventType already pins for a wholly
// different event type.
func TestGateway_VerifyWebhook_SubscriptionUpdatedActive_StillUnrecognized(t *testing.T) {
	payload := subscriptionUpdatedPayload(t, "evt_sub_active_1", "sub_stripe_2", "tenant-a", "sub-1", "inv-1", "active")
	_, err := signAndVerify(t, payload)
	if !apperr.HasCode(err, billing.ErrWebhookPayloadUnrecognized.Code) {
		t.Errorf("err = %v, want billing.ErrWebhookPayloadUnrecognized", err)
	}
}

// collectProviderMetric / providerCounterByAttr are this package's own
// copies of the billing-root test helpers: provider packages sit in
// their own test packages, so the helpers are duplicated per provider
// rather than exported from the root's test file.
func collectProviderMetric(t *testing.T, reader *sdkmetric.ManualReader, name string) metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	return metricdata.Metrics{}
}

func providerCounterByAttr(t *testing.T, m metricdata.Metrics, attrs map[string]string) int64 {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %q Data = %T, want metricdata.Sum[int64]", m.Name, m.Data)
	}
	for _, point := range sum.DataPoints {
		pointAttrs := map[string]string{}
		for _, kv := range point.Attributes.ToSlice() {
			pointAttrs[string(kv.Key)] = kv.Value.AsString()
		}
		match := true
		for key, value := range attrs {
			if pointAttrs[key] != value {
				match = false
				break
			}
		}
		if match {
			return point.Value
		}
	}
	return 0
}

// TestVerifyWebhook_OutcomeMetricRecorded drives the billing.webhook.verify
// counter through the real VerifyWebhook implementation: a genuinely
// signed delivery records succeeded, a refused one (missing signature
// header) records failed, both under the channel=stripe series. The
// metric registration happens in NewGateway, never in the test-only
// constructor -- this test therefore builds the gateway through the real
// constructor, whose webhook path needs no network.
func TestVerifyWebhook_OutcomeMetricRecorded(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))

	gw, err := NewGateway(testConfig())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	payload := checkoutSessionCompletedPayload(t, "evt_m1", "cs_m1", "tenant-a", "sub-1", "inv-1", 2900, "usd")
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload:   payload,
		Secret:    testWebhookSecret,
		Timestamp: time.Now(),
	})
	headers := map[string][]string{"Stripe-Signature": {signed.Header}}
	if _, err := gw.VerifyWebhook(context.Background(), headers, signed.Payload); err != nil {
		t.Fatalf("VerifyWebhook (genuine): %v", err)
	}
	if _, err := gw.VerifyWebhook(context.Background(), map[string][]string{}, payload); err == nil {
		t.Fatalf("VerifyWebhook (missing signature header) succeeded, want the refusal")
	}

	verify := collectProviderMetric(t, reader, billing.WebhookVerifyMetricName)
	if got := providerCounterByAttr(t, verify, map[string]string{"channel": "stripe", "outcome": "succeeded"}); got != 1 {
		t.Errorf("billing.webhook.verify{channel=stripe,outcome=succeeded} = %d, want 1", got)
	}
	if got := providerCounterByAttr(t, verify, map[string]string{"channel": "stripe", "outcome": "failed"}); got != 1 {
		t.Errorf("billing.webhook.verify{channel=stripe,outcome=failed} = %d, want 1", got)
	}
}
