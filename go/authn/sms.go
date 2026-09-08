package authn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/vislake/speed/go/authn/internal/safehttp"
)

// ErrMissingDistributedSMSSender is returned by NewModule and NewService
// when the module is being wired with WithDeploymentMode(pkgcore.
// DeploymentModeDistributed) and no SMSSender was supplied with
// WithSMSSender.
//
// It exists for exactly the reason pkgcore.ErrMissingDistributedMailer
// does: NewConsoleSMSSender prints to a writer nobody in a distributed
// deployment's replica pool is reading, so silently falling back to it
// there would look like phone sign-in works right up until the first
// person tries to use the code that was never actually delivered. The SMS
// seam lives in THIS module rather than in pkgcore -- SMS delivery is not
// a primitive every module needs -- so unlike the mailer and object-store
// cases this validation cannot happen inside pkgcore.Kernel --
// WithDeploymentMode records the one fact this module needs from the host
// to enforce it itself, at the same wiring-time moment NewModule already
// validates WithKeySource and WithBlindIndexKey.
var ErrMissingDistributedSMSSender = errors.New("authn: distributed deployment mode requires an explicit SMS sender")

// SMS is one message to deliver to a phone number.
type SMS struct {
	// To is the destination phone number, in whatever form the caller
	// building it used -- SMSSender implementations pass it through to
	// their transport unchanged rather than normalizing it.
	To string
	// Text is the message body, already rendered in the recipient's
	// locale. SMSSender never sees the code or the locale separately; by
	// the time an SMS reaches this seam it is content, not data.
	Text string
}

// SMSSender delivers a one-time verification code, or any other short
// message, to a phone number.
//
// It is authn's own seam, not a pkgcore one: the dependency floor carries
// only capabilities every module needs, and SMS delivery is specific to
// this module's phone sign-in flow. It ships two implementations in this
// package -- NewConsoleSMSSender (standalone, zero external dependency) and
// NewHTTPSMSSender (distributed, a real, independently testable second
// implementation) -- selected by which constructor the HOST calls, never by
// a mode branch inside this package, plus three real carrier adapters under
// go/authn/sms/ (aliyun, tencent, twilio), each implementing its vendor's
// own signing and request shape, constructed by the host through the
// package's own NewSender.
type SMSSender interface {
	// Send delivers msg. An error means the message was not delivered;
	// the caller (verification.go) surfaces that as a structured failure
	// rather than pretending the code went out.
	Send(ctx context.Context, msg SMS) error
}

// consoleSMSSender is the standalone deployment mode's SMS transport: it
// writes every message to an injected io.Writer instead of sending
// anything -- the console-form degradation for SMS ("printed to stdout").
type consoleSMSSender struct {
	w io.Writer
}

// NewConsoleSMSSender returns the standalone deployment mode's SMS
// transport, writing every message to w. w is required and never defaults
// to os.Stdout implicitly: an implicit global write target is exactly what
// this module's own coding standard forbids for an HTTP client (@speed/
// api-client's rule against an implicit environment fetch has the same
// shape), and requiring it explicitly is also what lets a test assert on
// the written content instead of capturing the process's real stdout.
func NewConsoleSMSSender(w io.Writer) SMSSender {
	return &consoleSMSSender{w: w}
}

// Send implements SMSSender.
func (s *consoleSMSSender) Send(_ context.Context, msg SMS) error {
	_, err := fmt.Fprintf(s.w, "SMS to %s: %s\n", msg.To, msg.Text)
	if err != nil {
		return fmt.Errorf("authn: write console sms: %w", err)
	}
	return nil
}

// maxSMSGatewayResponseBytes bounds how much of a gateway's response body
// this module reads, so a misbehaving or hostile gateway cannot hold a
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
// The default is the SSRF-guarded client (this module's internal/safehttp),
// which cannot connect to a private address -- so a test pointing a sender
// at an httptest server on loopback MUST inject a plain client (the TLS
// test server's own), and does; a deployment has no reason to. Replacing
// the client replaces only that dial-time address guard: the endpoint's
// https requirement (see NewHTTPSMSSender) is enforced by the sender
// itself, before any request, and survives any client swap.
func WithHTTPSMSSenderClient(client *http.Client) HTTPSMSSenderOption {
	return func(c *httpSMSSenderConfig) {
		if client != nil {
			c.httpClient = client
		}
	}
}

// httpSMSSender is the distributed deployment mode's real SMS transport: a
// generic JSON gateway POST. It is deliberately generic rather than a
// specific carrier's SDK -- it serves an operator-run gateway (this module's
// own AGENTS.md has the reasoning), while the real Aliyun, Tencent Cloud and
// Twilio carrier adapters live under go/authn/sms/ (each implementing the
// vendor's own signing and request shape against its official API). It is
// nonetheless a genuine, testable second implementation and not a
// placeholder: it is offline-testable end to end against
// httptest.NewTLSServer, and its endpoint is subject to the same
// internal/safehttp policy the enterprise OIDC issuer URL is, because both
// are a destination an operator, not this codebase, chose -- the endpoint's
// scheme must be https (checked before every send, see Send) and every
// connection is dialled through safehttp's guarded client, which refuses a
// private address at CONNECT time.
type httpSMSSender struct {
	endpoint string
	client   *http.Client
	guard    *safehttp.Guard
}

// NewHTTPSMSSender returns the distributed deployment mode's SMS transport,
// posting a JSON {"to","text"} body to endpoint.
//
// The body is a phone number and a rendered verification-code message --
// the code is the credential of this module's phone-login channel -- so
// endpoint MUST be https, and Send refuses one that is not before any
// request leaves this process. That is internal/safehttp's allowed-scheme
// policy, the same policy a tenant administrator's OIDC issuer URL is
// checked against when it is saved. This constructor returns no error; the
// refusal surfaces on the first Send instead, and through the
// delivery-failure log the service already writes for a failed Send -- an
// operator whose gateway URL is refused should change the scheme of the
// value they configured (the reference app's APP_SMS_GATEWAY_URL), never
// replace this constructor.
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
// Two internal/safehttp duties protect the request, and they are not the
// same duty. The endpoint's scheme is checked FIRST, against safehttp's
// allowed-scheme policy (https only -- NewGuard's default, the same policy
// the enterprise OIDC issuer URL is checked against when a tenant saves
// it). That is the confidentiality half, and it has no dial-time twin: the
// guarded client's dialler checks ADDRESSES, never schemes, so the scheme
// check must happen here, on the path that actually sends. The address half
// is the dialler's: every connection -- every redirect hop included -- goes
// through internal/safehttp's guarded client (the default, unless a test
// overrode it with WithHTTPSMSSenderClient), which refuses a private,
// loopback, link-local or otherwise non-public address at CONNECT time and
// defeats DNS rebinding. Both halves exist for one reason: an operator, not
// this codebase, chose the destination, exactly as with the enterprise SSO
// issuer URL.
func (s *httpSMSSender) Send(ctx context.Context, msg SMS) error {
	// The scheme refusal must precede everything else: a plaintext endpoint
	// would carry this message's verification code in cleartext, so no
	// request may ever be built, let alone sent, to one.
	if _, err := s.guard.CheckScheme(s.endpoint); err != nil {
		return fmt.Errorf("authn: sms gateway endpoint: %w", err)
	}

	body, err := json.Marshal(httpSMSGatewayRequest(msg))
	if err != nil {
		return fmt.Errorf("authn: encode sms gateway request: %w", err)
	}

	// #nosec G704 -- gosec's taint analysis flags any http.NewRequestWithContext
	// call whose URL is a variable, but s.endpoint's scheme was checked at the
	// top of this function and the request is dialled through internal/
	// safehttp's guarded client by default (this function's own doc comment
	// above), which resolves and dials the exact validated IP, rejecting
	// private/loopback/link-local/CGNAT ranges and defeating DNS rebinding --
	// the same false positive already justified at provider.go's doJSON,
	// getJSON and postJSON for the identical reason.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("authn: build sms gateway request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// #nosec G704 -- the same false positive as this function's earlier
	// #nosec comment, now at the point gosec's taint analysis actually
	// flags the request's execution rather than its construction.
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("authn: send sms: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain (bounded) rather than discard: an unread body on a keep-alive
	// connection prevents the transport from reusing it.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxSMSGatewayResponseBytes))

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("authn: sms gateway returned status %d", resp.StatusCode)
	}
	return nil
}

// compile-time checks that both transports satisfy SMSSender.
var (
	_ SMSSender = (*consoleSMSSender)(nil)
	_ SMSSender = (*httpSMSSender)(nil)
)
