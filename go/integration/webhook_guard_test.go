package integration

import (
	"context"
	"errors"
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
		// IPv6 special-purpose ranges the stdlib classification misses,
		// refused by the shared guard's blocked set: NAT64 forms of the
		// metadata endpoint and of loopback, plus a site-local address.
		"http://[64:ff9b::169.254.169.254]/hook",
		"http://[64:ff9b::127.0.0.1]/hook",
		"http://[fec0::1]/hook",
	}
	for _, u := range cases {
		t.Run(u, func(t *testing.T) {
			err := ValidateWebhookURL(context.Background(), u)
			if err == nil {
				t.Fatalf("ValidateWebhookURL(%q) = nil, want a refusal", u)
			}
			if !apperr.HasCode(err, ErrWebhookURLBlocked.Code) {
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
	if !apperr.HasCode(err, ErrWebhookURLBlocked.Code) {
		t.Fatalf("ValidateWebhookURL(localhost) = %v, want ErrWebhookURLBlocked", err)
	}
	found, _ := apperr.As(err)
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
			if !apperr.HasCode(err, ErrWebhookURLBlocked.Code) {
				t.Fatalf("error = %v, want ErrWebhookURLBlocked", err)
			}
			found, _ := apperr.As(err)
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
	if !apperr.HasCode(err, ErrWebhookURLInvalid.Code) {
		t.Fatalf("error = %v, want ErrWebhookURLInvalid", err)
	}
}

func TestValidateWebhookURL_Malformed_Refused(t *testing.T) {
	err := ValidateWebhookURL(context.Background(), "://not a url")
	if !apperr.HasCode(err, ErrWebhookURLInvalid.Code) {
		t.Fatalf("error = %v, want ErrWebhookURLInvalid", err)
	}
}

func TestValidateWebhookURL_NoHost_Refused(t *testing.T) {
	err := ValidateWebhookURL(context.Background(), "https:///hook")
	if !apperr.HasCode(err, ErrWebhookURLInvalid.Code) {
		t.Fatalf("error = %v, want ErrWebhookURLInvalid", err)
	}
}

func TestValidateWebhookURL_UnresolvableHost_Refused(t *testing.T) {
	err := ValidateWebhookURL(context.Background(), "https://this-host-should-not-exist.invalid/hook")
	if !apperr.HasCode(err, ErrWebhookURLUnresolvable.Code) {
		t.Fatalf("error = %v, want ErrWebhookURLUnresolvable", err)
	}
	found, _ := apperr.As(err)
	// The host param echoes the caller's own hostname -- zero disclosure --
	// and stays on the answer unchanged.
	if got := found.Params["host"]; got != "this-host-should-not-exist.invalid" {
		t.Fatalf("host param = %v, want the caller's own hostname", got)
	}
}

// TestNewSafeHTTPClient_RefusesLoopbackAtDialTime proves the CHECK half of
// the dial-time defense: a dial whose address is a blocked range is refused
// at the point of actually connecting, never sent. The dial address here is
// the httptest server's literal loopback IP, so the test exercises the
// literal-IP branch with no DNS at all. The rebinding property itself --
// that the address checked is the very address about to be dialled, so no
// second resolution can be raced -- belongs to the shared guard and is
// pinned by pkgcore/safehttp's
// TestGuard_DNSRebindingCannotGetPastTheConnectTimeCheck.
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
	if !errors.Is(err, errBlockedDialAddress) {
		t.Fatalf("error = %v, want it to wrap errBlockedDialAddress", err)
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("error = %v, want it to mention the dial was blocked", err)
	}
}

// TestNewSafeHTTPClient_BlockedHostname_RefusalNamesNoResolvedAddress pins
// the non-disclosure shaping newSafeHTTPClient applies to its dial refusal:
// when the refusal came from a HOSTNAME's DNS answer, the error must stay
// identifiable as the blocked-destination refusal and carry no token that
// parses as an IP address -- this text is persisted into the delivery row's
// last_error and served back to the tenant through the delivery-log API
// (webhook_delivery_test.go's
// TestHandler_IntegrationListWebhookDeliveries_BlockedDial_LastErrorNamesNoResolvedIP
// pins the persisted half), and echoing the resolved address there would
// make the refusal an internal-DNS reconnaissance oracle. localhost
// resolves to a loopback address through the real resolver on any standard
// system (hosts file, no network), so this needs no resolver seam; the port
// is never dialed, because every resolved candidate is refused before any
// connection is made.
func TestNewSafeHTTPClient_BlockedHostname_RefusalNamesNoResolvedAddress(t *testing.T) {
	client := newSafeHTTPClient(webhookDeliveryTimeout)
	_, err := client.Get("http://localhost:1/hook")
	if err == nil {
		t.Fatal("safe HTTP client dialed a hostname resolving to loopback, want a refusal")
	}
	if !errors.Is(err, errBlockedDialAddress) || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("error = %v, want the blocked-destination refusal", err)
	}
	for _, tok := range strings.Fields(err.Error()) {
		if net.ParseIP(tok) != nil {
			t.Fatalf("the refusal names the resolved address %q: %v -- an internal-DNS reconnaissance oracle for the tenant; the detail belongs in the server-side log", tok, err)
		}
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
