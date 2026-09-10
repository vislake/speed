package aigateway

import (
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// apperrCode returns err's *apperr.Error Code, for comparing a returned
// error against one of this package's sentinels by code rather than by
// identity -- a decorated error (WithParam/WithCause) is never == to the
// sentinel it was derived from.
func apperrCode(err error) (string, bool) {
	appErr, ok := apperr.As(err)
	if !ok {
		return "", false
	}
	return appErr.Code, true
}

func systemTestCtx(t *testing.T) pkgcore.SystemReason {
	t.Helper()
	pkgcore.RegisterSystemPurpose(SystemPurposeCredentialWrite)
	return pkgcore.SystemReason{Actor: "test-actor", Purpose: SystemPurposeCredentialWrite}
}

func TestCredentialService_SetPlatformCredential_RequiresSystemContext(t *testing.T) {
	svc := NewCredentialService(newTestDB(t))
	err := svc.SetPlatformCredential(t.Context(), ProviderOpenAICompatible, "sk-test", "")
	if got, ok := apperrCode(err); !ok || got != ErrSystemScopeRequiresSystemContext.Code {
		t.Fatalf("SetPlatformCredential with no system context = %v, want ErrSystemScopeRequiresSystemContext", err)
	}
}

func TestCredentialService_SetTenantCredential_RequiresTenant(t *testing.T) {
	svc := NewCredentialService(newTestDB(t))
	err := svc.SetTenantCredential(t.Context(), ProviderOpenAICompatible, "sk-test", "")
	if got, ok := apperrCode(err); !ok || got != ErrTenantScopeRequiresTenant.Code {
		t.Fatalf("SetTenantCredential with no tenant = %v, want ErrTenantScopeRequiresTenant", err)
	}
}

func TestCredentialService_Set_RequiresProviderAndAPIKey(t *testing.T) {
	svc := NewCredentialService(newTestDB(t))
	sysCtx, err := pkgcore.WithSystemContext(t.Context(), systemTestCtx(t))
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}
	tests := []struct {
		name     string
		provider string
		apiKey   string
	}{
		{"empty provider", "", "sk-test"},
		{"empty api key", ProviderOpenAICompatible, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.SetPlatformCredential(sysCtx, tc.provider, tc.apiKey, "")
			if got, ok := apperrCode(err); !ok || got != ErrCredentialRequired.Code {
				t.Fatalf("SetPlatformCredential(%q, %q) = %v, want ErrCredentialRequired", tc.provider, tc.apiKey, err)
			}
		})
	}
}

func TestCredentialService_Resolve_NoRowAnywhere_ReturnsNotFound(t *testing.T) {
	svc := NewCredentialService(newTestDB(t))
	_, err := svc.Resolve(pkgcore.WithTenant(t.Context(), "tenant-acme"), ProviderOpenAICompatible)
	if got, ok := apperrCode(err); !ok || got != ErrCredentialNotFound.Code {
		t.Fatalf("Resolve with no row = %v, want ErrCredentialNotFound", err)
	}
}

func TestCredentialService_Resolve_FallsBackToPlatformRow(t *testing.T) {
	svc := NewCredentialService(newTestDB(t))
	sysCtx, err := pkgcore.WithSystemContext(t.Context(), systemTestCtx(t))
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}
	if err = svc.SetPlatformCredential(sysCtx, ProviderOpenAICompatible, "sk-platform", "https://platform.example/v1"); err != nil {
		t.Fatalf("SetPlatformCredential: %v", err)
	}

	// A tenant with no BYOK row of its own resolves the platform row.
	cred, err := svc.Resolve(pkgcore.WithTenant(t.Context(), "tenant-acme"), ProviderOpenAICompatible)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.Scope != CredentialScopeSystem || cred.APIKey != "sk-platform" || cred.BaseURL != "https://platform.example/v1" {
		t.Fatalf("Resolve = %+v, want the platform row", cred)
	}

	// A context with no tenant at all resolves the platform row too.
	cred, err = svc.Resolve(t.Context(), ProviderOpenAICompatible)
	if err != nil {
		t.Fatalf("Resolve with no tenant: %v", err)
	}
	if cred.Scope != CredentialScopeSystem {
		t.Fatalf("Resolve with no tenant = %+v, want the platform row", cred)
	}
}

func TestCredentialService_Resolve_TenantBYOKOverridesPlatformRow(t *testing.T) {
	svc := NewCredentialService(newTestDB(t))
	sysCtx, err := pkgcore.WithSystemContext(t.Context(), systemTestCtx(t))
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}
	if err = svc.SetPlatformCredential(sysCtx, ProviderOpenAICompatible, "sk-platform", "https://platform.example/v1"); err != nil {
		t.Fatalf("SetPlatformCredential: %v", err)
	}

	acmeCtx := pkgcore.WithTenant(t.Context(), "tenant-acme")
	// A literal public IP: tenant-tier base URLs are SSRF-validated at write
	// time (credential.go), and a hostname this test cannot resolve would be
	// refused as unresolvable -- the IP is what this test's
	// resolution-override point needs, and it is never dialed here.
	if err = svc.SetTenantCredential(acmeCtx, ProviderOpenAICompatible, "sk-acme-byok", "https://93.184.216.34/v1"); err != nil {
		t.Fatalf("SetTenantCredential: %v", err)
	}

	// tenant-acme now resolves its own BYOK row.
	cred, err := svc.Resolve(acmeCtx, ProviderOpenAICompatible)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.Scope != CredentialScopeTenant || cred.APIKey != "sk-acme-byok" {
		t.Fatalf("Resolve for tenant-acme = %+v, want its own BYOK row", cred)
	}

	// A different tenant with no BYOK row of its own still falls back to
	// the platform row -- proving BYOK is genuinely per-tenant.
	globexCtx := pkgcore.WithTenant(t.Context(), "tenant-globex")
	cred, err = svc.Resolve(globexCtx, ProviderOpenAICompatible)
	if err != nil {
		t.Fatalf("Resolve for tenant-globex: %v", err)
	}
	if cred.Scope != CredentialScopeSystem || cred.APIKey != "sk-platform" {
		t.Fatalf("Resolve for tenant-globex = %+v, want the platform row", cred)
	}
}

func TestCredentialService_SetTenantCredential_Idempotent(t *testing.T) {
	svc := NewCredentialService(newTestDB(t))
	acmeCtx := pkgcore.WithTenant(t.Context(), "tenant-acme")

	if err := svc.SetTenantCredential(acmeCtx, ProviderOpenAICompatible, "sk-first", ""); err != nil {
		t.Fatalf("SetTenantCredential (first): %v", err)
	}
	if err := svc.SetTenantCredential(acmeCtx, ProviderOpenAICompatible, "sk-second", ""); err != nil {
		t.Fatalf("SetTenantCredential (second): %v", err)
	}

	cred, err := svc.Resolve(acmeCtx, ProviderOpenAICompatible)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.APIKey != "sk-second" {
		t.Fatalf("Resolve after two writes = %+v, want the second write to win (upsert)", cred)
	}
}

// --- tenant BYOK baseURL SSRF validation (credential.go, ssrf.go) ----------

// TestCredentialService_SetTenantCredential_BlockedBaseURL_Refused is the
// SSRF regression: a tenant's own BYOK credential write naming a private,
// loopback, link-local or otherwise blocked destination is refused at
// creation time, before any row is stored -- never silently accepted and
// dialed later from the platform's network. The cases cover the two
// refusal paths: literal IPs the caller typed into the URL (refused
// without any DNS lookup) and a hostname whose real DNS answer is blocked
// (localhost resolving to loopback through the hosts file, no resolver
// seam needed).
func TestCredentialService_SetTenantCredential_BlockedBaseURL_Refused(t *testing.T) {
	svc := NewCredentialService(newTestDB(t))
	acmeCtx := pkgcore.WithTenant(t.Context(), "tenant-acme")

	cases := []struct{ name, baseURL string }{
		{"loopback literal", "http://127.0.0.1:9000/v1"},
		{"IPv6 loopback literal", "http://[::1]:9000/v1"},
		{"cloud metadata literal", "http://169.254.169.254/latest/meta-data/"},
		{"private literal", "http://10.0.0.5/v1"},
		{"resolved loopback hostname", "http://localhost:9000/v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.SetTenantCredential(acmeCtx, ProviderOpenAICompatible, "sk-test", tc.baseURL)
			if got, ok := apperrCode(err); !ok || got != ErrBaseURLBlocked.Code {
				t.Fatalf("SetTenantCredential(baseURL %q) = %v, want ErrBaseURLBlocked -- a blocked destination must be refused before it is stored", tc.baseURL, err)
			}
			// Nothing was stored: the tenant still resolves nothing.
			if _, err := svc.Resolve(acmeCtx, ProviderOpenAICompatible); !apperr.HasCode(err, ErrCredentialNotFound.Code) {
				t.Fatalf("Resolve after a refused write = %v, want ErrCredentialNotFound -- the refusal must store no row", err)
			}
		})
	}
}

// TestCredentialService_SetTenantCredential_ResolvedBlockedHost_RefusalDoesNotDiscloseResolvedIP
// pins the deliberate asymmetry between the two blocked-refusal paths at
// the service level: when the refused destination was reached through a
// DNS resolution (the caller submitted a HOSTNAME), the refusal must NOT
// carry the resolved address in its params -- the resolved internal IP is
// information the caller does not have, so echoing it back would turn the
// refusal into an internal-DNS reconnaissance oracle (submit hostnames,
// read back the internal addresses they resolve to). See
// ErrBaseURLBlocked's own doc comment. localhost resolves to a loopback
// address through the real resolver on any standard system (hosts file,
// no network), so this needs no resolver seam.
func TestCredentialService_SetTenantCredential_ResolvedBlockedHost_RefusalDoesNotDiscloseResolvedIP(t *testing.T) {
	svc := NewCredentialService(newTestDB(t))
	acmeCtx := pkgcore.WithTenant(t.Context(), "tenant-acme")

	err := svc.SetTenantCredential(acmeCtx, ProviderOpenAICompatible, "sk-test", "http://localhost:9000/v1")
	if !apperr.HasCode(err, ErrBaseURLBlocked.Code) {
		t.Fatalf("SetTenantCredential(localhost) = %v, want ErrBaseURLBlocked", err)
	}
	found, _ := apperr.As(err)
	if ip, present := found.Params["ip"]; present {
		t.Fatalf("the resolution path's refusal carries the resolved address %v in its params -- an internal-DNS oracle for the caller; want no ip param", ip)
	}
}

// TestCredentialService_SetTenantCredential_BlockedLiteralIP_RefusalCarriesTheLiteralIP
// pins the mirror half of the same asymmetry: when the caller TYPED the
// blocked address into the URL, the refusal keeps carrying it in the ip
// param -- an echo of what the caller already knows, zero disclosure, and
// genuinely useful diagnostics (which address was refused).
func TestCredentialService_SetTenantCredential_BlockedLiteralIP_RefusalCarriesTheLiteralIP(t *testing.T) {
	svc := NewCredentialService(newTestDB(t))
	acmeCtx := pkgcore.WithTenant(t.Context(), "tenant-acme")

	err := svc.SetTenantCredential(acmeCtx, ProviderOpenAICompatible, "sk-test", "http://127.0.0.1:9000/v1")
	if !apperr.HasCode(err, ErrBaseURLBlocked.Code) {
		t.Fatalf("SetTenantCredential(127.0.0.1) = %v, want ErrBaseURLBlocked", err)
	}
	found, _ := apperr.As(err)
	if got := found.Params["ip"]; got != "127.0.0.1" {
		t.Fatalf("ip param = %v, want the literal address %q the caller typed", got, "127.0.0.1")
	}
}

// TestCredentialService_SetTenantCredential_PublicBaseURL_AcceptedAndStored
// pins the product boundary the SSRF guard deliberately preserves: a
// tenant may still point its BYOK credential at ANY public OpenAI-
// compatible endpoint -- the legitimate use the arbitrary-baseUrl
// capability exists for. A literal public IP needs no DNS resolution to
// validate and is never dialed by this service-level test, so the case is
// deterministic offline.
func TestCredentialService_SetTenantCredential_PublicBaseURL_AcceptedAndStored(t *testing.T) {
	svc := NewCredentialService(newTestDB(t))
	acmeCtx := pkgcore.WithTenant(t.Context(), "tenant-acme")

	if err := svc.SetTenantCredential(acmeCtx, ProviderOpenAICompatible, "sk-acme", "https://93.184.216.34/v1"); err != nil {
		t.Fatalf("SetTenantCredential(public baseURL) = %v, want nil -- a public destination must stay writable", err)
	}
	cred, err := svc.Resolve(acmeCtx, ProviderOpenAICompatible)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.Scope != CredentialScopeTenant || cred.BaseURL != "https://93.184.216.34/v1" {
		t.Fatalf("Resolve = %+v, want the stored public-URL row", cred)
	}
}

// TestCredentialService_SetPlatformCredential_PrivateBaseURL_StillAccepted
// pins the scope boundary of the SSRF guard: only the TENANT-scope write
// is tenant-influenceable and therefore validated. The
// platform-wide row is written by the operator under an audited system
// context (PermissionManagePlatform at the HTTP layer), and an intranet
// OpenAI-compatible LLM gateway is a legitimate platform default for the
// operator to declare -- refusing private destinations there would break
// exactly that deployment. A compromised tenant cannot reach or rewrite
// the platform row, so it cannot steer the platform dial either.
func TestCredentialService_SetPlatformCredential_PrivateBaseURL_StillAccepted(t *testing.T) {
	svc := NewCredentialService(newTestDB(t))
	sysCtx, err := pkgcore.WithSystemContext(t.Context(), systemTestCtx(t))
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}

	if err = svc.SetPlatformCredential(sysCtx, ProviderOpenAICompatible, "sk-platform", "http://10.0.0.9:9000/v1"); err != nil {
		t.Fatalf("SetPlatformCredential(private baseURL) = %v, want nil -- the platform-scope write is operator-trusted", err)
	}
	cred, err := svc.Resolve(t.Context(), ProviderOpenAICompatible)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.Scope != CredentialScopeSystem || cred.BaseURL != "http://10.0.0.9:9000/v1" {
		t.Fatalf("Resolve = %+v, want the stored platform row", cred)
	}
}

// TestCredentialService_SetTenantCredential_BlockedBaseURL_RefusedAcrossVersions
// pins the service-boundary refusal using only symbols that exist
// independently of the SSRF guard's own API (no ErrBaseURLBlocked, no
// ValidateBaseURL): a blocked-destination write is refused, and no row is
// stored -- the blocked destination never becomes a stored dial target
// waiting for a later call to reach it. (The code-level assertions of the
// refusal shape -- which coded error, which params -- live in the sibling
// tests above, which reference the guard's symbols directly.)
func TestCredentialService_SetTenantCredential_BlockedBaseURL_RefusedAcrossVersions(t *testing.T) {
	svc := NewCredentialService(newTestDB(t))
	acmeCtx := pkgcore.WithTenant(t.Context(), "tenant-acme")

	err := svc.SetTenantCredential(acmeCtx, ProviderOpenAICompatible, "sk-test", "http://127.0.0.1:9000/v1")
	if err == nil {
		t.Fatal("SetTenantCredential accepted and stored a loopback base URL -- the P0 the SSRF round closes; the write must be refused")
	}
	if _, err := svc.Resolve(acmeCtx, ProviderOpenAICompatible); !apperr.HasCode(err, ErrCredentialNotFound.Code) {
		t.Fatalf("Resolve after the refused write = %v, want ErrCredentialNotFound -- the refusal must store no row", err)
	}
}
