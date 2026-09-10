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
	"strings"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/safehttp"
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

// maxResponseBytes bounds how much of Aliyun's response body this adapter
// reads, so a misbehaving or hostile gateway cannot hold a request goroutine
// reading an unbounded body (the same bound pkgcore's own gateway sender
// applies).
const maxResponseBytes = 64 * 1024

// Template is one approved Aliyun message template and the variables it
// declares.
type Template struct {
	// Code is the approved template code (TemplateCode), e.g.
	// "SMS_0000001".
	Code string
	// Params lists the variable names the template declares, exactly as the
	// operator's Aliyun account spells them. Send forwards exactly these
	// names from pkgcore.SMS.Params; a declared variable the message
	// carries no value for is refused before any request, because Aliyun
	// refuses a declared-but-unassigned variable with
	// isv.TEMPLATE_MISSING_PARAMETERS. A message parameter the list does
	// not name is never sent: the operator may have baked that value into
	// the template's fixed text.
	Params []string
}

// Config is everything NewSender needs to sign and send messages through
// Aliyun SMS. Every field is required; a missing or malformed one is refused
// by NewSender with an error naming the field (or the offending Templates
// key), never a field's value.
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
	// Templates maps a message identity to the approved template it is sent
	// through, keyed "<locale>/<message-id>" -- the locale the message was
	// rendered in and the message id it was rendered from, both carried by
	// pkgcore.SMS ("zh-CN/authn.sms.verification_code"). Aliyun has one
	// template per kind of message, so this map is where the operator
	// declares which approved template serves which message in which
	// language; a send whose (locale, message-id) has no entry fails before
	// any request, with no fallback template and no free-text path.
	Templates map[string]Template
}

// Option configures NewSender.
type Option func(*sender)

// WithClient replaces the HTTP client a sender talks to Aliyun's gateway
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

// sender is the aliyun SMSSender implementation: one POST to the fixed
// gateway carrying the RPC-parameter form body, signed per Aliyun's RPC
// mechanism (sign.go).
type sender struct {
	accessKeyID     string
	accessKeySecret string
	signName        string
	templates       map[templateKey]Template
	client          *http.Client

	// now and newNonce are injectable so the unit tier can pin a whole
	// request deterministically (Timestamp and SignatureNonce are the only
	// non-deterministic parameters of the RPC signature). Real sends use
	// the zero-value defaults.
	now      func() time.Time
	newNonce func() (string, error)
}

// NewSender returns a sender that delivers pkgcore's SMS messages through
// Aliyun SMS, or an error naming the Config field that is missing (never its
// value -- AccessKeySecret is a credential). Constructing fails closed:
// Aliyun refuses a send with a business error envelope rather than an HTTP
// error, so an adapter built with a typo'd configuration must fail before
// the first send, not after a phone number already paid for the attempt.
// Every Templates entry is validated here -- the key parses into two
// non-empty halves, the template code is present, no declared variable name
// is empty -- so a send never has to discover a broken map entry.
func NewSender(cfg Config, opts ...Option) (pkgcore.SMSSender, error) {
	if cfg.AccessKeyID == "" {
		return nil, errors.New("aliyun: config: AccessKeyID is required")
	}
	if cfg.AccessKeySecret == "" {
		return nil, errors.New("aliyun: config: AccessKeySecret is required")
	}
	if cfg.SignName == "" {
		return nil, errors.New("aliyun: config: SignName is required")
	}
	if len(cfg.Templates) == 0 {
		return nil, errors.New("aliyun: config: Templates is required")
	}
	templates := make(map[templateKey]Template, len(cfg.Templates))
	for key, tpl := range cfg.Templates {
		locale, messageID, ok := strings.Cut(key, "/")
		if !ok || locale == "" || messageID == "" {
			return nil, fmt.Errorf("aliyun: config: Templates key %q is not of the form \"<locale>/<message-id>\"", key)
		}
		if tpl.Code == "" {
			return nil, fmt.Errorf("aliyun: config: Templates[%q].Code is required", key)
		}
		for _, name := range tpl.Params {
			if name == "" {
				return nil, fmt.Errorf("aliyun: config: Templates[%q].Params carries an empty variable name", key)
			}
		}
		templates[templateKey{locale: locale, messageID: messageID}] = tpl
	}

	s := &sender{
		accessKeyID:     cfg.AccessKeyID,
		accessKeySecret: cfg.AccessKeySecret,
		signName:        cfg.SignName,
		templates:       templates,
		client:          safehttp.NewGuard().Client(),
		now:             time.Now,
		newNonce:        randomNonce,
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

// Send implements pkgcore.SMSSender: a signed SendSms POST to the fixed
// gateway. The message is first mapped to its approved template by
// (msg.Locale, msg.MessageID) and its declared variables are resolved from
// msg.Params -- both before any request, and before the nonce is drawn so a
// refused send costs nothing -- and the whole parameter set -- common RPC
// parameters plus the SendSms-specific ones -- then travels percent-encoded
// with Aliyun's own RFC 3986 rule in the REQUEST LINE's query string with an
// empty body, the exact wire placement both official SDK generations use for
// dysmsapi's SendSms (the legacy alibaba-cloud-sdk-go builds the same
// dysmsapi.aliyuncs.com/?<sorted encoded params incl. Signature> URL; the
// current dysmsapi Tea SDK sends the same set as RPC-style query
// parameters). The signature is computed over the parameter set WITHOUT
// Signature and appended to the set; nothing sensitive rides in any log a
// request line could reach except over TLS, which every byte of the request
// is.
func (s *sender) Send(ctx context.Context, msg pkgcore.SMS) error {
	tpl, ok := s.templates[templateKey{locale: msg.Locale, messageID: msg.MessageID}]
	if !ok {
		return fmt.Errorf("aliyun: send: no template for locale %q and message %q", msg.Locale, msg.MessageID)
	}
	templateValues, err := templateVariables(tpl, msg.Params)
	if err != nil {
		return err
	}
	templateParam, err := json.Marshal(templateValues)
	if err != nil {
		return fmt.Errorf("aliyun: encode template param: %w", err)
	}

	nonce, err := s.newNonce()
	if err != nil {
		return err
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
		"TemplateCode":     tpl.Code,
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

// templateVariables selects the values tpl's declared variables receive from
// params, and only those: a name the message carries no value for is an
// error naming the variable (never a value -- a Params value can be a
// verification code), because Aliyun refuses a declared-but-unassigned
// variable with isv.TEMPLATE_MISSING_PARAMETERS and refusing here saves the
// send. A param the template does not declare is deliberately ignored. The
// returned map is always non-nil, so a template that declares no variables
// marshals to the empty JSON object Aliyun accepts, never to null.
func templateVariables(tpl Template, params map[string]string) (map[string]string, error) {
	values := make(map[string]string, len(tpl.Params))
	for _, name := range tpl.Params {
		value, ok := params[name]
		if !ok {
			return nil, fmt.Errorf("aliyun: send: template %s declares variable %q, which the message carries no value for", tpl.Code, name)
		}
		values[name] = value
	}
	return values, nil
}

// compile-time check that sender satisfies the seam.
var _ pkgcore.SMSSender = (*sender)(nil)
