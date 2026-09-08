//go:build integration

// This file is part of go/billing/gateway/stripe's integration tier (see
// stripe_mock_leg_test.go's package doc comment for the tier's overall
// scope and its Docker convention). It holds the webhook half of the
// dialogue this package performs, and documents honestly what that half
// can and cannot prove.
//
// # What VerifyWebhook's dialogue actually is
//
// Reading the package's code: VerifyWebhook performs NO network call of its
// own -- it is a pure local HMAC-SHA256 verification
// (webhook.ConstructEventWithOptions) over a delivered body and its
// Stripe-Signature header, so unlike CreateCharge/QueryStatus there is no
// outbound HTTP dialogue a mock server could answer. What CAN be driven
// over a real wire is the delivery contract the verification consumes: in
// production the body and header arrive as an HTTP POST from Stripe, and
// the header map handed to VerifyWebhook is net/http's own canonicalized
// map (this package's firstHeader reads the exact key "Stripe-Signature"
// case-sensitively -- a map produced any other way silently misses it).
// The unit tier always constructs that map by hand; this leg delivers a
// genuinely signed payload over real HTTP (httptest server + real client)
// and verifies the whole receiving path -- canonical header casing, raw
// body bytes, firstHeader's lookup -- end to end.
//
// What this leg deliberately does not claim: it does not claim stripe-mock
// signed anything. stripe-mock has no webhook-signing capability (verified
// by reading its source: no Stripe-Signature/HMAC/whsec handling exists
// anywhere in the server), so the signature itself is produced by the
// stripe-go SDK's own sanctioned test helper
// (webhook.GenerateTestSignedPayload, the same real HMAC machinery the
// package's unit tests use). Capturing a delivery actually signed by
// Stripe's own servers still requires a live test-mode webhook endpoint
// and remains on the untestable-without-credentials boundary the gateway
// package's own docs record.
package stripe_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82/webhook"

	"github.com/vislake/speed/go/billing"
	stripegw "github.com/vislake/speed/go/billing/gateway/stripe"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// metadataTenantID/metadataSubscriptionID/metadataInvoiceID mirror the
// identical unexported constants in the stripe package's gateway.go -- the
// Checkout Session metadata keys CreateCharge attaches the correlation
// identifiers under and VerifyWebhook's normalizeEvent reads back out. The
// integration tier is a separate package and cannot import unexported
// constants, so the delivery fixture below spells the same keys again.
const (
	metadataTenantID       = "speed_tenant_id"
	metadataSubscriptionID = "speed_subscription_id"
	metadataInvoiceID      = "speed_invoice_id"
)

// TestGateway_VerifyWebhook_RealHTTPDeliveryRoundTrip drives a
// checkout.session.completed delivery through the exact shape a host
// receives it in: a real HTTP POST whose body bytes and Stripe-Signature
// header a handler passes, untouched, into Gateway.VerifyWebhook. The
// signed event must normalize into the expected billing.NormalizedEvent,
// and the identifiers must survive the round trip -- the property that
// makes a delivered webhook correlate back to the billing.Subscription
// CreateCharge opened.
func TestGateway_VerifyWebhook_RealHTTPDeliveryRoundTrip(t *testing.T) {
	gw, err := stripegw.NewGateway(stripegw.Config{
		APIKey:        "sk_test_mock_credential_free",
		WebhookSecret: testWebhookSecret,
		SuccessURL:    "https://example.test/success",
		CancelURL:     "https://example.test/cancel",
	})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	// The handler is the host's side of the contract: read the raw body,
	// hand the request's own header map to VerifyWebhook exactly as net/http
	// canonicalized it, and answer 400 on a refused delivery.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			http.Error(w, "read body", http.StatusInternalServerError)
			return
		}
		event, verifyErr := gw.VerifyWebhook(r.Context(), r.Header, body)
		if verifyErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(verifyErr.Error()))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(event)
	}))
	defer srv.Close()

	payload := checkoutSessionCompletedDelivery(t, "evt_http_1", "cs_http_1")
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload:   payload,
		Secret:    testWebhookSecret,
		Timestamp: time.Now(),
	})

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, bytes.NewReader(signed.Payload))
	if err != nil {
		t.Fatalf("build delivery request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", signed.Header)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("deliver webhook over real HTTP: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delivery answered %d, want 200 -- the real-header-map verification path refused a genuine delivery", resp.StatusCode)
	}

	var event billing.NormalizedEvent
	if err := json.NewDecoder(resp.Body).Decode(&event); err != nil {
		t.Fatalf("decode normalized event: %v", err)
	}
	if event.EventID != "evt_http_1" {
		t.Errorf("EventID = %q, want evt_http_1", event.EventID)
	}
	if event.ChannelReference != "cs_http_1" {
		t.Errorf("ChannelReference = %q, want cs_http_1", event.ChannelReference)
	}
	if event.TenantID != "tenant-a" || event.SubscriptionID != "sub-1" || event.InvoiceID != "inv-1" {
		t.Errorf("identifiers = %+v, want the CreateCharge-attached tenant-a/sub-1/inv-1", event)
	}
	if event.Type != billing.NormalizedEventChargeSucceeded {
		t.Errorf("Type = %q, want %q", event.Type, billing.NormalizedEventChargeSucceeded)
	}
}

// TestGateway_VerifyWebhook_RealHTTPDelivery_TamperedBodyRefused is the
// negative half of the same real-wire contract: a body mutated after
// signing must be refused by the receiving handler -- the attack signature
// verification exists to stop, exercised through the same real HTTP path as
// the accepted delivery above.
func TestGateway_VerifyWebhook_RealHTTPDelivery_TamperedBodyRefused(t *testing.T) {
	gw, err := stripegw.NewGateway(stripegw.Config{
		APIKey:        "sk_test_mock_credential_free",
		WebhookSecret: testWebhookSecret,
		SuccessURL:    "https://example.test/success",
		CancelURL:     "https://example.test/cancel",
	})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			http.Error(w, "read body", http.StatusInternalServerError)
			return
		}
		if _, verifyErr := gw.VerifyWebhook(r.Context(), r.Header, body); verifyErr != nil {
			if !hasCode(verifyErr, billing.ErrWebhookSignatureInvalid.Code) {
				t.Errorf("verifyErr = %v, want billing.ErrWebhookSignatureInvalid", verifyErr)
			}
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	payload := checkoutSessionCompletedDelivery(t, "evt_http_2", "cs_http_2")
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload:   payload,
		Secret:    testWebhookSecret,
		Timestamp: time.Now(),
	})
	tampered := append([]byte(nil), signed.Payload...)
	tampered = append(tampered, '!')

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, bytes.NewReader(tampered))
	if err != nil {
		t.Fatalf("build tampered delivery request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", signed.Header)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("deliver tampered webhook over real HTTP: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("tampered delivery answered %d, want 400 -- a body mutated after signing must be refused on the real wire", resp.StatusCode)
	}
}

// checkoutSessionCompletedDelivery builds the same minimal, realistic
// checkout.session.completed event body the package's own unit fixtures
// build -- the shape event.go's normalizeEvent parses, carrying the
// metadata identifiers CreateCharge attaches and a paid, completed session
// object.
func checkoutSessionCompletedDelivery(t *testing.T, eventID, sessionID string) []byte {
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
				"amount_total":   2900,
				"currency":       "usd",
				"metadata": map[string]string{
					metadataTenantID:       "tenant-a",
					metadataSubscriptionID: "sub-1",
					metadataInvoiceID:      "inv-1",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return body
}

// hasCode mirrors the identical helper the stripe package's own unit tests
// carry (gateway_test.go) -- this package cannot import an unexported
// symbol, so it spells the same small comparison against apperr.As.
func hasCode(err error, code string) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == code
}
