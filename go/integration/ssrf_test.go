package integration

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore/apperr"
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

// TestValidateWebhookURL_ResolvedBlockedHost_RefusalDoesNotDiscloseResolvedIP
// pins the deliberate asymmetry between the two blocked-refusal paths: when
// the refused destination was reached through a DNS resolution (the caller
// submitted a HOSTNAME), the refusal must NOT carry the resolved address in
// its params. The resolved internal IP is information the caller does not
// have -- for a name only resolvable inside the platform's own network it is
// exactly the reconnaissance answer an internal-DNS oracle would give -- so
// echoing it back would turn the refusal into a scanning oracle: submit
// hostnames, read back the internal addresses they resolve to. The coded
// error plus the module's own generic webhook_url_blocked text (which
// deliberately names no address) is the honest answer shape.
//
// localhost resolves to a loopback address through the real resolver on any
// standard system (hosts file, no network), so this needs no resolver seam.
func TestValidateWebhookURL_ResolvedBlockedHost_RefusalDoesNotDiscloseResolvedIP(t *testing.T) {
	err := ValidateWebhookURL(context.Background(), "http://localhost:8080/hook")
	found, ok := apperr.As(err)
	if !ok || found.Code != ErrWebhookURLBlocked.Code {
		t.Fatalf("ValidateWebhookURL(localhost) = %v, want ErrWebhookURLBlocked", err)
	}
	if ip, present := found.Params["ip"]; present {
		t.Fatalf("the resolution path's refusal carries the resolved address %v in its params -- an internal-DNS oracle for the caller; want no ip param", ip)
	}
}

// TestValidateWebhookURL_BlockedLiteralIP_RefusalCarriesTheLiteralIP pins the
// mirror half of the same asymmetry: when the caller TYPED the blocked
// address into the URL, the refusal keeps carrying it in the ip param. That
// is an echo of what the caller already knows -- zero disclosure -- and
// genuinely useful diagnostics (which of the several addresses in the URL
// was refused), so this path must never lose the param.
func TestValidateWebhookURL_BlockedLiteralIP_RefusalCarriesTheLiteralIP(t *testing.T) {
	cases := []struct{ url, wantIP string }{
		{"http://127.0.0.1:8080/hook", "127.0.0.1"},
		{"http://[::1]:8080/hook", "::1"},
		{"http://10.0.0.5/hook", "10.0.0.5"},
		{"http://172.16.0.5/hook", "172.16.0.5"},
		{"http://192.168.1.5/hook", "192.168.1.5"},
		{"http://100.64.0.5/hook", "100.64.0.5"},
	}
	for _, tt := range cases {
		t.Run(tt.url, func(t *testing.T) {
			err := ValidateWebhookURL(context.Background(), tt.url)
			found, ok := apperr.As(err)
			if !ok || found.Code != ErrWebhookURLBlocked.Code {
				t.Fatalf("error = %v, want ErrWebhookURLBlocked", err)
			}
			if got := found.Params["ip"]; got != tt.wantIP {
				t.Fatalf("ip param = %v, want the literal address %q the caller typed", got, tt.wantIP)
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
	found, ok := apperr.As(err)
	if !ok || found.Code != ErrWebhookURLUnresolvable.Code {
		t.Fatalf("error = %v, want ErrWebhookURLUnresolvable", err)
	}
	// The host param echoes the caller's own hostname -- zero disclosure --
	// and stays on the answer unchanged.
	if got := found.Params["host"]; got != "this-host-should-not-exist.invalid" {
		t.Fatalf("host param = %v, want the caller's own hostname", got)
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
