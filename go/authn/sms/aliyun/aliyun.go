package aliyun

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/authn/internal/safehttp"
)

// gatewayEndpoint is Aliyun SMS's Dysmsapi gateway, the fixed China-site
// endpoint of the API version this adapter speaks (per Aliyun's SendSms
// reference, help.aliyun.com "dysmsapi SendSms"). It is a constant of this
// package, never operator-configurable, so the adapter needs no
// per-destination scheme or address validation of its own beyond the guarded
// default client.
const gatewayEndpoint = "https://dysmsapi.aliyuncs.com/"

// apiAction and apiVersion are the RPC common parameters fixing this
// adapter's target action and API version.
const (
	apiAction  = "SendSms"
	apiVersion = "2017-05-25"
)

// defaultTemplateParamName is what TemplateParamName defaults to when Config
// leaves it empty (see Config.TemplateParamName).
const defaultTemplateParamName = "content"

// maxResponseBytes bounds how much of Aliyun's response body this adapter
// reads, so a misbehaving or hostile gateway cannot hold a request goroutine
// reading an unbounded body (the same bound go/authn's own gateway sender
// applies).
const maxResponseBytes = 64 * 1024

// Config is everything NewSender needs to sign and send one message through
// Aliyun SMS. Every field except TemplateParamName is required; a missing one
// is refused by NewSender with an error naming the field.
type Config struct {
	// AccessKeyID is the Aliyun access key this sender signs with.
	AccessKeyID string
	// AccessKeySecret is the matching secret. It is used only inside the
	// HMAC-SHA1 key derivation and is never echoed by any error this
	// package returns.
	AccessKeySecret string
	// SignName is the approved SMS signature name the send is attributed
	// to, e.g. "speed-test".
	SignName string
	// TemplateCode is the approved message template the send instantiates,
	// e.g. "SMS_0000001".
	TemplateCode string
	// TemplateParamName names the template's single variable that receives
	// the whole message text (see the package doc's "The template
	// boundary"). Empty defaults to "content".
	TemplateParamName string
}

// Option configures NewSender.
type Option func(*sender)

// WithClient replaces the HTTP client a sender talks to Aliyun's gateway
// with. The default is the SSRF-guarded client (authn/internal/safehttp),
// which cannot connect to a private address -- so a test pointing a sender at
// an httptest server on loopback MUST inject a plain client (the test
// server's own), and does. A deployment has no reason to replace it.
func WithClient(client *http.Client) Option {
	return func(s *sender) {
		if client != nil {
			s.client = client
		}
	}
}

// sender is the aliyun SMSSender implementation: one POST to the fixed
// gateway carrying the RPC-parameter form body, signed per Aliyun's RPC
// mechanism (sign.go).
type sender struct {
	accessKeyID       string
	accessKeySecret   string
	signName          string
	templateCode      string
	templateParamName string
	client            *http.Client

	// now and newNonce are injectable so the unit tier can pin a whole
	// request deterministically (Timestamp and SignatureNonce are the only
	// non-deterministic parameters of the RPC signature). Real sends use
	// the zero-value defaults.
	now      func() time.Time
	newNonce func() (string, error)
}

// NewSender returns a sender that delivers go/authn's SMS messages through
// Aliyun SMS, or an error naming the Config field that is missing (never its
// value -- AccessKeySecret is a credential). Constructing fails closed:
// Aliyun refuses a send with a business error envelope rather than an HTTP
// error, so an adapter built with a typo'd configuration must fail before
// the first send, not after a phone number already paid for the attempt.
func NewSender(cfg Config, opts ...Option) (authn.SMSSender, error) {
	if cfg.AccessKeyID == "" {
		return nil, errors.New("aliyun: config: AccessKeyID is required")
	}
	if cfg.AccessKeySecret == "" {
		return nil, errors.New("aliyun: config: AccessKeySecret is required")
	}
	if cfg.SignName == "" {
		return nil, errors.New("aliyun: config: SignName is required")
	}
	if cfg.TemplateCode == "" {
		return nil, errors.New("aliyun: config: TemplateCode is required")
	}
	paramName := cfg.TemplateParamName
	if paramName == "" {
		paramName = defaultTemplateParamName
	}

	s := &sender{
		accessKeyID:       cfg.AccessKeyID,
		accessKeySecret:   cfg.AccessKeySecret,
		signName:          cfg.SignName,
		templateCode:      cfg.TemplateCode,
		templateParamName: paramName,
		client:            safehttp.NewGuard().Client(),
		now:               time.Now,
		newNonce:          randomNonce,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s, nil
}

// randomNonce is the default SignatureNonce source: 128 bits of crypto/rand,
// hex-encoded, unique per request so Aliyun's replay protection can
// distinguish them.
func randomNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("aliyun: generate signature nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// aliyunTimestamp renders now in the exact UTC form Aliyun's RPC signature
// specification fixes (yyyy-MM-ddTHH:mm:ssZ, no fractional seconds).
func aliyunTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// dysmsapiResponse is the SendSms response envelope. Aliyun answers a
// refused send -- bad signature, unapproved template, malformed number -- as
// HTTP 200 with Code other than "OK", never as an HTTP error, so Send must
// parse this envelope to know whether the message went out.
type dysmsapiResponse struct {
	Code      string `json:"Code"`
	Message   string `json:"Message"`
	RequestID string `json:"RequestId"`
	BizID     string `json:"BizId"`
}

// Send implements authn.SMSSender: a signed SendSms POST to the fixed
// gateway. The whole parameter set -- common RPC parameters plus the
// SendSms-specific ones -- travels percent-encoded with Aliyun's own RFC
// 3986 rule in the REQUEST LINE's query string with an empty body, the exact
// wire placement both official SDK generations use for dysmsapi's SendSms
// (the legacy alibaba-cloud-sdk-go builds the same
// dysmsapi.aliyuncs.com/?<sorted encoded params incl. Signature> URL; the
// current dysmsapi Tea SDK sends the same set as RPC-style query
// parameters). The signature is computed over the parameter set WITHOUT
// Signature and appended to the set; nothing sensitive rides in any log a
// request line could reach except over TLS, which every byte of the request
// is.
func (s *sender) Send(ctx context.Context, msg authn.SMS) error {
	nonce, err := s.newNonce()
	if err != nil {
		return err
	}
	templateParam, err := json.Marshal(map[string]string{s.templateParamName: msg.Text})
	if err != nil {
		return fmt.Errorf("aliyun: encode template param: %w", err)
	}

	params := map[string]string{
		"Action":           apiAction,
		"Version":          apiVersion,
		"Format":           "JSON",
		"AccessKeyId":      s.accessKeyID,
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   nonce,
		"SignatureVersion": "1.0",
		"Timestamp":        aliyunTimestamp(s.now()),
		"PhoneNumbers":     msg.To,
		"SignName":         s.signName,
		"TemplateCode":     s.templateCode,
		"TemplateParam":    string(templateParam),
	}
	// The signature is computed over the canonicalized query string of the
	// parameter set WITHOUT Signature, then joined into the set.
	canonical := canonicalQueryString(params)
	params["Signature"] = rpcSignature(s.accessKeySecret, http.MethodPost, canonical)

	endpoint := gatewayEndpoint + "?" + canonicalQueryString(params)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("aliyun: build send request: %w", err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("aliyun: send sms: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain (bounded) rather than discard: an unread body on a keep-alive
	// connection prevents the transport from reusing it.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("aliyun sms: gateway returned status %d", resp.StatusCode)
	}

	var envelope dysmsapiResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("aliyun sms: decode gateway response: %w", err)
	}
	if envelope.Code != "OK" {
		// The code and message are Aliyun's own business-error vocabulary
		// (isv.*, SignatureDoesNotMatch, ...); both surface so an operator
		// can act on the refusal, and neither echoes a credential.
		return fmt.Errorf("aliyun sms: send refused: %s %s (request %s)", envelope.Code, envelope.Message, envelope.RequestID)
	}
	return nil
}

// compile-time check that sender satisfies the seam.
var _ authn.SMSSender = (*sender)(nil)
