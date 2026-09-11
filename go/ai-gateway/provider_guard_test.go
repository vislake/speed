package aigateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/safehttp"
)

// The unit tests below pin this module's caller-side layer over the shared
// go/pkgcore/safehttp guard: the coded error vocabulary each refusal maps
// to (case for case with go/integration's webhook_guard_test.go, since
// ErrBaseURLBlocked's own comment demands the two modules' refusals keep
// agreeing), and the two properties this layer adds or preserves on top of
// the guard -- the no-IP-echo asymmetry of the write-time answer, and the
// guarded client's no-overall-timeout posture. The per-address predicate
// table these tests used to carry now lives with the guard itself
// (safehttp's TestCheckAddr_RefusesEverythingNotPubliclyRoutable).

func TestValidateBaseURL_PrivateIPLiteral_Blocked(t *testing.T) {
	// The creation-time half of the defense: a private-IP base URL is
	// refused, not silently accepted.
	cases := []string{
		"http://127.0.0.1:9000/v1",
		"http://[::1]:9000/v1",
		"http://10.0.0.5/v1",
		"http://172.16.0.5/v1",
		"http://192.168.1.5/v1",
		"http://169.254.169.254/latest/meta-data/", // cloud metadata endpoint
		"http://100.64.0.5/v1",                     // CGNAT
		"http://0.0.0.0/v1",
		// IPv6 special-purpose ranges the stdlib classification misses,
		// refused by the shared guard's blocked set: NAT64 forms of the
		// metadata endpoint and of loopback, plus a site-local address.
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
			if !apperr.HasCode(err, ErrBaseURLBlocked.Code) {
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
	if !apperr.HasCode(err, ErrBaseURLBlocked.Code) {
		t.Fatalf("ValidateBaseURL(localhost) = %v, want ErrBaseURLBlocked", err)
	}
	found, _ := apperr.As(err)
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
			if !apperr.HasCode(err, ErrBaseURLBlocked.Code) {
				t.Fatalf("error = %v, want ErrBaseURLBlocked", err)
			}
			found, _ := apperr.As(err)
			if got := found.Params["ip"]; got != tt.wantIP {
				t.Fatalf("ip param = %v, want the literal address %q the caller typed", got, tt.wantIP)
			}
		})
	}
}

// TestValidateBaseURL_PublicIPLiteral_Allowed pins the product boundary
// the SSRF guard preserves: a tenant BYOK base URL naming a public
// OpenAI-compatible endpoint is accepted. A literal public IP needs no DNS
// resolution, so the case is deterministic offline.
func TestValidateBaseURL_PublicIPLiteral_Allowed(t *testing.T) {
	if err := ValidateBaseURL(context.Background(), "https://93.184.216.34/v1"); err != nil {
		t.Fatalf("ValidateBaseURL(public IP) = %v, want nil", err)
	}
}

func TestValidateBaseURL_DisallowedScheme_Refused(t *testing.T) {
	err := ValidateBaseURL(context.Background(), "ftp://example.com/v1")
	if !apperr.HasCode(err, ErrBaseURLInvalid.Code) {
		t.Fatalf("error = %v, want ErrBaseURLInvalid", err)
	}
	found, _ := apperr.As(err)
	if got := found.Params["reason"]; got != "scheme" {
		t.Fatalf("reason param = %v, want %q", got, "scheme")
	}
}

func TestValidateBaseURL_Malformed_Refused(t *testing.T) {
	err := ValidateBaseURL(context.Background(), "://not a url")
	if !apperr.HasCode(err, ErrBaseURLInvalid.Code) {
		t.Fatalf("error = %v, want ErrBaseURLInvalid", err)
	}
}

func TestValidateBaseURL_NoHost_Refused(t *testing.T) {
	err := ValidateBaseURL(context.Background(), "https:///v1")
	if !apperr.HasCode(err, ErrBaseURLInvalid.Code) {
		t.Fatalf("error = %v, want ErrBaseURLInvalid", err)
	}
}

func TestValidateBaseURL_UnresolvableHost_Refused(t *testing.T) {
	err := ValidateBaseURL(context.Background(), "https://this-host-should-not-exist.invalid/v1")
	if !apperr.HasCode(err, ErrBaseURLUnresolvable.Code) {
		t.Fatalf("error = %v, want ErrBaseURLUnresolvable", err)
	}
	found, _ := apperr.As(err)
	// The host param echoes the caller's own hostname -- zero disclosure --
	// and stays on the answer unchanged.
	if got := found.Params["host"]; got != "this-host-should-not-exist.invalid" {
		t.Fatalf("host param = %v, want the caller's own hostname", got)
	}
}

// TestGuardedProviderHTTPClient_RefusesLoopbackAtDialTime proves the CHECK
// half of the dial-time defense: a dial whose address is a blocked range is
// refused at the point of actually connecting, never dialed. The dial
// address here is the httptest server's literal loopback IP, so the test
// exercises the literal-IP branch with no DNS at all, and the refusal is
// the guard's own sentinel -- errors.Is(err, safehttp.ErrBlockedAddress) --
// which a caller can match through the provider layer's wrapping. The
// rebinding property itself (the address checked is the very address about
// to be dialled, never a re-resolution) belongs to the shared guard and is
// pinned by safehttp's TestGuard_DNSRebindingCannotGetPastTheConnectTimeCheck.
func TestGuardedProviderHTTPClient_RefusesLoopbackAtDialTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, err := guardedProviderHTTPClient.Get(srv.URL)
	if err == nil {
		t.Fatal("guarded HTTP client dialed a loopback address, want a refusal")
	}
	if !errors.Is(err, safehttp.ErrBlockedAddress) {
		t.Fatalf("error = %v, want it to wrap safehttp.ErrBlockedAddress", err)
	}
}

// TestGuardedProviderHTTPClient_AllowsPublicAddress proves the guard does
// not refuse everything -- only blocked ranges. It cannot reach the real
// network in a sandboxed test environment, so it only asserts that whatever
// the outcome (reachable or not), the failure -- if any -- is never the
// blocked-address refusal this test is not exercising.
func TestGuardedProviderHTTPClient_AllowsPublicAddress(t *testing.T) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://93.184.216.34:1/v1", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	_, err = guardedProviderHTTPClient.Do(req)
	if err == nil {
		return // unexpectedly reachable; still not a blocked-address refusal, so this passes.
	}
	if errors.Is(err, safehttp.ErrBlockedAddress) {
		t.Fatalf("a public address was refused as blocked: %v", err)
	}
}

// TestGuardedProviderHTTPClient_CarriesNoOverallTimeout pins the streaming
// posture guardedProviderHTTPClient must keep: no Timeout of its own, since
// a chat stream can legitimately outlast any fixed bound and the call paths
// (Chat, ChatStream, GenerateImage) bound their own requests through the
// request context instead. A regression that added an overall timeout here
// would silently cut off long streams.
func TestGuardedProviderHTTPClient_CarriesNoOverallTimeout(t *testing.T) {
	if got := guardedProviderHTTPClient.Timeout; got != 0 {
		t.Errorf("guardedProviderHTTPClient.Timeout = %v, want 0 (no overall bound; calls bound themselves through ctx)", got)
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
	// is outside the guard's scope boundary (provider_guard.go's file
	// header).
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

// TestGateway_Resolve_TenantScopeCredential_UnguardableProvider_Refused pins
// the fail-closed rule for the tenant-tier dial guard: a provider that
// cannot carry the guarded client (it does not implement httpClientSettable
// -- a third-party registration, unlike this module's two OpenAI-compatible
// built-ins) is REFUSED at resolve time when the credential that built it
// resolved at the TENANT tier, never silently allowed to dial unguarded.
// An unguardable provider has no rebinding-defeating dial-time re-check at
// all, so allowing the combination would leave the tenant-influenced dial
// at exactly the write-time-validation-only state the dial-time guard
// exists to close. The platform tier stays allowed on the provider's own
// client --
// the operator's intranet-gateway default, outside the guard by scope
// boundary.
func TestGateway_Resolve_TenantScopeCredential_UnguardableProvider_Refused(t *testing.T) {
	provider := &fakeChatProvider{}
	credentials := NewCredentialService(newTestDB(t))
	sysCtx, err := pkgcore.WithSystemContext(context.Background(), systemTestCtx(t))
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}
	if err = credentials.SetPlatformCredential(sysCtx, fakeProviderName, "sk-platform", ""); err != nil {
		t.Fatalf("SetPlatformCredential: %v", err)
	}
	tenantCtx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if err = credentials.SetTenantCredential(tenantCtx, fakeProviderName, "sk-tenant", "https://93.184.216.34/v1"); err != nil {
		t.Fatalf("SetTenantCredential: %v", err)
	}
	g := NewGateway(credentials,
		WithModelRoute("chat:default", fakeProviderName, "vendor-model-x"),
		WithChatProviderRegistry(newFakeGatewayRegistry(t, provider)),
	)

	// fakeChatProvider deliberately implements no setHTTPClient, so the
	// tenant-tier combination must be refused with the coded error naming
	// the provider, never silently resolved onto an unguarded client.
	_, _, err = g.resolve(tenantCtx, "chat:default")
	if err == nil {
		t.Fatal("resolve accepted a tenant-scope credential for an unguardable provider, want the coded ErrProviderNotSSRFGuardable refusal")
	}
	if got, ok := apperrCode(err); !ok || got != ErrProviderNotSSRFGuardable.Code {
		t.Fatalf("resolve error = %v, want the coded ErrProviderNotSSRFGuardable", err)
	}
	if appErr, ok := apperr.As(err); ok {
		if got := appErr.Params["provider"]; got != fakeProviderName {
			t.Fatalf("provider param = %v, want %q -- the refusal must name the unguardable registration", got, fakeProviderName)
		}
	}

	// The same unguardable provider resolving at the platform tier (a
	// tenantless context, the operator's own default) is still allowed.
	if _, _, err = g.resolve(context.Background(), "chat:default"); err != nil {
		t.Fatalf("resolve of the platform-tier fallback was refused: %v", err)
	}
}

// TestGateway_ResolveImage_TenantScopeCredential_UnguardableProvider_Refused
// is the image-side twin of the chat resolve refusal above: the job
// handler's resolveImage applies the identical tenant-tier guard, so an
// image provider that cannot carry the guarded client must be refused for
// a tenant BYOK credential wherever the job executes.
func TestGateway_ResolveImage_TenantScopeCredential_UnguardableProvider_Refused(t *testing.T) {
	provider := &fakeImageProvider{}
	credentials := NewCredentialService(newTestDB(t))
	sysCtx, err := pkgcore.WithSystemContext(context.Background(), systemTestCtx(t))
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}
	if err = credentials.SetPlatformCredential(sysCtx, fakeImageProviderName, "sk-platform", ""); err != nil {
		t.Fatalf("SetPlatformCredential: %v", err)
	}
	tenantCtx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if err = credentials.SetTenantCredential(tenantCtx, fakeImageProviderName, "sk-tenant", "https://93.184.216.34/v1"); err != nil {
		t.Fatalf("SetTenantCredential: %v", err)
	}
	g := NewGateway(credentials,
		WithModelRoute("image:default", fakeImageProviderName, "dall-e-3"),
		WithImageProviderRegistry(newFakeImageRegistry(t, provider)),
	)

	_, _, err = g.resolveImage(tenantCtx, "image:default")
	if err == nil {
		t.Fatal("resolveImage accepted a tenant-scope credential for an unguardable image provider, want the coded ErrProviderNotSSRFGuardable refusal")
	}
	if got, ok := apperrCode(err); !ok || got != ErrProviderNotSSRFGuardable.Code {
		t.Fatalf("resolveImage error = %v, want the coded ErrProviderNotSSRFGuardable", err)
	}

	// The platform-tier fallback stays allowed, as on the chat side.
	if _, _, err = g.resolveImage(context.Background(), "image:default"); err != nil {
		t.Fatalf("resolveImage of the platform-tier fallback was refused: %v", err)
	}
}
