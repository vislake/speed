package aigateway

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
)

// CredentialService resolves and stores the platform-key / BYOK credentials
// Gateway needs to call a named ChatProvider. See model.go's own doc
// comment for the table shape and its data-domain classification.
//
// The zero value is not ready to use; construct one with NewCredentialService.
type CredentialService struct {
	store *credentialStore
}

// NewCredentialService returns a CredentialService whose
// ai_gateway_credentials table lives in db. Constructing one performs no
// I/O -- opening and migrating db is the caller's responsibility, done
// before any method here is called, exactly like every other module's
// service in this codebase.
func NewCredentialService(db *gorm.DB) *CredentialService {
	return &CredentialService{store: &credentialStore{db: db}}
}

// Resolve returns the effective credential for provider: the caller's own
// tenant BYOK row when ctx carries a tenant AND that tenant has one, the
// platform-wide row otherwise.
//
// A context carrying no tenant (pkgcore.TenantFromContext's ok == false)
// skips straight to the platform-wide row -- this is not an error, since a
// system-context caller (a background job, an admin operation) has no
// tenant to prefer a BYOK row for.
//
// Resolve returns an error wrapping ErrCredentialNotFound, naming provider,
// when neither tier has a row.
func (s *CredentialService) Resolve(ctx context.Context, provider string) (Credential, error) {
	if tenant, ok := pkgcore.TenantFromContext(ctx); ok {
		row, err := s.store.get(ctx, provider, CredentialScopeTenant, string(tenant))
		if err != nil {
			return Credential{}, err
		}
		if row != nil {
			return credentialFromRow(*row), nil
		}
	}

	row, err := s.store.get(ctx, provider, CredentialScopeSystem, "")
	if err != nil {
		return Credential{}, err
	}
	if row == nil {
		return Credential{}, ErrCredentialNotFound.WithParam("provider", provider)
	}
	return credentialFromRow(*row), nil
}

// SetPlatformCredential writes provider's platform-wide default credential.
// ctx must carry an audited system reason (pkgcore.WithSystemContext, or
// tenancy.WithSystemContext, which additionally publishes an audit event) --
// an ordinary tenant-scoped context is refused with
// ErrSystemScopeRequiresSystemContext. provider and apiKey must both be
// non-empty (ErrCredentialRequired otherwise); baseURL may be empty.
//
// Unlike SetTenantCredential, a non-empty baseURL is deliberately NOT
// SSRF-validated: this is the operator's own default, written under an
// audited system context, and an intranet OpenAI-compatible LLM gateway is
// a legitimate platform-wide vendor for a deployment whose tenants all
// share it -- see ssrf.go's own file header for the scope boundary.
func (s *CredentialService) SetPlatformCredential(ctx context.Context, provider, apiKey, baseURL string) error {
	if provider == "" || apiKey == "" {
		return ErrCredentialRequired.WithParam("provider", provider)
	}
	if _, ok := pkgcore.SystemReasonFromContext(ctx); !ok {
		return ErrSystemScopeRequiresSystemContext.WithParam("provider", provider)
	}
	return s.store.put(ctx, credentialRow{
		Provider:  provider,
		Scope:     string(CredentialScopeSystem),
		TenantID:  "",
		APIKey:    apiKey,
		BaseURL:   baseURL,
		UpdatedAt: time.Now().UTC(),
	})
}

// SetTenantCredential writes the calling tenant's own BYOK credential for
// provider. The owning tenant comes from ctx (pkgcore.MustTenantFromContext)
// -- never from a caller-supplied identifier -- so a context with no tenant
// is refused with ErrTenantScopeRequiresTenant. provider and apiKey must
// both be non-empty (ErrCredentialRequired otherwise); baseURL may be
// empty.
//
// A non-empty baseURL is SSRF-validated by ValidateBaseURL before anything
// is stored (ssrf.go): this row is the tenant's own influence over where
// the platform's network dials, so a baseURL naming a private, loopback,
// link-local or otherwise blocked destination is refused with
// ErrBaseURLInvalid / ErrBaseURLUnresolvable / ErrBaseURLBlocked -- never
// accepted and dialed later. The refusal's answer shape follows the
// no-IP-echo rule ErrBaseURLBlocked's own doc comment pins. The
// platform-scope write (SetPlatformCredential) is deliberately NOT
// validated: its row is operator-written under an audited system context,
// and an intranet OpenAI-compatible gateway is a legitimate platform
// default -- see ssrf.go's own file header for the full boundary.
func (s *CredentialService) SetTenantCredential(ctx context.Context, provider, apiKey, baseURL string) error {
	if provider == "" || apiKey == "" {
		return ErrCredentialRequired.WithParam("provider", provider)
	}
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return ErrTenantScopeRequiresTenant.WithParam("provider", provider)
	}
	if baseURL != "" {
		// The empty string means "no base URL configured here" and is legal
		// to store (credentialRow.BaseURL's doc comment) -- only a value a
		// tenant actually named is a dial destination worth validating.
		if err := ValidateBaseURL(ctx, baseURL); err != nil {
			return err
		}
	}
	return s.store.put(ctx, credentialRow{
		Provider:  provider,
		Scope:     string(CredentialScopeTenant),
		TenantID:  string(tenant),
		APIKey:    apiKey,
		BaseURL:   baseURL,
		UpdatedAt: time.Now().UTC(),
	})
}

// credentialFromRow converts a stored row into the Credential Resolve
// returns.
func credentialFromRow(r credentialRow) Credential {
	return Credential{
		Provider: r.Provider,
		Scope:    CredentialScope(r.Scope),
		APIKey:   r.APIKey,
		BaseURL:  r.BaseURL,
	}
}
