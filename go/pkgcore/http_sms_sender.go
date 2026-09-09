package pkgcore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/vislake/speed/go/pkgcore/safehttp"
)

// maxSMSGatewayResponseBytes bounds how much of a gateway's response body
// this sender reads, so a misbehaving or hostile gateway cannot hold a
// request goroutine reading an unbounded body.
const maxSMSGatewayResponseBytes = 64 * 1024

// httpSMSSenderConfig is an HTTP SMS sender's settings.
type httpSMSSenderConfig struct {
	httpClient *http.Client
}

// HTTPSMSSenderOption configures NewHTTPSMSSender.
type HTTPSMSSenderOption func(*httpSMSSenderConfig)

// WithHTTPSMSSenderClient replaces the HTTP client an HTTP SMS sender talks
// to its gateway with.
//
// The default is the SSRF-guarded client (pkgcore/safehttp), which cannot
// connect to a private address -- so a test pointing a sender at an httptest
// server on loopback MUST inject a plain client (the TLS test server's own),
// and does; a deployment has no reason to. Replacing the client replaces
// only that dial-time address guard: the endpoint's https requirement (see
// NewHTTPSMSSender) is enforced by the sender itself, before any request,
// and survives any client swap.
func WithHTTPSMSSenderClient(client *http.Client) HTTPSMSSenderOption {
	return func(c *httpSMSSenderConfig) {
		if client != nil {
			c.httpClient = client
		}
	}
}

// httpSMSSender is a real SMS transport for a deployment that has no vendor
// SDK in it: a generic JSON gateway POST. It is deliberately generic rather
// than a specific carrier's SDK -- it serves an operator-run gateway (the
// reference app's APP_SMS_GATEWAY_URL is its configuration home), while the
// real Aliyun, Tencent Cloud and Twilio carrier adapters live under
// pkgcore/sms/ (each implementing the vendor's own signing and request shape
// against its official API). It is nonetheless a genuine, testable
// implementation and not a placeholder: it is offline-testable end to end
// against httptest.NewTLSServer, and its endpoint is subject to the same
// pkgcore/safehttp policy a tenant administrator's OIDC issuer URL is,
// because both are a destination an operator, not this codebase, chose --
// the endpoint's scheme must be https (checked before every send, see Send)
// and every connection is dialled through safehttp's guarded client, which
// refuses a private address at CONNECT time.
type httpSMSSender struct {
	endpoint string
	client   *http.Client
	guard    *safehttp.Guard
}

// NewHTTPSMSSender returns the HTTP SMS transport, posting a JSON
// {"to","text"} body to endpoint.
//
// The body is a phone number and a rendered message -- for a phone-login
// verification code, the code is the credential of the sending module's
// phone channel -- so endpoint MUST be https, and Send refuses one that is
// not before any request leaves this process. That is safehttp's
// allowed-scheme policy, the same policy a tenant administrator's OIDC
// issuer URL is checked against when it is saved. This constructor returns
// no error; the refusal surfaces on the first Send instead, and through the
// delivery-failure log the calling module already writes for a failed Send
// -- an operator whose gateway URL is refused should change the scheme of
// the value they configured, never replace this constructor.
func NewHTTPSMSSender(endpoint string, opts ...HTTPSMSSenderOption) SMSSender {
	guard := safehttp.NewGuard()
	cfg := httpSMSSenderConfig{httpClient: guard.Client()}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return &httpSMSSender{endpoint: endpoint, client: cfg.httpClient, guard: guard}
}

// httpSMSGatewayRequest is the body posted to the gateway.
type httpSMSGatewayRequest struct {
	To   string `json:"to"`
	Text string `json:"text"`
}

// Send implements SMSSender.
//
// Two safehttp duties protect the request, and they are not the same duty.
// The endpoint's scheme is checked FIRST, against safehttp's allowed-scheme
// policy (https only -- NewGuard's default, the same policy the enterprise
// OIDC issuer URL is checked against when a tenant saves it). That is the
// confidentiality half, and it has no dial-time twin: the guarded client's
// dialler checks ADDRESSES, never schemes, so the scheme check must happen
// here, on the path that actually sends. The address half is the dialler's:
// every connection -- every redirect hop included -- goes through safehttp's
// guarded client (the default, unless a test overrode it with
// WithHTTPSMSSenderClient), which refuses a private, loopback, link-local or
// otherwise non-public address at CONNECT time and defeats DNS rebinding.
// Both halves exist for one reason: an operator, not this codebase, chose
// the destination, exactly as with the enterprise SSO issuer URL.
func (s *httpSMSSender) Send(ctx context.Context, sms SMS) error {
	// The scheme refusal must precede everything else: a plaintext endpoint
	// would carry this message's text in cleartext, so no request may ever
	// be built, let alone sent, to one.
	if _, err := s.guard.CheckScheme(s.endpoint); err != nil {
		return fmt.Errorf("pkgcore: sms gateway endpoint: %w", err)
	}

	body, err := json.Marshal(httpSMSGatewayRequest(sms))
	if err != nil {
		return fmt.Errorf("pkgcore: encode sms gateway request: %w", err)
	}

	// #nosec G704 -- gosec's taint analysis flags any http.NewRequestWithContext
	// call whose URL is a variable, but s.endpoint's scheme was checked at the
	// top of this function and the request is dialled through safehttp's
	// guarded client by default (this function's own doc comment above), which
	// resolves and dials the exact validated IP, rejecting
	// private/loopback/link-local/CGNAT ranges and defeating DNS rebinding.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("pkgcore: build sms gateway request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// #nosec G704 -- the same false positive as this function's earlier
	// #nosec comment, now at the point gosec's taint analysis actually
	// flags the request's execution rather than its construction.
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("pkgcore: send sms: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain (bounded) rather than discard: an unread body on a keep-alive
	// connection prevents the transport from reusing it.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxSMSGatewayResponseBytes))

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("pkgcore: sms gateway returned status %d", resp.StatusCode)
	}
	return nil
}

// compile-time check that the transport satisfies SMSSender.
var _ SMSSender = (*httpSMSSender)(nil)
