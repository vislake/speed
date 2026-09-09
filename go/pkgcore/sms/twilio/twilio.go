package twilio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/safehttp"
)

// gatewayBaseURL is Twilio's REST API base, the fixed endpoint of the
// Messages resource of API version 2010-04-01. It is a constant of this
// package, never operator-configurable, so the adapter needs no
// per-destination scheme or address validation of its own beyond the guarded
// default client.
const gatewayBaseURL = "https://api.twilio.com/2010-04-01"

// maxResponseBytes bounds how much of Twilio's response body this adapter
// reads, so a misbehaving or hostile gateway cannot hold a request goroutine
// reading an unbounded body (the same bound pkgcore's own gateway sender
// applies).
const maxResponseBytes = 64 * 1024

// Config is everything NewSender needs to send one message through Twilio's
// Messages API. AccountSID and AuthToken are required, and exactly one of
// From and MessagingServiceSID must be set -- Twilio's Messages resource
// accepts either as the sender, never both (a From used together with a
// MessagingServiceSID must itself belong to that service's sender pool, a
// coupling this adapter deliberately does not guess about).
type Config struct {
	// AccountSID is the owning account's SID (AC...), which names both the
	// Messages resource's URL path and the Basic-auth username.
	AccountSID string
	// AuthToken is the matching API auth token, the Basic-auth password.
	// It is never echoed by any error this package returns.
	AuthToken string
	// From is the sender's phone number (E.164, e.g. "+15005550000"), an
	// alphanumeric sender ID, or a Twilio-owned short code this account may
	// send from. Mutually exclusive with MessagingServiceSID.
	From string
	// MessagingServiceSID is the Messaging Service (MG...) whose sender pool
	// Twilio picks from. Mutually exclusive with From; verification-code
	// traffic commonly routes through a Messaging Service so delivery
	// warnings and pool rotation stay configuration, not code.
	MessagingServiceSID string
}

// Option configures NewSender.
type Option func(*sender)

// WithClient replaces the HTTP client a sender talks to Twilio's API with.
// The default is the SSRF-guarded client (pkgcore/safehttp), which
// cannot connect to a private address -- so a test pointing a sender at an
// httptest server on loopback MUST inject a plain client (the test server's
// own), and does. A deployment has no reason to replace it.
func WithClient(client *http.Client) Option {
	return func(s *sender) {
		if client != nil {
			s.client = client
		}
	}
}

// sender is the twilio SMSSender implementation: one Basic-authenticated
// form POST to the account's Messages resource.
type sender struct {
	accountSID          string
	authToken           string
	from                string
	messagingServiceSID string
	client              *http.Client
}

// NewSender returns a sender that delivers pkgcore's SMS messages through
// Twilio's Messages API, or an error naming the Config field that is missing
// or the sender-selection conflict. Constructing fails closed: Twilio
// refuses an invalid send with a 4xx error envelope, so an adapter built
// with a typo'd configuration must fail before the first send, not after a
// phone number already paid for the attempt.
func NewSender(cfg Config, opts ...Option) (pkgcore.SMSSender, error) {
	if cfg.AccountSID == "" {
		return nil, errors.New("twilio: config: AccountSID is required")
	}
	if cfg.AuthToken == "" {
		return nil, errors.New("twilio: config: AuthToken is required")
	}
	if cfg.From == "" && cfg.MessagingServiceSID == "" {
		return nil, errors.New("twilio: config: exactly one of From or MessagingServiceSID is required")
	}
	if cfg.From != "" && cfg.MessagingServiceSID != "" {
		return nil, errors.New("twilio: config: From and MessagingServiceSID are mutually exclusive; a From sent together with a MessagingServiceSID must belong to that service's own sender pool, which this adapter does not guess about")
	}

	s := &sender{
		accountSID:          cfg.AccountSID,
		authToken:           cfg.AuthToken,
		from:                cfg.From,
		messagingServiceSID: cfg.MessagingServiceSID,
		client:              safehttp.NewGuard().Client(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s, nil
}

// twilioErrorEnvelope is the JSON body Twilio answers a refused send with:
// a 4xx/5xx status plus a code and message from Twilio's own error
// vocabulary (a bad number, an unowned From, a trial-account restriction).
type twilioErrorEnvelope struct {
	Code     int    `json:"code"`
	Message  string `json:"message"`
	MoreInfo string `json:"more_info"`
}

// Send implements pkgcore.SMSSender: one POST to this account's Messages
// resource, Basic-authenticated with AccountSID and AuthToken, whose form
// body carries To, the message text as Body, and the configured sender
// (From or MessagingServiceSid). The message text rides free-form -- Twilio
// is the one vendor among this module's adapters with a free-text send -- and
// everything travels over TLS inside the request.
func (s *sender) Send(ctx context.Context, msg pkgcore.SMS) error {
	form := url.Values{
		"To":   {msg.To},
		"Body": {msg.Text},
	}
	if s.from != "" {
		form.Set("From", s.from)
	} else {
		form.Set("MessagingServiceSid", s.messagingServiceSID)
	}

	endpoint := gatewayBaseURL + "/Accounts/" + url.PathEscape(s.accountSID) + "/Messages.json"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return fmt.Errorf("twilio: build send request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(s.accountSID, s.authToken)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("twilio: send sms: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain (bounded) rather than discard: an unread body on a keep-alive
	// connection prevents the transport from reusing it.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return nil
	}

	// A refused send carries Twilio's own error vocabulary in the body;
	// surface it when the body parses, the bare status otherwise.
	var envelope twilioErrorEnvelope
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Message != "" {
		return fmt.Errorf("twilio sms: send refused: status %d: %s (code %d)", resp.StatusCode, envelope.Message, envelope.Code)
	}
	return fmt.Errorf("twilio sms: gateway returned status %d", resp.StatusCode)
}

// compile-time check that sender satisfies the seam.
var _ pkgcore.SMSSender = (*sender)(nil)
