package integration

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateWebhookURL_PrivateIPLiteral_Blocked(t *testing.T) {
	// The mandatory proof: a private-IP webhook URL is refused, not
	// silently accepted.
	cases := []string{
		"http://127.0.0.1:8080/hook",
		"http://[::1]:8080/hook",
		"http://10.0.0.5/hook",
		"http://172.16.0.5/hook",
		"http://192.168.1.5/hook",
		"http://169.254.169.254/latest/meta-data/", // cloud metadata endpoint
		"http://100.64.0.5/hook",                   // CGNAT
		"http://0.0.0.0/hook",
	}
	for _, u := range cases {
		t.Run(u, func(t *testing.T) {
			err := ValidateWebhookURL(context.Background(), u)
			if err == nil {
				t.Fatalf("ValidateWebhookURL(%q) = nil, want a refusal", u)
			}
			if !apperrIs(err, ErrWebhookURLBlocked) {
				t.Fatalf("ValidateWebhookURL(%q) error = %v, want ErrWebhookURLBlocked", u, err)
			}
		})
	}
}

func TestValidateWebhookURL_PublicIPLiteral_Allowed(t *testing.T) {
	if err := ValidateWebhookURL(context.Background(), "https://93.184.216.34/hook"); err != nil {
		t.Fatalf("ValidateWebhookURL(public IP) = %v, want nil", err)
	}
}

func TestValidateWebhookURL_DisallowedScheme_Refused(t *testing.T) {
	err := ValidateWebhookURL(context.Background(), "ftp://example.com/hook")
	if !apperrIs(err, ErrWebhookURLInvalid) {
		t.Fatalf("error = %v, want ErrWebhookURLInvalid", err)
	}
}

func TestValidateWebhookURL_Malformed_Refused(t *testing.T) {
	err := ValidateWebhookURL(context.Background(), "://not a url")
	if !apperrIs(err, ErrWebhookURLInvalid) {
		t.Fatalf("error = %v, want ErrWebhookURLInvalid", err)
	}
}

func TestValidateWebhookURL_NoHost_Refused(t *testing.T) {
	err := ValidateWebhookURL(context.Background(), "https:///hook")
	if !apperrIs(err, ErrWebhookURLInvalid) {
		t.Fatalf("error = %v, want ErrWebhookURLInvalid", err)
	}
}

func TestValidateWebhookURL_UnresolvableHost_Refused(t *testing.T) {
	err := ValidateWebhookURL(context.Background(), "https://this-host-should-not-exist.invalid/hook")
	if !apperrIs(err, ErrWebhookURLUnresolvable) {
		t.Fatalf("error = %v, want ErrWebhookURLUnresolvable", err)
	}
}

func TestIsBlockedIP(t *testing.T) {
	tests := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"10.1.2.3", true},
		{"172.31.255.255", true},
		{"192.168.0.1", true},
		{"169.254.1.1", true},
		{"fe80::1", true},
		{"224.0.0.1", true}, // multicast
		{"100.64.0.1", true},
		{"0.0.0.0", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"2001:4860:4860::8888", false},
	}
	for _, tt := range tests {
		ip := net.ParseIP(tt.ip)
		if ip == nil {
			t.Fatalf("net.ParseIP(%q) failed", tt.ip)
		}
		if got := isBlockedIP(ip); got != tt.blocked {
			t.Errorf("isBlockedIP(%q) = %v, want %v", tt.ip, got, tt.blocked)
		}
	}
}

// TestNewSafeHTTPClient_RefusesLoopbackAtDialTime proves the second half of
// this file's own SSRF defense: even a URL that somehow reached delivery
// time (bypassing ValidateWebhookURL, or one whose DNS answer changed after
// creation) is refused at the point of actually connecting, never sent.
func TestNewSafeHTTPClient_RefusesLoopbackAtDialTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := newSafeHTTPClient(webhookDeliveryTimeout)
	_, err := client.Get(srv.URL)
	if err == nil {
		t.Fatal("safe HTTP client dialed a loopback address, want a refusal")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("error = %v, want it to mention the dial was blocked", err)
	}
}

// TestNewSafeHTTPClient_AllowsPublicAddress proves the guard does not
// refuse everything -- only blocked ranges. It cannot reach the real
// network in a sandboxed test environment, so it only asserts that
// whatever the outcome (reachable or not), the failure -- if any -- is
// never the "blocked" refusal this test is not exercising.
func TestNewSafeHTTPClient_AllowsPublicAddress(t *testing.T) {
	client := newSafeHTTPClient(webhookDeliveryTimeout)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://93.184.216.34:1/hook", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	_, err = client.Do(req)
	if err == nil {
		return // unexpectedly reachable; still not a blocked-address refusal, so this passes.
	}
	if strings.Contains(err.Error(), "blocked") {
		t.Fatalf("a public address was refused as blocked: %v", err)
	}
}
