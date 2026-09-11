package tencent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// roundTripFunc adapts a function to http.RoundTripper so a test (or an
// example) can script the transport without a listening server.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fixedSender builds a real sender through NewSender (so the constructor's
// own Templates and Region handling runs) whose clock is then pinned to
// fixedTimestamp, so a whole signed request is deterministic and the unit
// tier can assert it against the independently precomputed signature (see
// sign_test.go's vector tests). The HTTP client is scripted through the
// caller's RoundTripper.
func fixedSender(t *testing.T, rt roundTripFunc, cfg Config) *sender {
	t.Helper()
	s, err := NewSender(cfg, WithClient(&http.Client{Transport: rt}))
	if err != nil {
		t.Fatalf("NewSender(%+v) error = %v", cfg, err)
	}
	fixed := s.(*sender)
	fixed.now = func() time.Time { return fixedTimestamp }
	return fixed
}

const (
	testSecretID  = "TC3-test-id"
	testSecretKey = "TC3-test-secret"
	testSdkAppID  = "1400006666"
	testSignName  = "speed-test"
	testTemplID   = "1234567"
	testMessageID = "authn.sms.verification_code"
	testLocale    = "zh-CN"
)

func testConfig() Config {
	return Config{
		SecretID:  testSecretID,
		SecretKey: testSecretKey,
		SdkAppID:  testSdkAppID,
		SignName:  testSignName,
		Templates: map[string]Template{
			testLocale + "/" + testMessageID: {
				ID:     testTemplID,
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

// okResponse is the gateway's success envelope for one sent message.
func okResponse(_ *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"Response":{"SendStatusSet":[{"SerialNo":"111","Code":"Ok","Message":"send success","IsoCode":"CN"}],"RequestId":"REQ-1"}}`)),
		Header:     make(http.Header),
	}, nil
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
			if strings.Contains(err.Error(), testSecretKey) {
				t.Errorf("NewSender error = %v, must never echo the secret", err)
			}
		})
	}
}

// TestNewSender_BrokenTemplatesEntry_RefusesNamingKey proves each malformed
// Templates entry -- a key that does not split into two non-empty halves, an
// empty template id, an empty variable name -- is refused at construction,
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
			mutate:  func(c *Config) { c.Templates = map[string]Template{testLocale: {ID: testTemplID}} },
			wantErr: `Templates key "zh-CN"`,
		},
		{
			name:    "empty locale half",
			mutate:  func(c *Config) { c.Templates = map[string]Template{"/" + testMessageID: {ID: testTemplID}} },
			wantErr: `Templates key "/authn.sms.verification_code"`,
		},
		{
			name:    "empty message-id half",
			mutate:  func(c *Config) { c.Templates = map[string]Template{testLocale + "/": {ID: testTemplID}} },
			wantErr: `Templates key "zh-CN/"`,
		},
		{
			name: "empty template id",
			mutate: func(c *Config) {
				c.Templates = map[string]Template{testLocale + "/" + testMessageID: {Params: []string{"code"}}}
			},
			wantErr: `Templates["zh-CN/authn.sms.verification_code"].ID`,
		},
		{
			name: "empty variable name",
			mutate: func(c *Config) {
				c.Templates = map[string]Template{testLocale + "/" + testMessageID: {ID: testTemplID, Params: []string{""}}}
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
// the byte-exact SendSms JSON -- the mapped template's id, and its declared
// variables positional -- and whose headers -- the X-TC-* quartet, the exact
// content type, and an Authorization header independently precomputed over
// those exact bytes (python oracle; the vector's fixed clock is this test's
// injected now) -- match what a genuine request must carry. A renamed field,
// a wrong version, a drifted body or a signature that does not cover the
// sent bytes all fail here.
func TestSend_PostsSignedSendSmsRequest(t *testing.T) {
	t.Parallel()

	const wantBody = `{"PhoneNumberSet":["+8613800000000"],"SmsSdkAppId":"1400006666","SignName":"speed-test","TemplateId":"1234567","TemplateParamSet":["654321","5"]}`
	const wantAuthz = "TC3-HMAC-SHA256 Credential=TC3-test-id/2026-05-28/sms/tc3_request, SignedHeaders=content-type;host, Signature=5c34bcb91b20068fae8b8be64675af89abfa3ccc0dd96d03edcdf5bb0851c453"

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
		return okResponse(r)
	})

	s := fixedSender(t, rt, testConfig())
	if err := s.Send(context.Background(), testSMS()); err != nil {
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

// TestSend_TemplateParamSetFollowsDeclaredOrder proves TemplateParamSet is
// built in the mapped template's declared order, not in map-iteration order:
// the template here declares minutes BEFORE code, and the wire set must be
// ["5","654321"] -- the positional variables {1} and {2} would otherwise
// receive each other's values.
func TestSend_TemplateParamSetFollowsDeclaredOrder(t *testing.T) {
	t.Parallel()

	var gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		return okResponse(r)
	})

	cfg := testConfig()
	cfg.Templates[testLocale+"/"+testMessageID] = Template{ID: testTemplID, Params: []string{"minutes", "code"}}
	s := fixedSender(t, rt, cfg)
	if err := s.Send(context.Background(), testSMS()); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	var got sendSmsRequest
	if err := json.Unmarshal([]byte(gotBody), &got); err != nil {
		t.Fatalf("decode request body %q: %v", gotBody, err)
	}
	want := []string{"5", "654321"}
	if len(got.TemplateParamSet) != len(want) {
		t.Fatalf("TemplateParamSet = %v, want %v (declared order)", got.TemplateParamSet, want)
	}
	for i := range want {
		if got.TemplateParamSet[i] != want[i] {
			t.Errorf("TemplateParamSet[%d] = %q, want %q (declared order, never map iteration order)", i, got.TemplateParamSet[i], want[i])
		}
	}
}

// TestSend_TemplateDeclaringNoVariables_OmitsParamSet proves a template that
// declares no variables sends no TemplateParamSet field at all (the
// omitempty contract), rather than an empty array.
func TestSend_TemplateDeclaringNoVariables_OmitsParamSet(t *testing.T) {
	t.Parallel()

	var gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		return okResponse(r)
	})

	cfg := testConfig()
	cfg.Templates[testLocale+"/"+testMessageID] = Template{ID: testTemplID}
	s := fixedSender(t, rt, cfg)
	if err := s.Send(context.Background(), testSMS()); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	if strings.Contains(gotBody, "TemplateParamSet") {
		t.Errorf("body = %s, want the TemplateParamSet field absent entirely for a template declaring no variables", gotBody)
	}
}

// TestSend_MappedTemplate_IgnoresUndeclaredParams proves a message parameter
// the mapped template does not name never reaches the wire.
func TestSend_MappedTemplate_IgnoresUndeclaredParams(t *testing.T) {
	t.Parallel()

	var gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		return okResponse(r)
	})

	s := fixedSender(t, rt, testConfig())
	msg := testSMS()
	msg.Params["unused_by_template"] = "must not be sent"
	if err := s.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if strings.Contains(gotBody, "unused_by_template") || strings.Contains(gotBody, "must not be sent") {
		t.Errorf("body = %s, want no trace of a parameter the template does not declare", gotBody)
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
// request, and that the refusal names the missing variable but never a
// value: the values here can be credentials, so an error carrying one would
// be a leak.
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

// TestSend_ConfiguredRegion_ReachesTheRegionHeader proves an explicit
// Config.Region travels in X-TC-Region instead of the default.
func TestSend_ConfiguredRegion_ReachesTheRegionHeader(t *testing.T) {
	t.Parallel()

	var gotRegion string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotRegion = r.Header.Get("X-TC-Region")
		return okResponse(r)
	})

	cfg := testConfig()
	cfg.Region = "ap-hongkong"
	s := fixedSender(t, rt, cfg)
	msg := testSMS()
	msg.To = "+85290000000"
	if err := s.Send(context.Background(), msg); err != nil {
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

	s := fixedSender(t, rt, testConfig())
	err := s.Send(context.Background(), testSMS())
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

	s := fixedSender(t, rt, testConfig())
	err := s.Send(context.Background(), testSMS())
	if err == nil {
		t.Fatalf("Send() error = nil, want the status row's refusal")
	}
	if !strings.Contains(err.Error(), "FailedOperation.TemplateParamSetNotMatchApprovedTemplate") {
		t.Errorf("Send() error = %v, want it to carry the row's code", err)
	}
}

// TestSend_HTTPErrorStatus_ReturnsError proves a non-2xx gateway answer
// surfaces as an error even when its envelope carries no Error (the
// transport-failure shape), unwrapped by the permanent signal: a gateway
// outage says nothing about the destination number.
func TestSend_HTTPErrorStatus_ReturnsError(t *testing.T) {
	t.Parallel()

	rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader(`{"Response":{"RequestId":"REQ-503"}}`)),
			Header:     make(http.Header),
		}, nil
	})

	s := fixedSender(t, rt, testConfig())
	err := s.Send(context.Background(), testSMS())
	if err == nil {
		t.Fatalf("Send() error = nil, want an error for a 503 gateway response")
	}
	if errors.Is(err, pkgcore.ErrTransportPermanent) {
		t.Errorf("Send() error = %v, want a 503 gateway answer NOT marked with pkgcore.ErrTransportPermanent", err)
	}
}

// TestSend_NumberVerdictRefusals_CarryThePermanentSentinel proves the
// refusals that are Tencent's verdict on the phone number itself -- the
// blacklist, unparsable-number and bad-format codes -- come back wrapping
// pkgcore.ErrTransportPermanent, whichever of the two places the verdict
// rides in: a per-number send-status row (the shape the gateway uses for a
// number-level refusal) or the envelope's own Error. No retry can change
// the number's standing, and go/notification answers the marked failure by
// bouncing the contact.
func TestSend_NumberVerdictRefusals_CarryThePermanentSentinel(t *testing.T) {
	t.Parallel()

	refusals := []struct {
		name    string
		code    string
		envelop bool // the refusal rides in the envelope's Error, not a status row
	}{
		{"a blacklisted number, status row", "FailedOperation.PhoneNumberInBlacklist", false},
		{"an unparsable number, status row", "FailedOperation.PhoneNumberParseFail", false},
		{"a bad-format number, envelope", "InvalidParameterValue.IncorrectPhoneNumber", true},
	}
	for _, tt := range refusals {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			body := `{"Response":{"SendStatusSet":[{"Code":"` + tt.code + `","Message":"refused"}],"RequestId":"REQ-REFUSED"}}`
			if tt.envelop {
				body = `{"Response":{"Error":{"Code":"` + tt.code + `","Message":"refused"},"RequestId":"REQ-REFUSED"}}`
			}
			rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(body)),
					Header:     make(http.Header),
				}, nil
			})

			s := fixedSender(t, rt, testConfig())
			err := s.Send(context.Background(), testSMS())
			if err == nil {
				t.Fatalf("Send() error = nil, want the %s refusal", tt.code)
			}
			if !errors.Is(err, pkgcore.ErrTransportPermanent) {
				t.Errorf("Send() error = %v, want errors.Is(err, pkgcore.ErrTransportPermanent) for %s", err, tt.code)
			}
			if !strings.Contains(err.Error(), tt.code) {
				t.Errorf("Send() error = %v, want the vendor code %s to stay reachable as the cause", err, tt.code)
			}
		})
	}
}

// TestSend_NonNumberRefusals_AreNotMarkedPermanent pins the other side of
// the classification: refusals that are the platform's, the operator's or
// the message's -- the per-number frequency caps included, which are
// temporary -- must travel unwrapped, so a caller acting on the sentinel as
// the number's own verdict never bounces a healthy number over one of them.
func TestSend_NonNumberRefusals_AreNotMarkedPermanent(t *testing.T) {
	t.Parallel()

	refusals := []struct {
		name string
		body string
	}{
		{"a per-number daily cap", `{"Response":{"SendStatusSet":[{"Code":"LimitExceeded.PhoneNumberDailyLimit","Message":"daily limit"}],"RequestId":"REQ-REFUSED"}}`},
		{"a template-parameter mismatch", `{"Response":{"SendStatusSet":[{"Code":"FailedOperation.TemplateParamSetNotMatchApprovedTemplate","Message":"template params do not match"}],"RequestId":"REQ-REFUSED"}}`},
		{"a signature failure", `{"Response":{"Error":{"Code":"AuthFailure.SignatureFailure","Message":"The provided credentials could not be validated"},"RequestId":"REQ-REFUSED"}}`},
	}
	for _, tt := range refusals {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(tt.body)),
					Header:     make(http.Header),
				}, nil
			})

			s := fixedSender(t, rt, testConfig())
			err := s.Send(context.Background(), testSMS())
			if err == nil {
				t.Fatalf("Send() error = nil, want the refusal")
			}
			if errors.Is(err, pkgcore.ErrTransportPermanent) {
				t.Errorf("Send() error = %v, want the refusal NOT marked with pkgcore.ErrTransportPermanent", err)
			}
		})
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

	s := fixedSender(t, rt, testConfig())
	err := s.Send(context.Background(), testSMS())
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

	s := fixedSender(t, rt, testConfig())
	if err := s.Send(context.Background(), testSMS()); err == nil {
		t.Fatalf("Send() error = nil, want an error for an envelope without a send status")
	}
}

// TestSend_TransportFailure_ReturnsError proves a dial/transport failure
// surfaces as an error and never as a silent success.
func TestSend_TransportFailure_ReturnsError(t *testing.T) {
	t.Parallel()

	s := fixedSender(t, func(_ *http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	}, testConfig())
	if err := s.Send(context.Background(), testSMS()); err == nil {
		t.Fatalf("Send() error = nil, want the transport failure")
	}
}
