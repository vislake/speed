package twilio

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// roundTripFunc adapts a function to http.RoundTripper so a test (or an
// example) can script the transport without a listening server.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const (
	testAccountSID = "AC11111111111111111111111111111111"
	testAuthToken  = "twilio-test-auth-token"
	testFrom       = "+15005550000"
)

func testConfig() Config {
	return Config{
		AccountSID: testAccountSID,
		AuthToken:  testAuthToken,
		From:       testFrom,
	}
}

// newSenderForTest is NewSender over a scripted transport, for the tests
// that need the full construction path plus a captured request.
func newSenderForTest(t *testing.T, cfg Config, rt roundTripFunc) *sender {
	t.Helper()
	s, err := NewSender(cfg, WithClient(&http.Client{Transport: rt}))
	if err != nil {
		t.Fatalf("NewSender(%+v) error = %v", cfg, err)
	}
	return s.(*sender)
}

// TestNewSender_MissingOrConflictingConfig_Refuses proves construction fails
// closed: each missing field is named, the secret never echoes, and the
// From/MessagingServiceSID selection is exactly-one (Twilio refuses both
// together and neither alone).
func TestNewSender_MissingOrConflictingConfig_Refuses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		mutate     func(*Config)
		wantErrSub string
	}{
		{name: "account sid", mutate: func(c *Config) { c.AccountSID = "" }, wantErrSub: "AccountSID"},
		{name: "auth token", mutate: func(c *Config) { c.AuthToken = "" }, wantErrSub: "AuthToken"},
		{name: "neither sender", mutate: func(c *Config) { c.From = "" }, wantErrSub: "exactly one of From or MessagingServiceSID"},
		{name: "both senders", mutate: func(c *Config) { c.MessagingServiceSID = "MG11111111111111111111111111111111" }, wantErrSub: "mutually exclusive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig()
			tc.mutate(&cfg)
			_, err := NewSender(cfg)
			if err == nil {
				t.Fatalf("NewSender(%+v) error = nil, want a refusal", cfg)
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Errorf("NewSender error = %v, want it to mention %q", err, tc.wantErrSub)
			}
			if strings.Contains(err.Error(), testAuthToken) {
				t.Errorf("NewSender error = %v, must never echo the auth token", err)
			}
		})
	}
}

// TestSend_PostsAuthenticatedMessageToMessagesResource is the offline
// request-shape proof: the transport records exactly one POST to this
// account's Messages resource carrying Twilio's Basic auth header and the
// exact form body -- recipient, free text and configured sender -- with
// every value asserted verbatim (values precomputed independently). A wrong
// URL shape, a missing or mis-encoded credential header, or a body field
// that drifted all fail here.
func TestSend_PostsAuthenticatedMessageToMessagesResource(t *testing.T) {
	t.Parallel()

	// base64("AC1111...:twilio-test-auth-token"), independently computed.
	const wantAuthz = "Basic QUMxMTExMTExMTExMTExMTExMTExMTExMTExMTExMTExMTp0d2lsaW8tdGVzdC1hdXRoLXRva2Vu"
	const wantBody = "Body=your+code+is+654321&From=%2B15005550000&To=%2B8613800000000"

	var gotURL, gotMethod, gotAuthz, gotContentType, gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		gotMethod = r.Method
		gotAuthz = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		gotBody = string(raw)
		return &http.Response{
			StatusCode: http.StatusCreated,
			Body:       io.NopCloser(strings.NewReader(`{"sid":"SM123","status":"queued","to":"+8613800000000","from":"+15005550000"}`)),
			Header:     make(http.Header),
		}, nil
	})

	s := newSenderForTest(t, testConfig(), rt)
	if err := s.Send(context.Background(), pkgcore.SMS{To: "+8613800000000", Text: "your code is 654321"}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if wantURL := gatewayBaseURL + "/Accounts/" + testAccountSID + "/Messages.json"; gotURL != wantURL {
		t.Errorf("URL = %q, want the account's Messages resource %q", gotURL, wantURL)
	}
	if gotAuthz != wantAuthz {
		t.Errorf("Authorization = %q, want the independently computed %q", gotAuthz, wantAuthz)
	}
	if gotContentType != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", gotContentType)
	}
	if gotBody != wantBody {
		t.Errorf("body = %q, want the independently computed %q", gotBody, wantBody)
	}
}

// TestSend_MessagingServiceSid_IsUsedInsteadOfFrom proves the messaging
// service variant of the sender selection reaches the body, and From does
// not.
func TestSend_MessagingServiceSid_IsUsedInsteadOfFrom(t *testing.T) {
	t.Parallel()

	var gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		return &http.Response{
			StatusCode: http.StatusCreated,
			Body:       io.NopCloser(strings.NewReader(`{"sid":"SM123","status":"accepted"}`)),
			Header:     make(http.Header),
		}, nil
	})

	cfg := testConfig()
	cfg.From = ""
	cfg.MessagingServiceSID = "MG11111111111111111111111111111111"
	s := newSenderForTest(t, cfg, rt)
	if err := s.Send(context.Background(), pkgcore.SMS{To: "+8613800000000", Text: "x"}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if !strings.Contains(gotBody, "MessagingServiceSid=MG11111111111111111111111111111111") {
		t.Errorf("body = %q, want the configured MessagingServiceSid", gotBody)
	}
	if strings.Contains(gotBody, "From=") {
		t.Errorf("body = %q, want no From once a MessagingServiceSid is configured", gotBody)
	}
}

// TestSend_RefusedSend_SurfacesTwiliosOwnError proves a 4xx refusal returns
// an error carrying Twilio's own error vocabulary -- the message and code of
// the vendor's envelope -- never a bare status.
func TestSend_RefusedSend_SurfacesTwiliosOwnError(t *testing.T) {
	t.Parallel()

	rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Body:       io.NopCloser(strings.NewReader(`{"code":21211,"message":"The 'To' number is not a valid phone number.","more_info":"https://www.twilio.com/docs/errors/21211"}`)),
			Header:     make(http.Header),
		}, nil
	})

	s := newSenderForTest(t, testConfig(), rt)
	err := s.Send(context.Background(), pkgcore.SMS{To: "+8613800000000", Text: "x"})
	if err == nil {
		t.Fatalf("Send() error = nil, want Twilio's refusal")
	}
	if !strings.Contains(err.Error(), "21211") || !strings.Contains(err.Error(), "not a valid phone number") {
		t.Errorf("Send() error = %v, want Twilio's code and message", err)
	}
}

// TestSend_RefusedWithoutParsableBody_StillReturnsError proves a refusal
// whose body is not Twilio's error envelope still surfaces as an error with
// the status.
func TestSend_RefusedWithoutParsableBody_StillReturnsError(t *testing.T) {
	t.Parallel()

	rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Body:       io.NopCloser(strings.NewReader("gateway blew up")),
			Header:     make(http.Header),
		}, nil
	})

	s := newSenderForTest(t, testConfig(), rt)
	if err := s.Send(context.Background(), pkgcore.SMS{To: "+8613800000000", Text: "x"}); err == nil {
		t.Fatalf("Send() error = nil, want an error for the 502 gateway response")
	}
}

// TestSend_TransportFailure_ReturnsError proves a dial/transport failure
// surfaces as an error and never as a silent success.
func TestSend_TransportFailure_ReturnsError(t *testing.T) {
	t.Parallel()

	s := newSenderForTest(t, testConfig(), func(_ *http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})
	if err := s.Send(context.Background(), pkgcore.SMS{To: "+8613800000000", Text: "x"}); err == nil {
		t.Fatalf("Send() error = nil, want the transport failure")
	}
}
