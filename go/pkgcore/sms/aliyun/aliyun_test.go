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

// fixedSender returns a sender whose clock and nonce are pinned, so a whole
// signed request is deterministic and the unit tier can assert it against an
// independently precomputed signature (see sign_test.go's vector test). The
// HTTP client is scripted through the caller's RoundTripper.
func fixedSender(rt roundTripFunc, cfg Config) *sender {
	if cfg.TemplateParamName == "" {
		cfg.TemplateParamName = defaultTemplateParamName
	}
	s := &sender{
		accessKeyID:       cfg.AccessKeyID,
		accessKeySecret:   cfg.AccessKeySecret,
		signName:          cfg.SignName,
		templateCode:      cfg.TemplateCode,
		templateParamName: cfg.TemplateParamName,
		client:            &http.Client{Transport: rt},
		now:               func() time.Time { return time.Date(2026, 9, 8, 2, 0, 0, 0, time.UTC) },
		newNonce: func() (string, error) {
			return "fixed-nonce-0000000000000000", nil
		},
	}
	return s
}

const (
	testAccessKeyID     = "LTAI-test-id"
	testAccessKeySecret = "LTAI-test-secret"
	testSignName        = "speed-test"
	testTemplateCode    = "SMS_0000001"
)

func testConfig() Config {
	return Config{
		AccessKeyID:     testAccessKeyID,
		AccessKeySecret: testAccessKeySecret,
		SignName:        testSignName,
		TemplateCode:    testTemplateCode,
	}
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
		{name: "template code", mutate: func(c *Config) { c.TemplateCode = "" }, wantErr: "TemplateCode"},
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

// TestNewSender_EmptyTemplateParamName_DefaultsToContent pins the documented
// default of the one optional Config field.
func TestNewSender_EmptyTemplateParamName_DefaultsToContent(t *testing.T) {
	t.Parallel()

	s, err := NewSender(testConfig())
	if err != nil {
		t.Fatalf("NewSender() error = %v", err)
	}
	if got := s.(*sender).templateParamName; got != "content" {
		t.Errorf("templateParamName = %q, want the documented default content", got)
	}
}

// TestSend_PostsFullySignedSendSmsForm is the offline request-shape proof:
// the transport records exactly one POST to Aliyun's fixed gateway carrying
// the full RPC parameter set -- Signature included -- in the request line's
// query string with an empty body, the wire placement both official SDK
// generations use for dysmsapi's SendSms. The Signature value is
// independently precomputed over that exact set (python oracle; the vector's
// fixed Timestamp and SignatureNonce are this test's injected clock and
// nonce). Every parameter an operator or Aliyun's gateway could care about
// is asserted verbatim, so a renamed field, a missing common parameter, a
// wrong version or a Signature that does not cover the set all fail here.
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
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"Code":"OK","Message":"OK","BizId":"9006","RequestId":"REQ-1"}`)),
			Header:     make(http.Header),
		}, nil
	})

	s := fixedSender(rt, testConfig())
	if err := s.Send(context.Background(), pkgcore.SMS{To: "+8613800000000", Text: "your code is 654321"}); err != nil {
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
		"TemplateParam":    {`{"content":"your code is 654321"}`},
		"Signature":        {"HN7tIYOlG1E+NEnu6FrtNne3vGY="},
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

// TestSend_ConfiguredTemplateParamName_IsUsed proves the operator's own
// template variable name reaches the TemplateParam JSON instead of the
// default.
func TestSend_ConfiguredTemplateParamName_IsUsed(t *testing.T) {
	t.Parallel()

	var gotParam string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotParam = r.URL.Query().Get("TemplateParam")
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"Code":"OK","Message":"OK","RequestId":"REQ-1"}`)),
			Header:     make(http.Header),
		}, nil
	})

	cfg := testConfig()
	cfg.TemplateParamName = "code"
	s := fixedSender(rt, cfg)
	if err := s.Send(context.Background(), pkgcore.SMS{To: "+8613800000000", Text: "123456"}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(gotParam), &decoded); err != nil {
		t.Fatalf("TemplateParam %q is not valid JSON: %v", gotParam, err)
	}
	if decoded["code"] != "123456" {
		t.Errorf("TemplateParam = %v, want the message text under the configured key code", decoded)
	}
	if _, ok := decoded["content"]; ok {
		t.Errorf("TemplateParam = %v, want no default content key once the config names its own", decoded)
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

	s := fixedSender(rt, testConfig())
	err := s.Send(context.Background(), pkgcore.SMS{To: "+8613800000000", Text: "x"})
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

	s := fixedSender(rt, testConfig())
	if err := s.Send(context.Background(), pkgcore.SMS{To: "+8613800000000", Text: "x"}); err == nil {
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

	s := fixedSender(rt, testConfig())
	if err := s.Send(context.Background(), pkgcore.SMS{To: "+8613800000000", Text: "x"}); err == nil {
		t.Fatalf("Send() error = nil, want an envelope decode error for a 200 with a non-envelope body")
	}
}

// TestSend_NonceFailure_ReturnsError proves a crypto/rand failure surfaces
// before any request leaves the process.
func TestSend_NonceFailure_ReturnsError(t *testing.T) {
	t.Parallel()

	s := fixedSender(func(_ *http.Request) (*http.Response, error) {
		t.Error("transport reached; a nonce failure must precede any request")
		return nil, nil
	}, testConfig())
	s.newNonce = func() (string, error) { return "", errTestNonce }
	if err := s.Send(context.Background(), pkgcore.SMS{To: "+8613800000000", Text: "x"}); err == nil {
		t.Fatalf("Send() error = nil, want the nonce failure")
	}
}
