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
	if err = svc.SetTenantCredential(acmeCtx, ProviderOpenAICompatible, "sk-acme-byok", "https://acme.example/v1"); err != nil {
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
