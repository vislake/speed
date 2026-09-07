package authn

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vislake/speed/go/authn/internal/safehttp"
	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
)

// TestConsoleSMSSender_WritesToInjectedWriter proves the standalone
// transport writes to the writer it was given -- not to the process's real
// stdout, which is what makes it assertable at all.
func TestConsoleSMSSender_WritesToInjectedWriter(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	sender := NewConsoleSMSSender(&buf)

	if err := sender.Send(t.Context(), SMS{To: "+8613800000000", Text: "your code is 123456"}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "+8613800000000") || !strings.Contains(got, "123456") {
		t.Errorf("Send() wrote %q, want it to contain the phone number and the message text", got)
	}
}

// TestHTTPSMSSender_PlaintextEndpoint_RefusedBeforeAnyRequest is the
// regression for the P2 finding that the distributed-mode SMS gateway
// endpoint did not require TLS while its payload is a phone number and a
// rendered verification-code message -- the code being the credential of
// this module's phone-login channel. Before the fix a sender built with an
// http:// endpoint accepted it with no signal anywhere, so a deployment
// whose gateway URL pointed at a public plaintext gateway would send every
// code across the public internet in cleartext. The gateway here is a live
// plaintext server that RECORDS whether it was reached: the fixed sender
// must refuse the endpoint with safehttp's scheme error BEFORE any request
// is made -- on the unfixed code this send succeeds silently, which is the
// whole defect. The plain client is injected exactly as the delivery-path
// tests do, so the refusal under test is the endpoint's scheme, not the
// dialler (which a loopback gateway would trip first on the default
// client).
func TestHTTPSMSSender_PlaintextEndpoint_RefusedBeforeAnyRequest(t *testing.T) {
	t.Parallel()

	var hit atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sender := NewHTTPSMSSender(server.URL, WithHTTPSMSSenderClient(server.Client()))
	err := sender.Send(t.Context(), SMS{To: "+8613800000004", Text: "your code is 112233"})
	if err == nil {
		t.Fatalf("Send(plaintext endpoint) error = nil, want a refusal before any request")
	}
	if !errors.Is(err, safehttp.ErrBlockedScheme) {
		t.Errorf("Send(plaintext endpoint) error = %v, want it to wrap safehttp.ErrBlockedScheme", err)
	}
	if hit.Load() {
		t.Errorf("Send(plaintext endpoint) reached the gateway; the scheme refusal must happen before any request is made")
	}
}

// TestHTTPSMSSender_PlaintextEndpoint_RefusedBySchemeNotDialler pins the
// ORDER of the two duties on the default guarded client: an http:// endpoint
// whose host the dialler would also refuse must answer with the scheme error,
// because the plaintext endpoint is a confidentiality violation before any
// address question is even reached -- on the unfixed code this send answers
// with the dialler's ErrBlockedAddress instead, which is the refusal for the
// wrong axis and says nothing about the cleartext problem.
func TestHTTPSMSSender_PlaintextEndpoint_RefusedBySchemeNotDialler(t *testing.T) {
	t.Parallel()

	sender := NewHTTPSMSSender("http://127.0.0.1:1/sms")
	err := sender.Send(t.Context(), SMS{To: "+8613800000005", Text: "x"})
	if err == nil {
		t.Fatalf("Send(plaintext endpoint) error = nil, want a scheme refusal")
	}
	if !errors.Is(err, safehttp.ErrBlockedScheme) {
		t.Errorf("Send(plaintext endpoint) error = %v, want it to wrap safehttp.ErrBlockedScheme, not the dialler's address error", err)
	}
	if errors.Is(err, safehttp.ErrBlockedAddress) {
		t.Errorf("Send(plaintext endpoint) error = %v, want the scheme refusal to precede any dial-time address refusal", err)
	}
}

// TestHTTPSMSSender_PostsExpectedJSON proves the distributed transport
// posts exactly the {"to","text"} body a generic gateway expects, entirely
// offline against an httptest TLS server -- the TLS server is not optional
// decoration: the transport refuses a plaintext gateway endpoint before any
// request (see TestHTTPSMSSender_PlaintextEndpoint_RefusedBeforeAnyRequest),
// so the delivery path is exercised over the TLS test server's own client,
// which trusts its certificate.
func TestHTTPSMSSender_PostsExpectedJSON(t *testing.T) {
	t.Parallel()

	var received httpSMSGatewayRequest
	var gotContentType string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sender := NewHTTPSMSSender(server.URL, WithHTTPSMSSenderClient(server.Client()))
	if err := sender.Send(t.Context(), SMS{To: "+8613800000001", Text: "your code is 654321"}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	if received.To != "+8613800000001" || received.Text != "your code is 654321" {
		t.Errorf("gateway received %+v, want To/Text to match the sent message", received)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
}

// TestHTTPSMSSender_GatewayErrorStatus_ReturnsError proves a non-2xx
// gateway response surfaces as an error rather than a silent success.
func TestHTTPSMSSender_GatewayErrorStatus_ReturnsError(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	sender := NewHTTPSMSSender(server.URL, WithHTTPSMSSenderClient(server.Client()))
	if err := sender.Send(t.Context(), SMS{To: "+8613800000002", Text: "x"}); err == nil {
		t.Fatalf("Send() error = nil, want an error for a 503 gateway response")
	}
}

// TestHTTPSMSSender_PrivateEndpoint_Refused proves the default (no
// WithHTTPSMSSenderClient override) transport refuses to connect to a
// private address -- the SSRF guard authn's AGENTS.md documents as
// mandatory for every operator-configurable outbound destination.
func TestHTTPSMSSender_PrivateEndpoint_Refused(t *testing.T) {
	t.Parallel()

	sender := NewHTTPSMSSender("https://127.0.0.1:1/sms")
	if err := sender.Send(t.Context(), SMS{To: "+8613800000003", Text: "x"}); err == nil {
		t.Fatalf("Send(private endpoint) error = nil, want the SSRF guard to refuse it")
	}
}

// TestNewOptions_DistributedModeWithoutSMSSender_Fails proves a distributed
// bootstrap that never wired an SMSSender fails closed with the named
// sentinel, mirroring pkgcore.ErrMissingDistributedMailer.
func TestNewOptions_DistributedModeWithoutSMSSender_Fails(t *testing.T) {
	t.Parallel()

	keys := testutil.NewKeySource(t, "kid-active")
	_, err := newOptions([]Option{
		WithKeySource(keys),
		WithBlindIndexKey(testutil.BlindIndexKey()),
		WithDeploymentMode(pkgcore.DeploymentModeDistributed),
	})
	if !errors.Is(err, ErrMissingDistributedSMSSender) {
		t.Errorf("newOptions() error = %v, want it to wrap ErrMissingDistributedSMSSender", err)
	}
}

// TestNewOptions_DistributedModeWithSMSSender_Succeeds proves the same
// bootstrap succeeds once an SMSSender is wired.
func TestNewOptions_DistributedModeWithSMSSender_Succeeds(t *testing.T) {
	t.Parallel()

	keys := testutil.NewKeySource(t, "kid-active")
	cfg, err := newOptions([]Option{
		WithKeySource(keys),
		WithBlindIndexKey(testutil.BlindIndexKey()),
		WithDeploymentMode(pkgcore.DeploymentModeDistributed),
		WithSMSSender(NewConsoleSMSSender(&bytes.Buffer{})),
	})
	if err != nil {
		t.Fatalf("newOptions() error = %v, want nil once an SMSSender is wired", err)
	}
	if cfg.smsSender == nil {
		t.Fatalf("newOptions() smsSender = nil, want the wired sender")
	}
}

// TestNewOptions_StandaloneModeWithoutSMSSender_DefaultsToConsole proves
// the standalone deployment mode, and the zero-value (unset) deployment
// mode used by every option-validation test that predates this block, both
// get a working default sender rather than failing to construct.
func TestNewOptions_StandaloneModeWithoutSMSSender_DefaultsToConsole(t *testing.T) {
	t.Parallel()

	keys := testutil.NewKeySource(t, "kid-active")
	cfg, err := newOptions([]Option{
		WithKeySource(keys),
		WithBlindIndexKey(testutil.BlindIndexKey()),
	})
	if err != nil {
		t.Fatalf("newOptions() error = %v, want nil", err)
	}
	if cfg.smsSender == nil {
		t.Fatalf("newOptions() smsSender = nil, want a default console sender")
	}
}
