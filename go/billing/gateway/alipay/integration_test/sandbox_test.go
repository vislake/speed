//go:build integration

// Package alipay_test holds go/billing/gateway/alipay's integration tier: a
// single env-gated leg that drives the package's real HTTP dialogue with
// Alipay's open platform against Alipay's genuine sandbox environment. It
// is physically separate from the package's unit tests (which live in
// package alipay itself, one file per source file) and carries the
// "integration" build tag: a plain "go test ./..." never compiles or runs
// anything in this directory.
//
// # Why the leg is env-gated (and what it needs to run)
//
// Unlike the stripe tier (whose stripe-mock container needs no credential
// of any kind), Alipay's sandbox is a real Ant-hosted environment that
// authenticates every request against a real sandbox application, so this
// leg can only run in an environment that holds one operator's sandbox
// credentials. Without them the leg SKIPS ITSELF with an explicit recorded
// note, mirroring the reference-app e2e suite's own env-gated self-skip
// shape (examples/reference-app/web/e2e/platform-staff-can-administer.spec.ts:
// a test that needs an operator secret skips unless the environment
// variable naming that secret is set). The credentials come from Alipay's
// open-platform sandbox console (open.alipaydev.com -- the sandbox is
// documented at opendocs.alipay.com/support/01rbcw, "Using the sandbox
// environment for face-to-face payment integration", which confirms the
// FACE_TO_FACE_PAYMENT product this package's CreateCharge uses is
// sandbox-testable, needs no signing of its own, and is provisioned by
// the console rather than created by the developer). Required variables:
//
//	ALIPAY_SANDBOX_APP_ID              the sandbox application's appid
//	ALIPAY_SANDBOX_APP_PRIVATE_KEY     the app's own RSA2 private key, PEM
//	ALIPAY_SANDBOX_ALIPAY_PUBLIC_KEY   the sandbox's own Alipay RSA public
//	                                   key ("Alipay public key"), PEM --
//	                                   the counterpart every response-
//	                                   envelope signature is verified
//	                                   against
//
// ALIPAY_SANDBOX_NOTIFY_URL is optional (a placeholder is used when unset:
// the sandbox only POSTs notifications after a real payment, which this leg
// never performs, so the value is not exercised here).
//
// # What the leg proves, and the boundary it leaves
//
// The package's unit tier signs requests and verifies responses with a
// LOCALLY GENERATED key pair -- genuine RSA2, but a self-signed world:
// nothing in it proves the package's outgoing signature would be accepted
// by Alipay's real gateway, or that its response-envelope verification
// accepts a genuinely Alipay-signed envelope. This leg runs both halves of
// that dialogue against the real sandbox gateway
// (https://openapi-sandbox.dl.alipaydev.com/gateway.do, the sandbox
// endpoint the opendocs page above gives as the fixed sandbox value):
// CreateCharge must be accepted (a bad signature or a malformed request is
// answered by the sandbox with a signed business-error envelope, and the
// leg fails with the sandbox's own code text), and QueryStatus must verify
// a real Alipay-signed response envelope over a real order.
//
// What this leg deliberately does NOT do is complete a payment: paying a
// sandbox order requires Alipay's Android-only sandbox Alipay client (per
// the same opendocs page, sandbox testing supports balance payment only,
// and the sandbox client itself is Android-only), which no server-side test
// can drive. The order this leg
// creates therefore stays at WAIT_BUYER_PAY, which is itself the assertion:
// QueryStatus must answer ChannelStatusPending. The asynchronously-
// notified post-payment half (VerifyWebhook against a genuinely
// Alipay-delivered notification) accordingly stays on the
// untestable-without-a-driven-payment boundary the gateway package's own
// docs record.
package alipay_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/billing"
	alipaygw "github.com/vislake/speed/go/billing/gateway/alipay"
)

// alipaySandboxGatewayURL is the sandbox gateway's fixed endpoint, per the
// opendocs page cited in the package doc comment above, which fixes the
// sandbox gateway value at https://openapi-sandbox.dl.alipaydev.com/gateway.do; it is
// what Config.GatewayURL's own doc comment (config.go) calls "Alipay's
// sandbox endpoint".
const alipaySandboxGatewayURL = "https://openapi-sandbox.dl.alipaydev.com/gateway.do"

// alipaySandboxEnv names the environment variables this leg reads.
const (
	envSandboxAppID        = "ALIPAY_SANDBOX_APP_ID"
	envSandboxAppPrivKey   = "ALIPAY_SANDBOX_APP_PRIVATE_KEY"
	envSandboxAlipayPubKey = "ALIPAY_SANDBOX_ALIPAY_PUBLIC_KEY"
	envSandboxNotifyURL    = "ALIPAY_SANDBOX_NOTIFY_URL"
)

// sandboxConfigFromEnv assembles the alipay.Config this leg drives,
// skipping the test with an explicit recorded note when the sandbox
// credentials are not present -- the reference-app e2e suite's self-skip
// shape: an operator-secret-gated test must say, in its own skip message,
// exactly which secret is missing and why the test cannot run without it.
func sandboxConfigFromEnv(t *testing.T) alipaygw.Config {
	t.Helper()

	cfg := alipaygw.Config{
		AppID:              os.Getenv(envSandboxAppID),
		PrivateKeyPEM:      []byte(os.Getenv(envSandboxAppPrivKey)),
		AlipayPublicKeyPEM: []byte(os.Getenv(envSandboxAlipayPubKey)),
		NotifyURL:          os.Getenv(envSandboxNotifyURL),
		GatewayURL:         alipaySandboxGatewayURL,
	}
	if cfg.NotifyURL == "" {
		// The sandbox only POSTs an async notification after the order is
		// genuinely paid, which no server-side leg can drive (the sandbox
		// payment client is Android-only -- see the package doc comment
		// above), so a placeholder that is never exercised is honest here.
		cfg.NotifyURL = "https://example.test/alipay/notify"
	}

	var missing []string
	if cfg.AppID == "" {
		missing = append(missing, envSandboxAppID)
	}
	if len(cfg.PrivateKeyPEM) == 0 {
		missing = append(missing, envSandboxAppPrivKey)
	}
	if len(cfg.AlipayPublicKeyPEM) == 0 {
		missing = append(missing, envSandboxAlipayPubKey)
	}
	if len(missing) > 0 {
		t.Skipf(
			"self-skip: the Alipay sandbox leg needs the operator's own sandbox credentials, which %s is/are not set (Alipay's sandbox environment, unlike stripe-mock, is a real Ant-hosted gateway that authenticates every request against a real sandbox application; get the values from the open.alipaydev.com sandbox console per opendocs.alipay.com/support/01rbcw). Set them and re-run: go test -tags=integration ./gateway/alipay/integration_test/",
			strings.Join(missing, ", "),
		)
	}
	return cfg
}

// TestGateway_Sandbox_PrecreateAndQuery drives CreateCharge and QueryStatus
// through the package's real HTTP client against Alipay's genuine sandbox
// gateway. The unit tier's scripted httpDoer double asserts the signed
// request shape and returns a self-signed canned envelope, so (a) whether
// Alipay's own gateway accepts this package's RSA2-signed request and (b)
// whether this package's response-envelope verification accepts a
// genuinely Alipay-signed envelope stay unproven there -- precisely the
// two properties this sandbox leg exists to prove. Both directions fail
// loudly here on any signature or request-shape defect, with the sandbox's
// own error text.
func TestGateway_Sandbox_PrecreateAndQuery(t *testing.T) {
	cfg := sandboxConfigFromEnv(t)

	gw, err := alipaygw.NewGateway(cfg)
	if err != nil {
		t.Fatalf("NewGateway: %v (a key parse failure here means the PEM values in %s/%s do not match what NewGateway expects)", err, envSandboxAppPrivKey, envSandboxAlipayPubKey)
	}

	// A fresh out_trade_no per run: Alipay's own idempotency is keyed on the
	// merchant order number, and an order number reused across runs would
	// collide with the previous run's still-unpaid sandbox order. Letters
	// and digits only -- Alipay's documented out_trade_no charset.
	idem := fmt.Sprintf("speedsbtest%d", time.Now().UnixNano())

	handle, err := gw.CreateCharge(context.Background(), billing.ChargeRequest{
		TenantID:       "tenant-sandbox",
		SubscriptionID: "sub-sandbox-1",
		InvoiceID:      "inv-sandbox-1",
		Amount:         billing.Money{Cents: 100, Currency: "CNY"},
		Description:    "alipay sandbox integration leg",
		IdempotencyKey: idem,
	})
	if err != nil {
		t.Fatalf("CreateCharge against the Alipay sandbox gateway: %v (if this is a business error like isv.*-permissions/application-signature, the sandbox application's face-to-face-payment setup is incomplete -- the sandbox console must hold the uploaded application public key matching the private key in %s, and the %s value must be the sandbox's own Alipay public key)", err, envSandboxAppPrivKey, envSandboxAlipayPubKey)
	}
	if string(handle.ChannelReference) != idem {
		t.Errorf("ChannelReference = %q, want the out_trade_no %q the order was created under", handle.ChannelReference, idem)
	}
	if handle.QRCodeContent == "" || !strings.HasPrefix(handle.QRCodeContent, "https://") {
		t.Errorf("QRCodeContent = %q, want the sandbox's real payment QR URL for the created order", handle.QRCodeContent)
	}

	status, amount, err := gw.QueryStatus(context.Background(), handle.ChannelReference)
	if err != nil {
		t.Fatalf("QueryStatus against the Alipay sandbox gateway: %v", err)
	}
	// The order this leg just created has not been paid (paying a sandbox
	// order needs Alipay's Android-only sandbox client, which no server-side
	// leg can drive), so the sandbox's genuine answer must be
	// WAIT_BUYER_PAY, mapped to Pending -- with the amount the order was
	// created for read back over a genuinely Alipay-signed envelope.
	if status != billing.ChannelStatusPending {
		t.Errorf("status = %q, want %q (an unpaid sandbox order answers WAIT_BUYER_PAY)", status, billing.ChannelStatusPending)
	}
	if amount.Cents != 100 || amount.Currency != "CNY" {
		t.Errorf("amount = %+v, want the 100 CNY cents the order was created for", amount)
	}
}
