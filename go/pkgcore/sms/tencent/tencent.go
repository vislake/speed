package tencent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/safehttp"
)

// gatewayEndpoint is Tencent Cloud SMS's fixed API 3.0 endpoint for the
// SendSms action (per Tencent's SendSms reference, cloud.tencent.com
// document/api/382/55981). It is a constant of this package, never
// operator-configurable, so the adapter needs no per-destination scheme or
// address validation of its own beyond the guarded default client.
const gatewayEndpoint = "https://sms.tencentcloudapi.com/"

// gatewayHost is the canonical host header value the TC3 signature signs
// over; it must match the request's actual Host exactly. It is the host of
// gatewayEndpoint and must stay in step with it: an endpoint variant added
// later (a per-region host, say) that changed one without the other would
// sign a Host header the request does not carry, and every signature
// would silently fail Tencent's verification.
const gatewayHost = "sms.tencentcloudapi.com"

// apiAction and apiVersion are the X-TC-Action / X-TC-Version header values
// fixing this adapter's target action and API version.
const (
	apiAction  = "SendSms"
	apiVersion = "2021-01-11"
)

// defaultRegion is what Config.Region defaults to when Config leaves it
// empty: ap-guangzhou, the region Tencent Cloud SMS's own documentation and
// SDK defaults fix.
const defaultRegion = "ap-guangzhou"

// maxResponseBytes bounds how much of Tencent's response body this adapter
// reads, so a misbehaving or hostile gateway cannot hold a request goroutine
// reading an unbounded body (the same bound pkgcore's own gateway sender
// applies).
const maxResponseBytes = 64 * 1024

// Template is one approved Tencent Cloud message template and the seam
// parameter names its positional variables take.
type Template struct {
	// ID is the approved template's id (Template ID).
	ID string
	// Params lists the seam parameter names the template's positional
	// variables take, in the order the template declares them -- the first
	// name fills {1}, the second {2}, and so on -- because TemplateParamSet
	// is positional and the template itself carries no variable names this
	// codebase could read. Send forwards exactly these names from
	// pkgcore.SMS.Params, in this order; a declared name the message
	// carries no value for is refused before any request. An empty list
	// means the template declares no variables, and the send omits
	// TemplateParamSet entirely. A message parameter the list does not name
	// is never sent.
	Params []string
}

// Config is everything NewSender needs to sign and send messages through
// Tencent Cloud SMS. Every field except Region is required; a missing or
// malformed one is refused by NewSender with an error naming the field (or
// the offending Templates key), never a field's value.
type Config struct {
	// SecretID is the Tencent Cloud API secret id (SecretId, AKID...).
	SecretID string
	// SecretKey is the matching secret. It is used only inside the TC3 key
	// derivation and is never echoed by any error this package returns.
	SecretKey string
	// SdkAppID is the SMS SDK AppID (SmsSdkAppId, a 1400... number) of the
	// SMS application this sender sends through -- NOT the account APPID.
	SdkAppID string
	// SignName is the approved SMS signature content the send is
	// attributed to, e.g. "speed-test".
	SignName string
	// Templates maps a message identity to the approved template it is sent
	// through, keyed "<locale>/<message-id>" -- the locale the message was
	// rendered in and the message id it was rendered from, both carried by
	// pkgcore.SMS ("zh-CN/authn.sms.verification_code"). Tencent has one
	// template per kind of message, so this map is where the operator
	// declares which approved template serves which message in which
	// language; a send whose (locale, message-id) has no entry fails before
	// any request, with no fallback template and no free-text path.
	Templates map[string]Template
	// Region is the Tencent Cloud region the send is addressed to (SMS is
	// region-scoped). Empty defaults to ap-guangzhou.
	Region string
}

// Option configures NewSender.
type Option func(*sender)

// WithClient replaces the HTTP client a sender talks to Tencent's gateway
// with. The default is the SSRF-guarded client (pkgcore/safehttp),
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

// templateKey is a parsed Templates key: the message identity a send maps
// through, split into its two halves once at construction so a Send looks up
// a struct rather than re-splitting a string.
type templateKey struct {
	locale    string
	messageID string
}

// sender is the tencent SMSSender implementation: one TC3-signed JSON POST
// to the fixed gateway (sign.go).
type sender struct {
	secretID  string
	secretKey string
	sdkAppID  string
	signName  string
	templates map[templateKey]Template
	region    string
	client    *http.Client

	// now is injectable so the unit tier can pin a whole request
	// deterministically (the TC3 signature derives from the Unix
	// timestamp). Real sends use the zero-value default.
	now func() time.Time
}

// NewSender returns a sender that delivers pkgcore's SMS messages through
// Tencent Cloud SMS, or an error naming the Config field that is missing
// (never its value -- SecretKey is a credential). Constructing fails closed:
// Tencent refuses a send with an error envelope rather than an HTTP error,
// so an adapter built with a typo'd configuration must fail before the first
// send, not after a phone number already paid for the attempt. Every
// Templates entry is validated here -- the key parses into two non-empty
// halves, the template id is present, no declared variable name is empty --
// so a send never has to discover a broken map entry.
func NewSender(cfg Config, opts ...Option) (pkgcore.SMSSender, error) {
	if cfg.SecretID == "" {
		return nil, errors.New("tencent: config: SecretID is required")
	}
	if cfg.SecretKey == "" {
		return nil, errors.New("tencent: config: SecretKey is required")
	}
	if cfg.SdkAppID == "" {
		return nil, errors.New("tencent: config: SdkAppID is required")
	}
	if cfg.SignName == "" {
		return nil, errors.New("tencent: config: SignName is required")
	}
	if len(cfg.Templates) == 0 {
		return nil, errors.New("tencent: config: Templates is required")
	}
	templates := make(map[templateKey]Template, len(cfg.Templates))
	for key, tpl := range cfg.Templates {
		locale, messageID, ok := strings.Cut(key, "/")
		if !ok || locale == "" || messageID == "" {
			return nil, fmt.Errorf("tencent: config: Templates key %q is not of the form \"<locale>/<message-id>\"", key)
		}
		if tpl.ID == "" {
			return nil, fmt.Errorf("tencent: config: Templates[%q].ID is required", key)
		}
		for _, name := range tpl.Params {
			if name == "" {
				return nil, fmt.Errorf("tencent: config: Templates[%q].Params carries an empty variable name", key)
			}
		}
		templates[templateKey{locale: locale, messageID: messageID}] = tpl
	}
	region := cfg.Region
	if region == "" {
		region = defaultRegion
	}

	s := &sender{
		secretID:  cfg.SecretID,
		secretKey: cfg.SecretKey,
		sdkAppID:  cfg.SdkAppID,
		signName:  cfg.SignName,
		templates: templates,
		region:    region,
		client:    safehttp.NewGuard().Client(),
		now:       time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s, nil
}

// sendSmsRequest is the SendSms operation's JSON payload. The field order is
// the order json.Marshal emits, which is part of what the unit tier pins
// (the TC3 signature covers the exact bytes sent).
type sendSmsRequest struct {
	PhoneNumberSet   []string `json:"PhoneNumberSet"`
	SmsSdkAppID      string   `json:"SmsSdkAppId"`
	SignName         string   `json:"SignName"`
	TemplateID       string   `json:"TemplateId"`
	TemplateParamSet []string `json:"TemplateParamSet,omitempty"`
}

// tencentResponse is the API 3.0 response envelope: business errors and
// send-status rows both ride inside "Response" of an HTTP 200 body, while
// signature and transport failures come back as a non-2xx status with the
// same envelope. Send must therefore check the envelope's own Error before
// trusting a 2xx, and parse the envelope even when the status is not 2xx.
type tencentResponse struct {
	Response struct {
		RequestID string `json:"RequestId"`
		Error     *struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
		} `json:"Error"`
		SendStatusSet []struct {
			Code        string `json:"Code"`
			Message     string `json:"Message"`
			PhoneNumber string `json:"PhoneNumber"`
		} `json:"SendStatusSet"`
	} `json:"Response"`
}

// Send implements pkgcore.SMSSender: a TC3-signed SendSms POST to the fixed
// gateway. The message is first mapped to its approved template by
// (msg.Locale, msg.MessageID) and the template's positional parameters --
// Tencent's templates declare positional variables substituted by
// TemplateParamSet, so the mapped template's Params list fixes which seam
// parameter fills {1}, {2} and so on -- are resolved from msg.Params; both
// steps fail before any request, and a template declaring no variables sends
// no TemplateParamSet at all. A refusal that is the destination's own
// verdict on the number is wrapped with pkgcore.ErrTransportPermanent (see
// isNumberVerdict); every other failure returns unwrapped.
func (s *sender) Send(ctx context.Context, msg pkgcore.SMS) error {
	tpl, ok := s.templates[templateKey{locale: msg.Locale, messageID: msg.MessageID}]
	if !ok {
		return fmt.Errorf("tencent: send: no template for locale %q and message %q", msg.Locale, msg.MessageID)
	}
	paramSet, err := templateParamSet(tpl, msg.Params)
	if err != nil {
		return err
	}

	payload, err := json.Marshal(sendSmsRequest{
		PhoneNumberSet:   []string{msg.To},
		SmsSdkAppID:      s.sdkAppID,
		SignName:         s.signName,
		TemplateID:       tpl.ID,
		TemplateParamSet: paramSet,
	})
	if err != nil {
		return fmt.Errorf("tencent: encode send request: %w", err)
	}

	ts := s.now()
	authorization := signTC3(s.secretID, s.secretKey, ts, gatewayHost, "application/json", payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayEndpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("tencent: build send request: %w", err)
	}
	// application/json without a charset suffix, byte-for-byte what the TC3
	// signature covers -- the exact shape the official tencentcloud-sdk-go
	// sends.
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-TC-Action", apiAction)
	req.Header.Set("X-TC-Version", apiVersion)
	req.Header.Set("X-TC-Timestamp", strconv.FormatInt(ts.Unix(), 10))
	req.Header.Set("X-TC-Region", s.region)
	req.Header.Set("Authorization", authorization)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("tencent: send sms: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain (bounded) rather than discard: an unread body on a keep-alive
	// connection prevents the transport from reusing it.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))

	var envelope tencentResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return fmt.Errorf("tencent sms: gateway returned status %d", resp.StatusCode)
		}
		return fmt.Errorf("tencent sms: decode gateway response: %w", err)
	}
	// A refused send -- bad signature, unapproved template, wrong region --
	// is answered with the envelope's own Error, whatever the HTTP status.
	if envelope.Response.Error != nil {
		refusal := fmt.Errorf("tencent sms: send refused: %s: %s (request %s)", envelope.Response.Error.Code, envelope.Response.Error.Message, envelope.Response.RequestID)
		if isNumberVerdict(envelope.Response.Error.Code) {
			return fmt.Errorf("%w: %w", pkgcore.ErrTransportPermanent, refusal)
		}
		return refusal
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("tencent sms: gateway returned status %d (request %s)", resp.StatusCode, envelope.Response.RequestID)
	}
	if len(envelope.Response.SendStatusSet) == 0 {
		return fmt.Errorf("tencent sms: response carried no send status (request %s)", envelope.Response.RequestID)
	}
	// A per-number refusal is NOT an envelope error: the send-status row
	// itself carries the verdict, "Ok" the one success vocabulary value.
	if code := envelope.Response.SendStatusSet[0].Code; code != "Ok" {
		msg := envelope.Response.SendStatusSet[0].Message
		if msg == "" {
			msg = "(no message)"
		}
		refusal := fmt.Errorf("tencent sms: send refused: %s: %s (request %s)", code, msg, envelope.Response.RequestID)
		if isNumberVerdict(code) {
			return fmt.Errorf("%w: %w", pkgcore.ErrTransportPermanent, refusal)
		}
		return refusal
	}
	return nil
}

// isNumberVerdict reports whether a Tencent SendSms error code -- the
// envelope's own Error or a per-number send-status row's Code, one
// vocabulary -- is the destination's own verdict on the phone number, the
// refusal a retry can never change, so Send can mark it with
// pkgcore.ErrTransportPermanent (go/notification answers the marked failure
// by stopping the attempt and bouncing the contact; the sentinel's own doc
// comment draws the boundary):
//
//   - FailedOperation.PhoneNumberInBlacklist: the number is on the opt-out
//     or carrier blacklist.
//   - FailedOperation.PhoneNumberParseFail: the number cannot be parsed as
//     a phone number at all.
//   - InvalidParameterValue.IncorrectPhoneNumber: the number's format is
//     wrong.
//
// Every other code stays unwrapped on purpose. The LimitExceeded.* per-number
// caps (30-second, hourly, daily, duplicate-content) are temporary; the
// signature, template, auth and region refusals are the request's or the
// operator's; envelope system errors and non-2xx statuses are the platform's
// or the connection's. None of them is the destination's verdict, so a
// sender that marked one would have a healthy number bounced over a
// platform or configuration problem.
func isNumberVerdict(code string) bool {
	switch code {
	case "FailedOperation.PhoneNumberInBlacklist",
		"FailedOperation.PhoneNumberParseFail",
		"InvalidParameterValue.IncorrectPhoneNumber":
		return true
	default:
		return false
	}
}

// templateParamSet resolves tpl's positional parameters from params, in the
// template's declared order: the returned slice is the TemplateParamSet
// Tencent substitutes into {1}, {2} and so on. A declared name the message
// carries no value for is an error naming the variable (never a value -- a
// Params value can be a verification code). A template declaring no
// variables returns a nil set, so the request marshals with the
// TemplateParamSet field omitted entirely (its omitempty tag), which is what
// Tencent expects from a template without variables. A param the template
// does not name is deliberately ignored.
func templateParamSet(tpl Template, params map[string]string) ([]string, error) {
	if len(tpl.Params) == 0 {
		return nil, nil
	}
	values := make([]string, 0, len(tpl.Params))
	for _, name := range tpl.Params {
		value, ok := params[name]
		if !ok {
			return nil, fmt.Errorf("tencent: send: template %s declares variable %q, which the message carries no value for", tpl.ID, name)
		}
		values = append(values, value)
	}
	return values, nil
}

// compile-time check that sender satisfies the seam.
var _ pkgcore.SMSSender = (*sender)(nil)
