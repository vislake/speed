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
// seam lives in THIS module rather than in pkgcore (see this module's
// AGENTS.md: it is not a primitive every module needs), so unlike the
// mailer and object-store cases this validation cannot happen inside
// pkgcore.Kernel -- WithDeploymentMode records the one fact this module
// needs from the host to enforce it itself, at the same wiring-time moment
// NewModule already validates WithKeySource and WithBlindIndexKey.
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
// It is authn's own seam, not a pkgcore one: docs/internal's rule for what
// belongs in the dependency floor every module carries is "a capability
// every module needs", and SMS delivery is specific to this module's phone
// sign-in flow. Per the root CLAUDE.md's dual-deployment-mode rule, it
// still ships two implementations -- NewConsoleSMSSender (standalone,
// zero external dependency) and NewHTTPSMSSender (distributed, a real,
// independently testable second implementation) -- selected by which
// constructor the HOST calls, never by a mode branch inside this package.
type SMSSender interface {
	// Send delivers msg. An error means the message was not delivered;
	// the caller (verification.go) surfaces that as a structured failure
	// rather than pretending the code went out.
	Send(ctx context.Context, msg SMS) error
}

// consoleSMSSender is the standalone deployment mode's SMS transport: it
// writes every message to an injected io.Writer instead of sending
// anything, mirroring docs/internal/03-deployment-modes.md's degradation
// matrix entry for SMS ("printed to stdout").
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
// to its gateway with. The default is the SSRF-guarded client (this
// module's internal/safehttp), which cannot connect to a private address --
// so a test pointing a sender at an httptest server on loopback MUST inject
// a plain client, and does; a deployment has no reason to.
func WithHTTPSMSSenderClient(client *http.Client) HTTPSMSSenderOption {
	return func(c *httpSMSSenderConfig) {
		if client != nil {
			c.httpClient = client
		}
	}
}

// httpSMSSender is the distributed deployment mode's real SMS transport: a
// generic JSON gateway POST. It is deliberately generic rather than a
// specific carrier's SDK -- see this module's AGENTS.md for why the actual
// Aliyun/Tencent Cloud/Twilio adapters are deferred to the M2 notification
// round, and for why this is nonetheless a genuine, testable second
// implementation and not a placeholder: it is offline-testable end to end
// against httptest.NewServer, and its endpoint is validated exactly like
// the enterprise OIDC issuer URL is (internal/safehttp), because both are a
// destination an operator, not this codebase, chose.
type httpSMSSender struct {
	endpoint string
	client   *http.Client
}

// NewHTTPSMSSender returns the distributed deployment mode's SMS transport,
// posting a JSON {"to","text"} body to endpoint.
func NewHTTPSMSSender(endpoint string, opts ...HTTPSMSSenderOption) SMSSender {
	cfg := httpSMSSenderConfig{httpClient: safehttp.NewClient()}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return &httpSMSSender{endpoint: endpoint, client: cfg.httpClient}
}

// httpSMSGatewayRequest is the body posted to the gateway.
type httpSMSGatewayRequest struct {
	To   string `json:"to"`
	Text string `json:"text"`
}

// Send implements SMSSender.
//
// The endpoint is dialled through internal/safehttp's guarded client (the
// default, unless a test overrode it), which refuses a private, loopback,
// link-local or otherwise non-public address at CONNECT time -- the same
// defence this module's enterprise SSO issuer URL gets, and for the same
// reason: an operator, not this codebase, chose the destination.
func (s *httpSMSSender) Send(ctx context.Context, msg SMS) error {
	body, err := json.Marshal(httpSMSGatewayRequest(msg))
	if err != nil {
		return fmt.Errorf("authn: encode sms gateway request: %w", err)
	}

	// #nosec G704 -- gosec's taint analysis flags any http.NewRequestWithContext
	// call whose URL is a variable, but s.endpoint is dialled through
	// internal/safehttp's guarded client by default (this function's own doc
	// comment above), which resolves and dials the exact validated IP,
	// rejecting private/loopback/link-local/CGNAT ranges and defeating DNS
	// rebinding -- the same false positive already justified at provider.go's
	// doJSON, getJSON and postJSON for the identical reason.
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
