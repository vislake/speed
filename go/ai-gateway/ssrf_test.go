package aigateway

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// The unit tests below mirror go/integration/ssrf_test.go's own suite case
// for case, per ssrf.go's file header: the two modules' SSRF defenses must
// keep agreeing, and a mirrored table is how a divergence would surface.

func TestValidateBaseURL_PrivateIPLiteral_Blocked(t *testing.T) {
	// The mandatory proof: a private-IP base URL is refused, not silently
	// accepted -- the creation-time half of the P0 fix.
	cases := []string{
		"http://127.0.0.1:9000/v1",
		"http://[::1]:9000/v1",
		"http://10.0.0.5/v1",
		"http://172.16.0.5/v1",
		"http://192.168.1.5/v1",
		"http://169.254.169.254/latest/meta-data/", // cloud metadata endpoint
		"http://100.64.0.5/v1",                     // CGNAT
		"http://0.0.0.0/v1",
		// IPv6 ranges the stdlib classification misses, refused only through
		// blockedIPv6CIDRs: NAT64 forms of the metadata endpoint and of
		// loopback, plus a site-local address.
		"http://[64:ff9b::169.254.169.254]/v1",
		"http://[64:ff9b::127.0.0.1]/v1",
		"http://[fec0::1]/v1",
	}
	for _, u := range cases {
		t.Run(u, func(t *testing.T) {
			err := ValidateBaseURL(context.Background(), u)
			if err == nil {
				t.Fatalf("ValidateBaseURL(%q) = nil, want a refusal", u)
			}
			found, ok := apperr.As(err)
			if !ok || found.Code != ErrBaseURLBlocked.Code {
				t.Fatalf("ValidateBaseURL(%q) error = %v, want ErrBaseURLBlocked", u, err)
			}
		})
	}
}

// TestValidateBaseURL_ResolvedBlockedHost_RefusalDoesNotDiscloseResolvedIP
// pins the deliberate asymmetry between the two blocked-refusal paths: when
// the refused destination was reached through a DNS resolution (the caller
// submitted a HOSTNAME), the refusal must NOT carry the resolved address in
// its params. The resolved internal IP is information the caller does not
// have -- for a name only resolvable inside the platform's own network it is
// exactly the reconnaissance answer an internal-DNS oracle would give -- so
// echoing it back would turn the refusal into a scanning oracle: submit
// hostnames, read back the internal addresses they resolve to. localhost
// resolves to a loopback address through the real resolver on any standard
// system (hosts file, no network), so this needs no resolver seam.
func TestValidateBaseURL_ResolvedBlockedHost_RefusalDoesNotDiscloseResolvedIP(t *testing.T) {
	err := ValidateBaseURL(context.Background(), "http://localhost:9000/v1")
	found, ok := apperr.As(err)
	if !ok || found.Code != ErrBaseURLBlocked.Code {
		t.Fatalf("ValidateBaseURL(localhost) = %v, want ErrBaseURLBlocked", err)
	}
	if ip, present := found.Params["ip"]; present {
		t.Fatalf("the resolution path's refusal carries the resolved address %v in its params -- an internal-DNS oracle for the caller; want no ip param", ip)
	}
}

// TestValidateBaseURL_BlockedLiteralIP_RefusalCarriesTheLiteralIP pins the
// mirror half of the same asymmetry: when the caller TYPED the blocked
// address into the URL, the refusal keeps carrying it in the ip param --
// an echo of what the caller already knows, zero disclosure, and genuinely
// useful diagnostics (which of the several addresses in the URL was
// refused).
func TestValidateBaseURL_BlockedLiteralIP_RefusalCarriesTheLiteralIP(t *testing.T) {
	cases := []struct{ url, wantIP string }{
		{"http://127.0.0.1:9000/v1", "127.0.0.1"},
		{"http://[::1]:9000/v1", "::1"},
		{"http://10.0.0.5/v1", "10.0.0.5"},
		{"http://172.16.0.5/v1", "172.16.0.5"},
		{"http://192.168.1.5/v1", "192.168.1.5"},
		{"http://100.64.0.5/v1", "100.64.0.5"},
	}
	for _, tt := range cases {
		t.Run(tt.url, func(t *testing.T) {
			err := ValidateBaseURL(context.Background(), tt.url)
			found, ok := apperr.As(err)
			if !ok || found.Code != ErrBaseURLBlocked.Code {
				t.Fatalf("error = %v, want ErrBaseURLBlocked", err)
			}
			if got := found.Params["ip"]; got != tt.wantIP {
				t.Fatalf("ip param = %v, want the literal address %q the caller typed", got, tt.wantIP)
			}
		})
	}
}

// TestValidateBaseURL_PublicIPLiteral_Allowed pins the product decision the
// fix preserves: a tenant BYOK base URL naming a public OpenAI-compatible
// endpoint is accepted. A literal public IP needs no DNS resolution, so
// the case is deterministic offline.
func TestValidateBaseURL_PublicIPLiteral_Allowed(t *testing.T) {
	if err := ValidateBaseURL(context.Background(), "https://93.184.216.34/v1"); err != nil {
		t.Fatalf("ValidateBaseURL(public IP) = %v, want nil", err)
	}
}

func TestValidateBaseURL_DisallowedScheme_Refused(t *testing.T) {
	err := ValidateBaseURL(context.Background(), "ftp://example.com/v1")
	found, ok := apperr.As(err)
	if !ok || found.Code != ErrBaseURLInvalid.Code {
		t.Fatalf("error = %v, want ErrBaseURLInvalid", err)
	}
	if got := found.Params["reason"]; got != "scheme" {
		t.Fatalf("reason param = %v, want %q", got, "scheme")
	}
}

func TestValidateBaseURL_Malformed_Refused(t *testing.T) {
	err := ValidateBaseURL(context.Background(), "://not a url")
	found, ok := apperr.As(err)
	if !ok || found.Code != ErrBaseURLInvalid.Code {
		t.Fatalf("error = %v, want ErrBaseURLInvalid", err)
	}
}

func TestValidateBaseURL_NoHost_Refused(t *testing.T) {
	err := ValidateBaseURL(context.Background(), "https:///v1")
	found, ok := apperr.As(err)
	if !ok || found.Code != ErrBaseURLInvalid.Code {
		t.Fatalf("error = %v, want ErrBaseURLInvalid", err)
	}
}

func TestValidateBaseURL_UnresolvableHost_Refused(t *testing.T) {
	err := ValidateBaseURL(context.Background(), "https://this-host-should-not-exist.invalid/v1")
	found, ok := apperr.As(err)
	if !ok || found.Code != ErrBaseURLUnresolvable.Code {
		t.Fatalf("error = %v, want ErrBaseURLUnresolvable", err)
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
		// Covered forms that must stay refused, whatever the supplementary
		// list mechanism grows into: v4-mapped addresses are refused through
		// the embedded IPv4 net.IP.To4 exposes (the loopback and link-local
		// tests both reach into it), 169.254.169.254 through
		// IsLinkLocalUnicast directly, and the CGNAT range through
		// blockedIPv4CIDRs -- none of them belongs in an IPv6 list.
		{"::ffff:127.0.0.1", true},
		{"::ffff:169.254.169.254", true},
		{"169.254.169.254", true},
		{"100.64.255.255", true},
		// IPv6 ranges net.IP's own classification leaves unclassified (they
		// read as ordinary global unicast) that isBlockedIP must refuse
		// through blockedIPv6CIDRs: NAT64 (RFC 6052), IPv4-compatible
		// (RFC 4291) and site-local (RFC 3879). The NAT64 rows use the
		// dotted-quad form a DNS answer would actually carry.
		{"64:ff9b::1", true},
		{"64:ff9b::169.254.169.254", true},
		{"64:ff9b::127.0.0.1", true},
		{"::127.0.0.1", true},
		{"fec0::1", true},
		{"feff::1", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"2001:4860:4860::8888", false},
		{"2606:4700:4700::1111", false},
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

// TestGuardedProviderHTTPClient_RefusesLoopbackAtDialTime proves the
// dial-time half of this file's own SSRF defense: even a base URL that
// somehow reached call time is refused at the point of actually
// connecting, never dialed -- a tenant-tier credential's provider always
// carries this client (the resolve-level tests below pin the wiring), so a
// DNS answer that changed after the credential's validated write cannot
// redirect a call into an internal address.
func TestGuardedProviderHTTPClient_RefusesLoopbackAtDialTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, err := guardedProviderHTTPClient.Get(srv.URL)
	if err == nil {
		t.Fatal("guarded HTTP client dialed a loopback address, want a refusal")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("error = %v, want it to mention the dial was blocked", err)
	}
}

// TestGuardedProviderHTTPClient_AllowsPublicAddress proves the guard does
// not refuse everything -- only blocked ranges. It cannot reach the real
// network in a sandboxed test environment, so it only asserts that
// whatever the outcome (reachable or not), the failure -- if any -- is
// never the "blocked" refusal this test is not exercising.
func TestGuardedProviderHTTPClient_AllowsPublicAddress(t *testing.T) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://93.184.216.34:1/v1", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	_, err = guardedProviderHTTPClient.Do(req)
	if err == nil {
		return // unexpectedly reachable; still not a blocked-address refusal, so this passes.
	}
	if strings.Contains(err.Error(), "blocked") {
		t.Fatalf("a public address was refused as blocked: %v", err)
	}
}

// TestGateway_Resolve_TenantScopeCredential_GetsGuardedHTTPClient proves
// the scope boundary is enforced on the dial path: a provider built for a
// credential that resolved at the TENANT tier carries
// guardedProviderHTTPClient, while one built for the platform-tier
// fallback keeps the provider's own ordinary client. resolve is the exact
// function Gateway.Chat/ChatStream run per call.
func TestGateway_Resolve_TenantScopeCredential_GetsGuardedHTTPClient(t *testing.T) {
	credentials := NewCredentialService(newTestDB(t))
	sysCtx, err := pkgcore.WithSystemContext(context.Background(), systemTestCtx(t))
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}
	if err = credentials.SetPlatformCredential(sysCtx, ProviderOpenAICompatible, "sk-platform", "https://93.184.216.34/v1"); err != nil {
		t.Fatalf("SetPlatformCredential: %v", err)
	}
	tenantCtx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if err = credentials.SetTenantCredential(tenantCtx, ProviderOpenAICompatible, "sk-tenant", "https://93.184.216.34/v1"); err != nil {
		t.Fatalf("SetTenantCredential: %v", err)
	}
	g := NewGateway(credentials,
		WithModelRoute("chat:default", ProviderOpenAICompatible, "gpt-4o-mini"),
	)

	provider, route, err := g.resolve(tenantCtx, "chat:default")
	if err != nil {
		t.Fatalf("resolve under the tenant: %v", err)
	}
	if route.Provider != ProviderOpenAICompatible {
		t.Fatalf("resolve returned provider %q, want %q", route.Provider, ProviderOpenAICompatible)
	}
	if got := provider.(*OpenAICompatibleProvider).httpClient; got != guardedProviderHTTPClient {
		t.Fatal("tenant-tier resolve left the provider on an unguarded HTTP client, want guardedProviderHTTPClient -- the tenant's own base URL must dial through the SSRF guard")
	}

	// A tenantless context resolves the platform-tier fallback, which stays
	// on the provider's own ordinary client -- the operator-chosen default
	// is outside the guard's scope boundary (ssrf.go's file header).
	provider, _, err = g.resolve(context.Background(), "chat:default")
	if err != nil {
		t.Fatalf("resolve without a tenant: %v", err)
	}
	if got := provider.(*OpenAICompatibleProvider).httpClient; got == guardedProviderHTTPClient {
		t.Fatal("platform-tier resolve was given the guarded HTTP client, want the provider's own client -- the platform default is operator-chosen")
	}
}

// TestGateway_ResolveImage_TenantScopeCredential_GetsGuardedHTTPClient is
// the image-side twin of the resolve test above: the job handler's
// resolveImage (callProvider re-resolves fresh at execution time) must
// apply the identical tenant-tier guard, so an image job executes against
// the tenant's own base URL through the guarded client wherever the worker
// runs.
func TestGateway_ResolveImage_TenantScopeCredential_GetsGuardedHTTPClient(t *testing.T) {
	credentials := NewCredentialService(newTestDB(t))
	sysCtx, err := pkgcore.WithSystemContext(context.Background(), systemTestCtx(t))
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}
	if err = credentials.SetPlatformCredential(sysCtx, ProviderOpenAICompatibleImage, "sk-platform", "https://93.184.216.34/v1"); err != nil {
		t.Fatalf("SetPlatformCredential: %v", err)
	}
	tenantCtx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if err = credentials.SetTenantCredential(tenantCtx, ProviderOpenAICompatibleImage, "sk-tenant", "https://93.184.216.34/v1"); err != nil {
		t.Fatalf("SetTenantCredential: %v", err)
	}
	queue := &recordingImageQueue{jobID: "job-unused"}
	g := NewGateway(credentials,
		WithModelRoute("image:default", ProviderOpenAICompatibleImage, "dall-e-3"),
		WithImageGeneration(queue, newTestStorageObjectService(t)),
	)

	provider, _, err := g.resolveImage(tenantCtx, "image:default")
	if err != nil {
		t.Fatalf("resolveImage under the tenant: %v", err)
	}
	if got := provider.(*OpenAICompatibleImageProvider).httpClient; got != guardedProviderHTTPClient {
		t.Fatal("tenant-tier resolveImage left the provider on an unguarded HTTP client, want guardedProviderHTTPClient")
	}

	provider, _, err = g.resolveImage(context.Background(), "image:default")
	if err != nil {
		t.Fatalf("resolveImage without a tenant: %v", err)
	}
	if got := provider.(*OpenAICompatibleImageProvider).httpClient; got == guardedProviderHTTPClient {
		t.Fatal("platform-tier resolveImage was given the guarded HTTP client, want the provider's own client")
	}
}
