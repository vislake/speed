//go:build integration

// Package stripe_test holds go/billing/gateway/stripe's integration tier: a
// Docker-backed leg driving the package's real HTTP dialogue with Stripe's
// API against stripe-mock, the official Stripe mock server
// (github.com/stripe/stripe-mock), plus a real-HTTP webhook-delivery-shape
// leg in webhook_delivery_test.go. It is physically separate from the
// package's unit tests (which live in package stripe itself, one file per
// source file, per the backend coding standard's testing layout rule) and
// carries the "integration" build tag: a plain "go test ./..." never
// compiles or runs anything in this directory; it is invoked explicitly
// with "go test -tags=integration ./..." (the form go/billing's own
// PostgreSQL integration tier already uses, and the form full-check.yml's
// integration-tiers matrix runs for the go/billing module row -- this
// directory sits under go/billing, so the existing billing matrix row picks
// it up with no workflow change).
//
// # Why this leg exists (the fail-before gap it closes)
//
// The package's unit tests exercise CreateCharge/QueryStatus through
// newGatewayWithBackend's scripted stripe.Backend double (gateway_test.go's
// fakeBackend). That double's Call signature receives the wire-encoded
// request body but DISCARDS it ("Call(method, path, _ string, ...)") -- it
// records method/path/idempotency-key/expand/metadata from the params
// object and answers with canned JSON it unmarshals itself, so nothing in
// the unit tier ever checks what stripe-go's own serializer actually puts
// on the wire against anything that implements Stripe's documented API
// contract, and no unit test ever constructs the real HTTP transport
// NewGateway uses (stripego.GetBackend(stripego.APIBackend)); every unit
// test reaches the gateway through the unexported
// newGatewayWithBackend constructor instead. This leg runs the REAL
// constructor path -- NewGateway over stripe-go's genuine HTTP backend,
// pointed at a server Stripe itself publishes that validates requests
// against the same OpenAPI-derived schema Stripe's real API enforces
// (verified live: a request carrying an unrecognized parameter is answered
// with Stripe's own 400 error envelope) -- so any drift in the method, the
// path, a parameter's name/type/nesting or the query encoding that would
// make a genuine Stripe call fail now fails here with the mock's error
// instead of passing the scripted double.
//
// stripe-mock needs no credential of any kind (any "valid looking testmode
// secret API key" is accepted -- the mock's own error message says so), and
// the repository's Docker-backed integration tiers are the sanctioned place
// for legs that need a container: there is deliberately no
// skip-on-missing-Docker path, mirroring go/billing's PostgreSQL tier and
// go/pkgcore's RustFS leg.
//
// The mock is stateless and fixture-driven, which bounds what this leg can
// honestly claim: the dialogue it proves is that this package's requests
// are accepted by a real Stripe-API-shaped server and its responses decode
// through stripe-go's own unmarshalers -- never that Stripe's live API
// would answer a particular status, that a session created here can be
// completed by a payer (no checkout flow exists against a mock), or that
// stripe-mock could sign a webhook delivery (it cannot -- verified by
// reading its source; see webhook_delivery_test.go's own doc comment for
// what that leg proves instead).
package stripe_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	stripego "github.com/stripe/stripe-go/v82"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/vislake/speed/go/billing"
	stripegw "github.com/vislake/speed/go/billing/gateway/stripe"
)

// stripeMockImage pins the stripe-mock image this leg runs against.
// stripe-mock is Stripe's official mock HTTP server
// (github.com/stripe/stripe-mock), published as the stripe/stripe-mock
// image on Docker Hub; its tags track upstream's GitHub releases, and
// v0.203.0 is the current upstream release as of this round (2026-09-08).
// The tag is pinned rather than "latest" so a fixture or validator change
// upstream cannot silently alter what this leg proves; a deliberate bump
// re-verifies the assertions below against the new release's fixtures.
const stripeMockImage = "stripe/stripe-mock:v0.203.0"

// startStripeMock starts one disposable stripe-mock container and returns
// its HTTP base URL, with the container terminated via t.Cleanup on test
// completion (pass or fail). stripe-mock serves HTTP on 12111 (and HTTPS on
// 12112); this leg only needs the HTTP port. The wait strategy polls the
// root path and treats any 2xx/401 answer as ready: stripe-mock answers
// every unauthenticated request with its "Please authenticate" 401 error
// envelope (never a bare TCP accept), so an HTTP response of either kind
// proves the router is genuinely listening -- verified directly against
// this image pin before relying on it here.
func startStripeMock(t *testing.T, ctx context.Context) string {
	t.Helper()

	req := testcontainers.ContainerRequest{
		Image:        stripeMockImage,
		ExposedPorts: []string{"12111/tcp"},
		WaitingFor: wait.ForHTTP("/").
			WithPort("12111/tcp").
			WithStatusCodeMatcher(func(status int) bool {
				return status == http.StatusOK || status == http.StatusUnauthorized
			}).
			WithStartupTimeout(60 * time.Second),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start stripe-mock testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate stripe-mock testcontainer: %v", terminateErr)
		}
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("stripe-mock testcontainer host: %v", err)
	}
	mappedPort, err := container.MappedPort(ctx, "12111/tcp")
	if err != nil {
		t.Fatalf("stripe-mock testcontainer mapped port: %v", err)
	}
	return fmt.Sprintf("http://%s:%s", host, mappedPort.Port())
}

// gatewayAgainstMock builds a real Gateway whose stripe-go HTTP backend is
// pointed at endpoint. This is the production construction path --
// stripe.NewGateway resolving stripego.GetBackend(stripego.APIBackend) --
// with stripego.SetBackend installing, in place of the default
// api.stripe.com-pointed backend, a genuine stripe-go HTTP backend whose
// base URL is the mock (stripe.BackendConfig.URL, the SDK's own documented
// seam for changing the API base). Nothing here is faked at the HTTP layer:
// stripe-go's own request serialization, transport and response decoding
// all run, exactly as they would against api.stripe.com.
func gatewayAgainstMock(t *testing.T, endpoint string) *stripegw.Gateway {
	t.Helper()

	backend := stripego.GetBackendWithConfig(stripego.APIBackend, &stripego.BackendConfig{
		HTTPClient:        &http.Client{Timeout: 30 * time.Second},
		MaxNetworkRetries: stripego.Int64(0),
		URL:               stripego.String(endpoint),
	})
	stripego.SetBackend(stripego.APIBackend, backend)

	gw, err := stripegw.NewGateway(stripegw.Config{
		// stripe-mock requires a "valid looking testmode secret API key"
		// (its own 401 message): the value is arbitrary and credential-free,
		// but its SHAPE is validated -- sk_test_ followed by alphanumerics
		// only, discovered live when the first draft of this leg used
		// "sk_test_mock_credential_free" and the mock refused it with 401.
		APIKey:        "sk_test_12345",
		WebhookSecret: testWebhookSecret,
		SuccessURL:    "https://example.test/success",
		CancelURL:     "https://example.test/cancel",
	})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	return gw
}

// testWebhookSecret mirrors the identical constant in the package's own
// unit tests (gateway_test.go); it is only used by the webhook-delivery leg
// in webhook_delivery_test.go, which signs with it and verifies with it.
const testWebhookSecret = "whsec_test_secret_0123456789"

// TestGateway_CreateChargeAndQueryStatus_RealStripeMockDialogue drives the
// two HTTP operations Gateway actually performs against a real
// stripe-mock server through stripe-go's genuine HTTP backend:
//
//   - CreateCharge posts the subscription-mode Checkout Session (inline
//     recurring price, session metadata, subscription_data.metadata,
//     idempotency key) to POST /v1/checkout/sessions;
//   - QueryStatus then retrieves the session the mock answered with via
//     GET /v1/checkout/sessions/{id}?expand[]=subscription.
//
// The assertions run on what the mock genuinely answers: CreateCharge must
// decode the mock's fixture (id + checkout URL) into a ChargeHandle, and
// QueryStatus must map the fixture's "open" session to
// ChannelStatusPending. On the pre-leg unit tier neither wire transaction
// ever happened: the scripted double discarded the encoded request body and
// every unit test reached the gateway through newGatewayWithBackend rather
// than NewGateway, so a serialization defect invisible to those tests (a
// wrong parameter name or nesting, a malformed query encoding, an
// unparseable response) fails this leg with the mock's own error.
func TestGateway_CreateChargeAndQueryStatus_RealStripeMockDialogue(t *testing.T) {
	ctx := context.Background()
	endpoint := startStripeMock(t, ctx)
	gw := gatewayAgainstMock(t, endpoint)

	// A unique idempotency key per run: the mock is stateless and accepts
	// anything, but the header must survive the real transport, and a key
	// that could collide across runs would make a rerun answer for a stale
	// session were the mock ever to become stateful.
	idempotencyKey := fmt.Sprintf("idem-mock-%d", time.Now().UnixNano())

	handle, err := gw.CreateCharge(ctx, billing.ChargeRequest{
		TenantID:       "tenant-mock",
		SubscriptionID: "sub-mock-1",
		InvoiceID:      "inv-mock-1",
		Amount:         billing.Money{Cents: 2900, Currency: "usd"},
		Description:    "stripe-mock integration leg",
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		t.Fatalf("CreateCharge over the real HTTP backend: %v (the mock answers a non-2xx with Stripe's own error envelope when the request violates its schema -- see the control test below for proof the mock validates)", err)
	}
	if handle.ChannelReference == "" || !strings.HasPrefix(string(handle.ChannelReference), "cs_") {
		t.Errorf("ChannelReference = %q, want a cs_-prefixed session id from the mock's fixture", handle.ChannelReference)
	}
	if handle.RedirectURL == "" || !strings.Contains(handle.RedirectURL, "checkout.stripe.com") {
		t.Errorf("RedirectURL = %q, want the mock fixture's checkout.stripe.com URL", handle.RedirectURL)
	}

	status, _, err := gw.QueryStatus(ctx, handle.ChannelReference)
	if err != nil {
		t.Fatalf("QueryStatus over the real HTTP backend for the session CreateCharge created: %v", err)
	}
	// The mock's checkout.session fixture carries status "open" -- the
	// pre-payment state -- so the status mapping this package applies to a
	// genuine server answer must land on Pending, the same answer a live
	// Stripe session awaiting its payer would give.
	if status != billing.ChannelStatusPending {
		t.Errorf("QueryStatus = %q, want %q (the mock fixture's open session maps to pending)", status, billing.ChannelStatusPending)
	}
}

// TestStripeMock_RejectsSchemaViolatingRequests is the control that keeps
// the leg honest: the assertions above only mean something if the mock is
// not a rubber stamp that answers 200 to anything. The same server that
// accepted CreateCharge's request above must refuse a request carrying a
// parameter Stripe's API schema does not define, with Stripe's own error
// envelope -- exactly what a live Stripe account would answer, and exactly
// the class of failure the pre-leg unit tier could never produce, since its
// double never validated the request body it was handed.
func TestStripeMock_RejectsSchemaViolatingRequests(t *testing.T) {
	ctx := context.Background()
	endpoint := startStripeMock(t, ctx)

	body := strings.NewReader("mode=subscription&success_url=https%3A%2F%2Fexample.test%2Fsuccess&this_is_not_a_stripe_parameter=1")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/checkout/sessions", body)
	if err != nil {
		t.Fatalf("build control request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer sk_test_123")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("control request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("stripe-mock answered the schema-violating request with %d, want 400 -- without this the CreateCharge acceptance above proves nothing (a rubber-stamp server would accept anything)", resp.StatusCode)
	}
}
