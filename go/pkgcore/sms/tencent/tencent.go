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

// Config is everything NewSender needs to sign and send one message through
// Tencent Cloud SMS. Every field except Region is required; a missing one is
// refused by NewSender with an error naming the field.
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
	// TemplateID is the approved message template's id (Template ID).
	TemplateID string
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

// sender is the tencent SMSSender implementation: one TC3-signed JSON POST
// to the fixed gateway (sign.go).
type sender struct {
	secretID   string
	secretKey  string
	sdkAppID   string
	signName   string
	templateID string
	region     string
	client     *http.Client

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
// send, not after a phone number already paid for the attempt.
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
	if cfg.TemplateID == "" {
		return nil, errors.New("tencent: config: TemplateID is required")
	}
	region := cfg.Region
	if region == "" {
		region = defaultRegion
	}

	s := &sender{
		secretID:   cfg.SecretID,
		secretKey:  cfg.SecretKey,
		sdkAppID:   cfg.SdkAppID,
		signName:   cfg.SignName,
		templateID: cfg.TemplateID,
		region:     region,
		client:     safehttp.NewGuard().Client(),
		now:        time.Now,
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
// gateway. The message text travels as the template's FIRST (and only)
// positional parameter -- Tencent's templates declare positional variables
// substituted by TemplateParamSet -- so the registered template must declare
// exactly one variable, which receives the whole rendered message.
func (s *sender) Send(ctx context.Context, msg pkgcore.SMS) error {
	payload, err := json.Marshal(sendSmsRequest{
		PhoneNumberSet:   []string{msg.To},
		SmsSdkAppID:      s.sdkAppID,
		SignName:         s.signName,
		TemplateID:       s.templateID,
		TemplateParamSet: []string{msg.Text},
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
		return fmt.Errorf("tencent sms: send refused: %s: %s (request %s)", envelope.Response.Error.Code, envelope.Response.Error.Message, envelope.Response.RequestID)
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
		return fmt.Errorf("tencent sms: send refused: %s: %s (request %s)", code, msg, envelope.Response.RequestID)
	}
	return nil
}

// compile-time check that sender satisfies the seam.
var _ pkgcore.SMSSender = (*sender)(nil)
