package aliyun

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// errTestNonce is the failure fixedSender's injectable nonce source can be
// pointed at to prove a nonce error precedes any request.
var errTestNonce = errors.New("test nonce failure")

// roundTripFunc adapts a function to http.RoundTripper so a test (or an
// example) can script the transport without a listening server.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fixedSender builds a real sender through NewSender (so the constructor's
// own Templates validation runs) whose clock and nonce are then pinned, so a
// whole signed request is deterministic and the unit tier can assert it
// against an independently precomputed signature (see sign_test.go's vector
// test). The HTTP client is scripted through the caller's RoundTripper.
func fixedSender(t *testing.T, rt roundTripFunc, cfg Config) *sender {
	t.Helper()
	s, err := NewSender(cfg, WithClient(&http.Client{Transport: rt}))
	if err != nil {
		t.Fatalf("NewSender(%+v) error = %v", cfg, err)
	}
	fixed := s.(*sender)
	fixed.now = func() time.Time { return time.Date(2026, 9, 8, 2, 0, 0, 0, time.UTC) }
	fixed.newNonce = func() (string, error) { return "fixed-nonce-0000000000000000", nil }
	return fixed
}

const (
	testAccessKeyID     = "LTAI-test-id"
	testAccessKeySecret = "LTAI-test-secret"
	testSignName        = "speed-test"
	testTemplateCode    = "SMS_0000001"
	testMessageID       = "authn.sms.verification_code"
	testLocale          = "zh-CN"
)

func testConfig() Config {
	return Config{
		AccessKeyID:     testAccessKeyID,
		AccessKeySecret: testAccessKeySecret,
		SignName:        testSignName,
		Templates: map[string]Template{
			testLocale + "/" + testMessageID: {
				Code:   testTemplateCode,
				Params: []string{"code", "minutes"},
			},
		},
	}
}

// testSMS is the message testConfig's single mapping serves: the phone-login
// verification-code message go/authn sends, with both variables of its
// mapped template assigned.
func testSMS() pkgcore.SMS {
	return pkgcore.SMS{
		To:        "+8613800000000",
		Text:      "your code is 654321, valid for 5 minutes",
		MessageID: testMessageID,
		Locale:    testLocale,
		Params:    map[string]string{"code": "654321", "minutes": "5"},
	}
}

// okResponse is the gateway's success envelope.
func okResponse(_ *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"Code":"OK","Message":"OK","BizId":"9006","RequestId":"REQ-1"}`)),
		Header:     make(http.Header),
	}, nil
}

// TestNewSender_MissingField_RefusesNamesField proves construction fails
// closed naming exactly the missing Config field, one at a time, and never
// echoes a value -- AccessKeySecret is a credential and must not appear in
// an error.
func TestNewSender_MissingField_RefusesNamesField(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "access key id", mutate: func(c *Config) { c.AccessKeyID = "" }, wantErr: "AccessKeyID"},
		{name: "access key secret", mutate: func(c *Config) { c.AccessKeySecret = "" }, wantErr: "AccessKeySecret"},
		{name: "sign name", mutate: func(c *Config) { c.SignName = "" }, wantErr: "SignName"},
		{name: "templates", mutate: func(c *Config) { c.Templates = nil }, wantErr: "Templates"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig()
			tc.mutate(&cfg)
			_, err := NewSender(cfg)
			if err == nil {
				t.Fatalf("NewSender(%+v) error = nil, want a refusal naming %s", cfg, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("NewSender error = %v, want it to name %s", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), testAccessKeySecret) {
				t.Errorf("NewSender error = %v, must never echo the secret", err)
			}
		})
	}
}

// TestNewSender_BrokenTemplatesEntry_RefusesNamingKey proves each malformed
// Templates entry -- a key that does not split into two non-empty halves, an
// empty template code, an empty variable name -- is refused at construction,
// with the error naming the offending key or field so an operator can fix
// the map without guessing which entry is broken.
func TestNewSender_BrokenTemplatesEntry_RefusesNamingKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:    "key without a slash",
			mutate:  func(c *Config) { c.Templates = map[string]Template{testLocale: {Code: testTemplateCode}} },
			wantErr: `Templates key "zh-CN"`,
		},
		{
			name:    "empty locale half",
			mutate:  func(c *Config) { c.Templates = map[string]Template{"/" + testMessageID: {Code: testTemplateCode}} },
			wantErr: `Templates key "/authn.sms.verification_code"`,
		},
		{
			name:    "empty message-id half",
			mutate:  func(c *Config) { c.Templates = map[string]Template{testLocale + "/": {Code: testTemplateCode}} },
			wantErr: `Templates key "zh-CN/"`,
		},
		{
			name: "empty template code",
			mutate: func(c *Config) {
				c.Templates = map[string]Template{testLocale + "/" + testMessageID: {Params: []string{"code"}}}
			},
			wantErr: `Templates["zh-CN/authn.sms.verification_code"].Code`,
		},
		{
			name: "empty variable name",
			mutate: func(c *Config) {
				c.Templates = map[string]Template{testLocale + "/" + testMessageID: {Code: testTemplateCode, Params: []string{""}}}
			},
			wantErr: `Templates["zh-CN/authn.sms.verification_code"].Params`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig()
			tc.mutate(&cfg)
			_, err := NewSender(cfg)
			if err == nil {
				t.Fatalf("NewSender(%+v) error = nil, want a refusal naming the broken entry", cfg)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("NewSender error = %v, want it to name %q", err, tc.wantErr)
			}
		})
	}
}

// TestSend_PostsFullySignedSendSmsForm is the offline request-shape proof:
// the transport records exactly one POST to Aliyun's fixed gateway carrying
// the full RPC parameter set -- Signature included -- in the request line's
// query string with an empty body, the wire placement both official SDK
// generations use for dysmsapi's SendSms. The mapped template's code and
// its declared variables' JSON ride as TemplateCode and TemplateParam, and
// the Signature value is independently precomputed over that exact set
// (python oracle; the vector's fixed Timestamp and SignatureNonce are this
// test's injected clock and nonce). Every parameter an operator or Aliyun's
// gateway could care about is asserted verbatim, so a renamed field, a
// missing common parameter, a wrong version or a Signature that does not
// cover the set all fail here.
func TestSend_PostsFullySignedSendSmsForm(t *testing.T) {
	t.Parallel()

	var gotForm url.Values
	var gotURL, gotMethod string
	var gotBodyBytes int64
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		gotMethod = r.Method
		if r.Body != nil {
			gotBodyBytes, _ = io.Copy(io.Discard, r.Body)
		}
		var err error
		gotForm, err = url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			t.Errorf("parse request query %q: %v", r.URL.RawQuery, err)
		}
		return okResponse(r)
	})

	s := fixedSender(t, rt, testConfig())
	if err := s.Send(context.Background(), testSMS()); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if !strings.HasPrefix(gotURL, gatewayEndpoint+"?") {
		t.Errorf("URL = %q, want the fixed gateway %q carrying the query string -- nothing operator-configurable may appear in the request line", gotURL, gatewayEndpoint)
	}
	if gotBodyBytes != 0 {
		t.Errorf("request body = %d bytes, want an empty body (SDK parity: all parameters ride the query string)", gotBodyBytes)
	}

	wantForm := url.Values{
		"Action":           {"SendSms"},
		"Version":          {"2017-05-25"},
		"Format":           {"JSON"},
		"AccessKeyId":      {testAccessKeyID},
		"SignatureMethod":  {"HMAC-SHA1"},
		"SignatureNonce":   {"fixed-nonce-0000000000000000"},
		"SignatureVersion": {"1.0"},
		"Timestamp":        {"2026-09-08T02:00:00Z"},
		"PhoneNumbers":     {"+8613800000000"},
		"SignName":         {testSignName},
		"TemplateCode":     {testTemplateCode},
		"TemplateParam":    {`{"code":"654321","minutes":"5"}`},
		"Signature":        {"g1DBjm3d8FbyqTHtZ9qEnjp9Kpg="},
	}
	if len(gotForm) != len(wantForm) {
		t.Errorf("form has %d parameters, want %d: %v", len(gotForm), len(wantForm), gotForm)
	}
	for k, want := range wantForm {
		if got := gotForm.Get(k); got != want[0] {
			t.Errorf("form[%s] = %q, want %q", k, got, want[0])
		}
	}
}

// TestSend_TemplateSelectedByLocaleAndMessageID proves the mapping's two
// halves both select: with a zh-CN and an en-US entry for the same message
// id, a message rendered in en-US goes through the en-US template with the
// en-US entry's declared variables -- the lookup is by (Locale, MessageID),
// never by either half alone.
func TestSend_TemplateSelectedByLocaleAndMessageID(t *testing.T) {
	t.Parallel()

	var gotCode, gotParam string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotCode = r.URL.Query().Get("TemplateCode")
		gotParam = r.URL.Query().Get("TemplateParam")
		return okResponse(r)
	})

	cfg := testConfig()
	cfg.Templates[testLocale+"/"+testMessageID] = Template{Code: testTemplateCode, Params: []string{"code"}}
	cfg.Templates["en-US/"+testMessageID] = Template{Code: "SMS_0000002", Params: []string{"code", "minutes"}}
	s := fixedSender(t, rt, cfg)

	msg := testSMS()
	msg.Locale = "en-US"
	if err := s.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if gotCode != "SMS_0000002" {
		t.Errorf("TemplateCode = %q, want the en-US entry's SMS_0000002", gotCode)
	}
	if gotParam != `{"code":"654321","minutes":"5"}` {
		t.Errorf("TemplateParam = %q, want the en-US entry's declared variables", gotParam)
	}
}

// TestSend_ForwardsOnlyDeclaredVariables proves the template's declared list
// is the whole wire contract: a message parameter the mapped template does
// not declare never appears in TemplateParam, even though the message
// carries it.
func TestSend_ForwardsOnlyDeclaredVariables(t *testing.T) {
	t.Parallel()

	var gotParam string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotParam = r.URL.Query().Get("TemplateParam")
		return okResponse(r)
	})

	cfg := testConfig()
	cfg.Templates[testLocale+"/"+testMessageID] = Template{Code: testTemplateCode, Params: []string{"code"}}
	s := fixedSender(t, rt, cfg)

	msg := testSMS()
	msg.Params["unused_by_template"] = "must not be sent"
	if err := s.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	var decoded map[string]string
	if err := json.Unmarshal([]byte(gotParam), &decoded); err != nil {
		t.Fatalf("TemplateParam %q is not valid JSON: %v", gotParam, err)
	}
	if len(decoded) != 1 || decoded["code"] != "654321" {
		t.Errorf("TemplateParam = %v, want exactly the declared variable code=654321", decoded)
	}
}

// TestSend_TemplateWithNoVariables_MarshalsEmptyObject proves a template
// declaring no variables sends TemplateParam as the empty JSON object `{}`,
// never as `null` -- a null body is not the shape Aliyun's SendSms accepts.
func TestSend_TemplateWithNoVariables_MarshalsEmptyObject(t *testing.T) {
	t.Parallel()

	var gotParam string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotParam = r.URL.Query().Get("TemplateParam")
		return okResponse(r)
	})

	cfg := testConfig()
	cfg.Templates[testLocale+"/"+testMessageID] = Template{Code: testTemplateCode}
	s := fixedSender(t, rt, cfg)

	if err := s.Send(context.Background(), testSMS()); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if gotParam != `{}` {
		t.Errorf("TemplateParam = %q, want the empty JSON object {} -- never null", gotParam)
	}
}

// TestSend_NoMappedTemplate_RefusedBeforeRequest proves a message whose
// (Locale, MessageID) has no Templates entry -- including one whose
// MessageID or Locale is empty, which can match no entry -- is refused
// before any request leaves the process, with no fallback template and no
// free-text path. The transport would fail the test if it were reached.
func TestSend_NoMappedTemplate_RefusedBeforeRequest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(*pkgcore.SMS)
	}{
		{name: "unmapped message id", mutate: func(m *pkgcore.SMS) { m.MessageID = "notification.contact.verify_code.sms" }},
		{name: "unmapped locale", mutate: func(m *pkgcore.SMS) { m.Locale = "en-US" }},
		{name: "empty message id", mutate: func(m *pkgcore.SMS) { m.MessageID = "" }},
		{name: "empty locale", mutate: func(m *pkgcore.SMS) { m.Locale = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				t.Error("transport reached; an unmapped message must be refused before any request")
				return nil, nil
			})
			s := fixedSender(t, rt, testConfig())
			msg := testSMS()
			tc.mutate(&msg)
			err := s.Send(context.Background(), msg)
			if err == nil {
				t.Fatalf("Send(%+v) error = nil, want a no-mapped-template refusal", msg)
			}
			if !strings.Contains(err.Error(), "no template for locale") {
				t.Errorf("Send() error = %v, want it to say no template was mapped", err)
			}
		})
	}
}

// TestSend_MissingDeclaredVariable_RefusedBeforeRequest proves a declared
// template variable with no value in SMS.Params is refused before any
// request -- Aliyun would refuse the same message with
// isv.TEMPLATE_MISSING_PARAMETERS, after the send was already billed -- and
// that the refusal names the missing variable but never a value: the values
// here can be credentials, so an error carrying one would be a leak.
func TestSend_MissingDeclaredVariable_RefusedBeforeRequest(t *testing.T) {
	t.Parallel()

	rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		t.Error("transport reached; a missing declared variable must be refused before any request")
		return nil, nil
	})
	s := fixedSender(t, rt, testConfig())

	msg := testSMS()
	delete(msg.Params, "minutes")
	err := s.Send(context.Background(), msg)
	if err == nil {
		t.Fatalf("Send(%+v) error = nil, want a refusal naming the missing variable", msg)
	}
	if !strings.Contains(err.Error(), `"minutes"`) {
		t.Errorf("Send() error = %v, want it to name the missing variable minutes", err)
	}
	if strings.Contains(err.Error(), "654321") {
		t.Errorf("Send() error = %v, must never echo a parameter value", err)
	}
}

// TestSend_VendorBusinessRefusal_SurfacesCodeAndMessage proves a refused send
// -- Aliyun answers those as HTTP 200 with a Code other than "OK" -- returns
// an error carrying Aliyun's own code and message, never a silent success.
func TestSend_VendorBusinessRefusal_SurfacesCodeAndMessage(t *testing.T) {
	t.Parallel()

	rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"Code":"isv.SMS_SIGNATURE_ILLEGAL","Message":"The specified signature is not approved","RequestId":"REQ-REFUSED"}`)),
			Header:     make(http.Header),
		}, nil
	})

	s := fixedSender(t, rt, testConfig())
	err := s.Send(context.Background(), testSMS())
	if err == nil {
		t.Fatalf("Send() error = nil, want the vendor's business refusal")
	}
	if !strings.Contains(err.Error(), "isv.SMS_SIGNATURE_ILLEGAL") || !strings.Contains(err.Error(), "not approved") {
		t.Errorf("Send() error = %v, want it to carry Aliyun's code and message", err)
	}
}

// TestSend_HTTPErrorStatus_ReturnsError proves a non-2xx gateway answer
// surfaces as an error.
func TestSend_HTTPErrorStatus_ReturnsError(t *testing.T) {
	t.Parallel()

	rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("gateway down")),
			Header:     make(http.Header),
		}, nil
	})

	s := fixedSender(t, rt, testConfig())
	if err := s.Send(context.Background(), testSMS()); err == nil {
		t.Fatalf("Send() error = nil, want an error for a 503 gateway response")
	}
}

// TestSend_UnparsableOKBody_ReturnsError proves a 200 whose body is not the
// documented envelope cannot be taken for a success -- a message only went
// out when the envelope's own Code says OK.
func TestSend_UnparsableOKBody_ReturnsError(t *testing.T) {
	t.Parallel()

	rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`<html>not the envelope</html>`)),
			Header:     make(http.Header),
		}, nil
	})

	s := fixedSender(t, rt, testConfig())
	if err := s.Send(context.Background(), testSMS()); err == nil {
		t.Fatalf("Send() error = nil, want an envelope decode error for a 200 with a non-envelope body")
	}
}

// TestSend_NonceFailure_ReturnsError proves a crypto/rand failure surfaces
// before any request leaves the process.
func TestSend_NonceFailure_ReturnsError(t *testing.T) {
	t.Parallel()

	s := fixedSender(t, func(_ *http.Request) (*http.Response, error) {
		t.Error("transport reached; a nonce failure must precede any request")
		return nil, nil
	}, testConfig())
	s.newNonce = func() (string, error) { return "", errTestNonce }
	if err := s.Send(context.Background(), testSMS()); err == nil {
		t.Fatalf("Send() error = nil, want the nonce failure")
	}
}
