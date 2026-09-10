package aigateway

import (
	"fmt"
	"time"

	"github.com/vislake/speed/go/dbkit"
)

// CredentialScope names which tier of the ai_gateway_credentials table a
// credential row sits at, mirroring go/config's own Scope tier exactly
// (the "system" -> "tenant" fallback config.Service.Get walks).
type CredentialScope string

const (
	// CredentialScopeSystem is the platform-wide default credential: the
	// row every tenant resolves to when it has no BYOK row of its own.
	CredentialScopeSystem CredentialScope = "system"

	// CredentialScopeTenant is a tenant's own bring-your-own-key override,
	// tried first by Resolve and falling back to CredentialScopeSystem
	// when absent.
	CredentialScopeTenant CredentialScope = "tenant"
)

// CredentialAPIKeySerializerName is the GORM serializer name a host
// registers via dbkit.RegisterEncryptedSerializer before opening the
// database this module's migrations apply to (GORM resolves a named
// serializer at schema-parse time, so this must happen before dbkit.Open --
// the same ordering org.EmailSerializerName and
// notification.ContactAddressSerializerName already require of their own
// hosts). The cipher registered under this name must be a DIFFERENT key
// from any HMAC blind-index key elsewhere in the host's wiring --
// dbkit.NewCipher's own doc comment explains why an AES key must never
// double as an HMAC key -- though this module needs no blind index of its
// own at all: a credential is always looked up by (provider, scope,
// tenant), never by the key's own value.
const CredentialAPIKeySerializerName = "aigateway_credential_api_key"

// RegisterCredentialAPIKeySerializer wires cipher into GORM's serializer
// registry under CredentialAPIKeySerializerName, so a stored credential's
// api_key column is transparently sealed on write and opened on read. It is
// the host-facing half of the registration that constant's own doc comment
// documents.
//
// Call it during host bootstrap, BEFORE opening the *gorm.DB this module's
// models live in: GORM's registry is process-global and is consulted while
// a model's schema is parsed, so registering afterwards leaves the parsed
// schema pointing at nothing. It is deliberately NOT done inside
// Module.Register -- by then the database is already open, and Register is
// forbidden from doing anything but declare.
//
// A nil cipher is refused rather than registered: a model whose serializer
// silently does nothing would store a provider API key in plaintext, which
// is the one outcome the encrypted column exists to prevent. The cipher's
// key MUST be a different secret from any HMAC blind-index key elsewhere in
// the host's wiring; this module itself needs no blind index (a credential
// is looked up by provider, scope and tenant, never by its own value).
func RegisterCredentialAPIKeySerializer(cipher *dbkit.Cipher) error {
	if cipher == nil {
		return fmt.Errorf("aigateway: RegisterCredentialAPIKeySerializer requires a cipher for %q", CredentialAPIKeySerializerName)
	}
	dbkit.RegisterEncryptedSerializer(CredentialAPIKeySerializerName, cipher)
	return nil
}

// credentialRow is one stored row of the shared ai_gateway_credentials
// table.
//
// The table is platform data, not tenant data, shaped like go/config's own
// configs table (go/config/model.go): rows are written and read only
// through CredentialService's methods, which enforce the scope and
// system-context rules, so this model deliberately implements no
// dbkit.TenantScoped interface and is never touched through a
// dbkit.Repository[T]. The GORM tenant-isolation plugin only filters models
// that opt into tenancy, so a plain *gorm.DB carries no tenant filter for
// this table; credential_test.go's tenancytest.AssertNotTenantScoped proves
// the point, exactly as config's own model_test.go does for its row type.
//
// The primary key is (provider, scope, tenant_id): one row per provider per
// scope, with tenant_id disambiguating the rows of the tenant tier.
// tenant_id is NOT NULL and holds the empty string on system-tier rows --
// empty rather than NULL because NULLs are distinct in a PostgreSQL unique
// index: two system rows for one provider could otherwise coexist under
// NULL, where the empty-string sentinel collapses them into the single row
// the primary key promises.
type credentialRow struct {
	// Provider is the ChatProviderRegistry name this credential is for
	// (for example "chat.openai-compatible").
	Provider string `gorm:"column:provider;primaryKey;size:100"`
	// Scope is CredentialScopeSystem or CredentialScopeTenant.
	Scope string `gorm:"column:scope;primaryKey;size:16"`
	// TenantID is the owning tenant on a tenant-tier row, and the
	// empty-string sentinel on a system-tier row -- never SQL NULL.
	TenantID string `gorm:"column:tenant_id;primaryKey;size:64"`
	// APIKey is the vendor API key, encrypted at rest through the
	// CredentialAPIKeySerializerName serializer a host registers before
	// dbkit.Open.
	APIKey string `gorm:"column:api_key;serializer:aigateway_credential_api_key;not null"`
	// BaseURL is the OpenAI-compatible endpoint's base URL for this
	// credential (for example "https://api.openai.com/v1"), stored
	// unencrypted -- it identifies a network destination, not a secret.
	// Empty is legal: a provider whose constructor supplies its own
	// default base URL (or that ignores this field entirely) simply never
	// sees a value here. A non-empty value on a tenant-tier row was
	// SSRF-validated at write time (credential.go's SetTenantCredential,
	// ssrf.go's ValidateBaseURL); the system-tier row's value is the
	// operator's own choice and deliberately skips that validation
	// (ssrf.go's file header).
	BaseURL string `gorm:"column:base_url;size:500;not null"`
	// UpdatedAt is the moment of the last write.
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName names the shared ai_gateway_credentials table.
func (credentialRow) TableName() string { return "ai_gateway_credentials" }

// Credential is one resolved credential -- CredentialService.Resolve's
// result.
type Credential struct {
	// Provider is the ChatProviderRegistry name this credential resolves.
	Provider string
	// Scope reports which tier actually answered: CredentialScopeTenant
	// when the caller's own tenant had a BYOK row, CredentialScopeSystem
	// when it fell back to the platform-wide default.
	Scope CredentialScope
	// APIKey is the vendor API key, decrypted.
	APIKey string
	// BaseURL is the credential's configured base URL, possibly empty --
	// see credentialRow.BaseURL's own doc comment.
	BaseURL string
}
