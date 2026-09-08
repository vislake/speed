package tencent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/authn"
)

// roundTripFunc adapts a function to http.RoundTripper so a test (or an
// example) can script the transport without a listening server.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fixedSender returns a sender whose clock is pinned to fixedTimestamp, so a
// whole signed request is deterministic and the unit tier can assert it
// against the independently precomputed signature (see sign_test.go's vector
// tests). The HTTP client is scripted through the caller's RoundTripper.
func fixedSender(rt roundTripFunc, cfg Config) *sender {
	if cfg.Region == "" {
		cfg.Region = defaultRegion // NewSender's own default; this helper bypasses it
	}
	s := &sender{
		secretID:   cfg.SecretID,
		secretKey:  cfg.SecretKey,
		sdkAppID:   cfg.SdkAppID,
		signName:   cfg.SignName,
		templateID: cfg.TemplateID,
		region:     cfg.Region,
		client:     &http.Client{Transport: rt},
		now:        func() time.Time { return fixedTimestamp },
	}
	return s
}

const (
	testSecretID  = "TC3-test-id"
	testSecretKey = "TC3-test-secret"
	testSdkAppID  = "1400006666"
	testSignName  = "speed-test"
	testTemplID   = "1234567"
)

func testConfig() Config {
	return Config{
		SecretID:   testSecretID,
		SecretKey:  testSecretKey,
		SdkAppID:   testSdkAppID,
		SignName:   testSignName,
		TemplateID: testTemplID,
	}
}

// TestNewSender_MissingField_RefusesNamesField proves construction fails
// closed naming exactly the missing Config field, one at a time, and never
// echoes a value -- SecretKey is a credential and must not appear in an
// error.
func TestNewSender_MissingField_RefusesNamesField(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "secret id", mutate: func(c *Config) { c.SecretID = "" }, wantErr: "SecretID"},
		{name: "secret key", mutate: func(c *Config) { c.SecretKey = "" }, wantErr: "SecretKey"},
		{name: "sdk app id", mutate: func(c *Config) { c.SdkAppID = "" }, wantErr: "SdkAppID"},
		{name: "sign name", mutate: func(c *Config) { c.SignName = "" }, wantErr: "SignName"},
		{name: "template id", mutate: func(c *Config) { c.TemplateID = "" }, wantErr: "TemplateID"},
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
			if strings.Contains(err.Error(), testSecretKey) {
				t.Errorf("NewSender error = %v, must never echo the secret", err)
			}
		})
	}
}

// TestNewSender_EmptyRegion_DefaultsToApGuangzhou pins the documented
// default of the one optional Config field.
func TestNewSender_EmptyRegion_DefaultsToApGuangzhou(t *testing.T) {
	t.Parallel()

	s, err := NewSender(testConfig())
	if err != nil {
		t.Fatalf("NewSender() error = %v", err)
	}
	if got := s.(*sender).region; got != "ap-guangzhou" {
		t.Errorf("region = %q, want the documented default ap-guangzhou", got)
	}
}

// TestSend_PostsSignedSendSmsRequest is the offline request-shape proof: the
// transport records exactly one POST to Tencent's fixed gateway whose body is
// the byte-exact SendSms JSON and whose headers -- the X-TC-* quartet, the
// exact content type, and an Authorization header independently precomputed
// over those exact bytes (python oracle; the vector's fixed clock is this
// test's injected now) -- match what a genuine request must carry. A renamed
// field, a wrong version, a drifted body or a signature that does not cover
// the sent bytes all fail here.
func TestSend_PostsSignedSendSmsRequest(t *testing.T) {
	t.Parallel()

	const wantBody = `{"PhoneNumberSet":["+8613800000000"],"SmsSdkAppId":"1400006666","SignName":"speed-test","TemplateId":"1234567","TemplateParamSet":["your code is 654321"]}`
	const wantAuthz = "TC3-HMAC-SHA256 Credential=TC3-test-id/2026-05-28/sms/tc3_request, SignedHeaders=content-type;host, Signature=fb2b497248a5517203af23cceef807fd13136eeea441a631dbbbc8979ee93dd4"

	var gotBody, gotURL, gotMethod string
	gotHeaders := make(http.Header)
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		gotMethod = r.Method
		gotHeaders = r.Header.Clone()
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		gotBody = string(raw)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"Response":{"SendStatusSet":[{"SerialNo":"111","Code":"Ok","Message":"send success","IsoCode":"CN"}],"RequestId":"REQ-1"}}`)),
			Header:     make(http.Header),
		}, nil
	})

	s := fixedSender(rt, testConfig())
	if err := s.Send(context.Background(), authn.SMS{To: "+8613800000000", Text: "your code is 654321"}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotURL != gatewayEndpoint {
		t.Errorf("URL = %q, want the fixed gateway %q -- nothing operator-configurable may appear in the request line", gotURL, gatewayEndpoint)
	}
	if gotBody != wantBody {
		t.Errorf("body = %s\nwant    %s", gotBody, wantBody)
	}
	if got := gotHeaders.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (the exact value the signature covers, SDK parity)", got)
	}
	if got := gotHeaders.Get("X-TC-Action"); got != "SendSms" {
		t.Errorf("X-TC-Action = %q, want SendSms", got)
	}
	if got := gotHeaders.Get("X-TC-Version"); got != "2021-01-11" {
		t.Errorf("X-TC-Version = %q, want 2021-01-11", got)
	}
	if got := gotHeaders.Get("X-TC-Timestamp"); got != "1780000000" {
		t.Errorf("X-TC-Timestamp = %q, want the fixed vector timestamp 1780000000", got)
	}
	if got := gotHeaders.Get("X-TC-Region"); got != "ap-guangzhou" {
		t.Errorf("X-TC-Region = %q, want the default ap-guangzhou", got)
	}
	if got := gotHeaders.Get("Authorization"); got != wantAuthz {
		t.Errorf("Authorization = %q\nwant            %q", got, wantAuthz)
	}
}

// TestSend_ConfiguredRegion_ReachesTheRegionHeader proves an explicit
// Config.Region travels in X-TC-Region instead of the default.
func TestSend_ConfiguredRegion_ReachesTheRegionHeader(t *testing.T) {
	t.Parallel()

	var gotRegion string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotRegion = r.Header.Get("X-TC-Region")
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"Response":{"SendStatusSet":[{"Code":"Ok","Message":"send success"}],"RequestId":"REQ-1"}}`)),
			Header:     make(http.Header),
		}, nil
	})

	cfg := testConfig()
	cfg.Region = "ap-hongkong"
	s := fixedSender(rt, cfg)
	if err := s.Send(context.Background(), authn.SMS{To: "+85290000000", Text: "x"}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if gotRegion != "ap-hongkong" {
		t.Errorf("X-TC-Region = %q, want the configured ap-hongkong", gotRegion)
	}
}

// TestSend_EnvelopeError_ReturnsRefusal proves a refused send -- answered
// inside the 200 envelope's own Error, the shape Tencent uses for signature,
// region and permission failures -- returns an error carrying Tencent's code
// and message.
func TestSend_EnvelopeError_ReturnsRefusal(t *testing.T) {
	t.Parallel()

	rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"Response":{"Error":{"Code":"AuthFailure.SignatureFailure","Message":"The provided credentials could not be validated"},"RequestId":"REQ-REFUSED"}}`)),
			Header:     make(http.Header),
		}, nil
	})

	s := fixedSender(rt, testConfig())
	err := s.Send(context.Background(), authn.SMS{To: "+8613800000000", Text: "x"})
	if err == nil {
		t.Fatalf("Send() error = nil, want the envelope's refusal")
	}
	if !strings.Contains(err.Error(), "AuthFailure.SignatureFailure") {
		t.Errorf("Send() error = %v, want it to carry Tencent's error code", err)
	}
}

// TestSend_StatusRowRefusal_ReturnsError proves a per-number refusal -- a
// 200 envelope whose send-status row carries a Code other than "Ok" -- is an
// error, never a silent success (Tencent reports bad numbers and
// template/signature mismatches per row, not per envelope).
func TestSend_StatusRowRefusal_ReturnsError(t *testing.T) {
	t.Parallel()

	rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"Response":{"SendStatusSet":[{"Code":"FailedOperation.TemplateParamSetNotMatchApprovedTemplate","Message":"template params do not match"}],"RequestId":"REQ-REFUSED"}}`)),
			Header:     make(http.Header),
		}, nil
	})

	s := fixedSender(rt, testConfig())
	err := s.Send(context.Background(), authn.SMS{To: "+8613800000000", Text: "x"})
	if err == nil {
		t.Fatalf("Send() error = nil, want the status row's refusal")
	}
	if !strings.Contains(err.Error(), "FailedOperation.TemplateParamSetNotMatchApprovedTemplate") {
		t.Errorf("Send() error = %v, want it to carry the row's code", err)
	}
}

// TestSend_HTTPErrorStatus_ReturnsError proves a non-2xx gateway answer
// surfaces as an error even when its envelope carries no Error (the
// transport-failure shape).
func TestSend_HTTPErrorStatus_ReturnsError(t *testing.T) {
	t.Parallel()

	rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader(`{"Response":{"RequestId":"REQ-503"}}`)),
			Header:     make(http.Header),
		}, nil
	})

	s := fixedSender(rt, testConfig())
	if err := s.Send(context.Background(), authn.SMS{To: "+8613800000000", Text: "x"}); err == nil {
		t.Fatalf("Send() error = nil, want an error for a 503 gateway response")
	}
}

// TestSend_EnvelopeErrorOnNon200_StillReturnsTheEnvelopeRefusal proves the
// refusal wins over the raw status when a non-2xx response carries a
// parseable error envelope -- Tencent's signature failures come back as
// HTTP 401 with the envelope, and the envelope's own code is the actionable
// answer, not the status number.
func TestSend_EnvelopeErrorOnNon200_StillReturnsTheEnvelopeRefusal(t *testing.T) {
	t.Parallel()

	rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Body:       io.NopCloser(strings.NewReader(`{"Response":{"Error":{"Code":"AuthFailure.SignatureFailure","Message":"signature expired"},"RequestId":"REQ-401"}}`)),
			Header:     make(http.Header),
		}, nil
	})

	s := fixedSender(rt, testConfig())
	err := s.Send(context.Background(), authn.SMS{To: "+8613800000000", Text: "x"})
	if err == nil {
		t.Fatalf("Send() error = nil, want the envelope's refusal")
	}
	if !strings.Contains(err.Error(), "signature expired") {
		t.Errorf("Send() error = %v, want the envelope's message, not the raw status alone", err)
	}
}

// TestSend_EmptyStatusSet_ReturnsError proves a 200 envelope with neither an
// Error nor any send-status row cannot be taken for a success.
func TestSend_EmptyStatusSet_ReturnsError(t *testing.T) {
	t.Parallel()

	rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"Response":{"RequestId":"REQ-EMPTY"}}`)),
			Header:     make(http.Header),
		}, nil
	})

	s := fixedSender(rt, testConfig())
	if err := s.Send(context.Background(), authn.SMS{To: "+8613800000000", Text: "x"}); err == nil {
		t.Fatalf("Send() error = nil, want an error for an envelope without a send status")
	}
}

// TestSend_TransportFailure_ReturnsError proves a dial/transport failure
// surfaces as an error and never as a silent success.
func TestSend_TransportFailure_ReturnsError(t *testing.T) {
	t.Parallel()

	s := fixedSender(func(_ *http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	}, testConfig())
	if err := s.Send(context.Background(), authn.SMS{To: "+8613800000000", Text: "x"}); err == nil {
		t.Fatalf("Send() error = nil, want the transport failure")
	}
}
