package pkgcore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vislake/speed/go/pkgcore/safehttp"
)

// TestConsoleSMSSender_WritesToInjectedWriter proves the console transport
// writes to the writer it was given -- not to the process's real stdout,
// which is what makes it assertable at all.
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

// TestConsoleSMSSender_TemplateIdentityFields_PrintsOnlyToAndText proves the
// console transport is a free-text one: a message whose template-identity
// fields (MessageID, Locale, Params) are all set prints exactly the same
// one-line record it prints for a bare To/Text message, with no identity
// field and no parameter value in the output. The Params value here is a
// verification-code stand-in: a transport that echoed it would put the
// credential on whatever the writer is pointed at, which is exactly what
// the seam's Params doc forbids.
func TestConsoleSMSSender_TemplateIdentityFields_PrintsOnlyToAndText(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	sender := NewConsoleSMSSender(&buf)

	err := sender.Send(t.Context(), SMS{
		To:        "+8613800000000",
		Text:      "your code is 123456",
		MessageID: "authn.sms.verification_code",
		Locale:    "zh-CN",
		Params:    map[string]string{"code": "123456", "minutes": "5"},
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	const want = "SMS to +8613800000000: your code is 123456\n"
	if got := buf.String(); got != want {
		t.Errorf("Send() wrote %q, want exactly %q -- the template-identity fields must not reach the record", got, want)
	}
}

// TestConsoleSMSSender_CancelledContext_SendsNothing proves the console
// transport honours the seam contract's first rule -- a Send that begins on
// an already-cancelled context sends nothing and returns the context's
// error -- so every consumer's test double behaves the way the interface
// doc promises.
func TestConsoleSMSSender_CancelledContext_SendsNothing(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var buf bytes.Buffer
	sender := NewConsoleSMSSender(&buf)
	err := sender.Send(ctx, SMS{To: "+8613800000000", Text: "your code is 123456"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Send(cancelled context) error = %v, want context.Canceled", err)
	}
	if buf.Len() != 0 {
		t.Errorf("Send(cancelled context) wrote %q, want nothing written", buf.String())
	}
}

// TestHTTPSMSSender_PlaintextEndpoint_RefusedBeforeAnyRequest pins the
// transport's standing contract for its gateway endpoint: a sender built
// with an http:// endpoint must refuse it with safehttp's scheme error
// BEFORE any request is made. The payload is a phone number and a rendered
// message -- for a phone-login verification code, the credential of the
// sending module's phone channel -- so sending it over a plaintext gateway
// would put every code on the public internet in cleartext. The gateway
// here is a live plaintext server that RECORDS whether it was reached, so
// a refusal that happens after the request would be caught. The plain
// client is injected exactly as the delivery-path tests do, so the refusal
// under test is the endpoint's scheme, not the dialler (which a loopback
// gateway would trip first on the default client).
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
// address question is even reached -- an answer with the dialler's
// ErrBlockedAddress instead would be the refusal for the wrong axis and
// would say nothing about the cleartext problem.
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

// TestHTTPSMSSender_PostsExpectedJSON proves the HTTP transport posts
// exactly the {"to","text"} body a generic gateway expects, entirely
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

// TestHTTPSMSSender_TemplateIdentityFields_BodyStaysToAndText proves the
// gateway's body contract is unaffected by the template-identity fields: a
// message carrying all three of them still posts the byte-exact
// {"to","text"} body, with no extra keys and no parameter values -- the
// gateway is a free-text transport, and an operator's gateway contract must
// not silently widen when the seam grows a field.
func TestHTTPSMSSender_TemplateIdentityFields_BodyStaysToAndText(t *testing.T) {
	t.Parallel()

	var gotBody []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		gotBody = raw
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sender := NewHTTPSMSSender(server.URL, WithHTTPSMSSenderClient(server.Client()))
	err := sender.Send(t.Context(), SMS{
		To:        "+8613800000001",
		Text:      "your code is 654321",
		MessageID: "authn.sms.verification_code",
		Locale:    "zh-CN",
		Params:    map[string]string{"code": "654321", "minutes": "5"},
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	const want = `{"to":"+8613800000001","text":"your code is 654321"}`
	if got := string(gotBody); got != want {
		t.Errorf("gateway body = %s, want exactly %s -- the template-identity fields must not widen the body", got, want)
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
// private address -- the SSRF guard, mandatory for an operator-configurable
// outbound destination whose URL no code review of a caller can vouch for.
func TestHTTPSMSSender_PrivateEndpoint_Refused(t *testing.T) {
	t.Parallel()

	sender := NewHTTPSMSSender("https://127.0.0.1:1/sms")
	if err := sender.Send(t.Context(), SMS{To: "+8613800000003", Text: "x"}); err == nil {
		t.Fatalf("Send(private endpoint) error = nil, want the SSRF guard to refuse it")
	}
}
