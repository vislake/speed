package integration

import (
	"encoding/json"
	"time"

	"gorm.io/datatypes"

	"github.com/vislake/speed/go/dbkit"
)

// tableAPIKeys is the integration_api_keys table name.
const tableAPIKeys = "integration_api_keys"

// APIKey is a credential a tenant issued for programmatic access to its own
// data, following docs/internal/07-platform-services.md's field list.
//
// # Data domain
//
// Tenant data (docs/internal/04-data-and-tenancy.md): a key belongs to
// exactly one tenant and must never be visible from another, so the model
// implements dbkit.TenantScoped, is reached only through APIKeyRepository,
// and its isolation is proven by tenancytest.AssertIsolated
// (repository_test.go).
//
// # The key material is never stored
//
// The value a caller authenticates with is generated once by
// newAPIKeyToken, handed to Service.Create's caller in CreatedAPIKey.Key,
// and never persisted. What the row keeps is Hash, the SHA-256 of that
// value (hashAPIKeyToken) -- the same "full-entropy randomness needs no
// dictionary-resistant hash" reasoning go/org's Invitation.TokenHash
// applies to its own bearer token, and deliberately NOT one of dbkit's
// reversible field-encryption serializers: encrypting a value that is
// already a one-way digest of a secret that is itself never stored would
// add a decryption key with nothing to decrypt back to, one more moving
// part protecting nothing. A leaked database backup therefore yields no
// usable key, and authenticating a request is a hash lookup, never a
// comparison against a stored secret.
//
// Prefix is the plaintext portion shown in a key list so an operator can
// tell two keys apart without ever seeing the rest -- see newAPIKeyToken's
// doc comment for its exact shape.
//
// # Scopes are frozen at issuance
//
// Scopes is the subset of CreatedBy's permissions Service.Create validated
// at the moment the key was issued (via the host-injected PermissionLister
// seam, see seams.go). It is stored, not re-derived: a later change to the
// creator's own permissions -- promoted, demoted, or removed from the
// tenant entirely -- never widens or shrinks an already-issued key, per
// the design doc's explicit rule that a key's scope does not change along
// with its creator's later permission changes. Changing what a key may do
// means issuing a new one; nothing in this module ever rewrites Scopes
// after Create.
//
// # The creator leaving does not revoke the key
//
// CreatedBy is the responsible party on record, not an ownership tie: the
// design doc is explicit that it is unacceptable for an integration to
// break just because someone left the tenant. What the doc does ask for is
// visibility -- APIKeySummary.CreatorLeft, computed at List time through
// the optional MembershipChecker seam -- so a tenant administrator notices
// a key needs a new owner of record, without the key itself being touched.
type APIKey struct {
	// ID is an application-generated UUID (uuid.NewString, in
	// Service.Create), globally unique on its own -- which is what lets the
	// primary key be (id) alone, tenant_id riding along as a plain,
	// non-key column promoted by the embedded TenantModel below. This
	// mirrors go/storage's Object precedent (see that type's own "Primary
	// key" doc comment section) rather than the composite (tenant_id, id)
	// shape a module-local id would need.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes the tenant_id column and the GetTenantID method
	// that satisfies dbkit.TenantScoped.
	dbkit.TenantModel

	// Prefix is the plaintext, non-secret portion of the key, shown in a key
	// list so an operator can recognize which key is which (e.g.
	// "sk_a1b2c3d4"). See newAPIKeyToken.
	Prefix string `gorm:"column:prefix;size:32;not null"`

	// Hash is the hex-encoded SHA-256 of the raw key. The raw value itself
	// is never stored; see the type's own doc comment.
	Hash string `gorm:"column:hash;size:64;not null"`

	// Scopes is the JSON array of permission strings this key was issued
	// with -- a subset of CreatedBy's permissions at the moment of Create,
	// frozen from then on. Read and written only through scopesJSON /
	// parseScopes, following the identical convention go/notification's
	// NotificationPreference.Channels documents for the same reason: no
	// native arrays (PostgreSQL-only), plain TEXT on both dialects, NOT
	// NULL, and the empty selection is the JSON empty array, never NULL.
	Scopes datatypes.JSON `gorm:"column:scopes;not null"`

	// CreatedBy is the authn user id of whoever issued this key -- an id
	// reference only, per the root CLAUDE.md's "no cross-module struct
	// imports" rule. It never changes after Create, even if the key is
	// later rotated (Rotate carries it forward from the predecessor).
	CreatedBy string `gorm:"column:created_by;size:64;not null"`

	// ExpiresAt is when this key stops authenticating anything, enforced by
	// Service.Create (capped at MaxAPIKeyLifetime) and by whatever
	// authenticates a request with it -- this round issues and revokes keys
	// but does not itself authenticate requests; see AGENTS.md's Deferred
	// list.
	ExpiresAt time.Time `gorm:"column:expires_at;not null"`

	// LastUsedAt is when this key last authenticated a request. Nil until
	// first use. Round 1 never writes it: no code path here authenticates a
	// request with a key (see the type's own Deferred note); the column
	// exists now so the migration that adds it later is not a breaking
	// schema change for an already-shipped table.
	LastUsedAt *time.Time `gorm:"column:last_used_at"`

	// RevokedAt is when this key was revoked, nil while it is live. A
	// non-nil value is permanent: nothing in this module ever clears it.
	RevokedAt *time.Time `gorm:"column:revoked_at"`

	// CreatedAt and UpdatedAt are written by gorm's autoCreateTime /
	// autoUpdateTime, never by application code and never by a database
	// default (SQLite has no NOW()).
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

// TableName names the integration_api_keys table.
func (APIKey) TableName() string { return tableAPIKeys }

// IsRevoked reports whether the key has been revoked.
func (k APIKey) IsRevoked() bool { return k.RevokedAt != nil }

// IsExpired reports whether the key's ExpiresAt is at or before now.
func (k APIKey) IsExpired(now time.Time) bool { return !now.Before(k.ExpiresAt) }

// compile-time check that APIKey satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = APIKey{}

// scopesJSON marshals a scope selection into the form the scopes column
// stores, following NotificationPreference.channelsJSON's exact contract: a
// nil slice marshals to the stored empty array "[]", never JSON null, since
// json.Marshal(nil []string) would otherwise blur "no scopes" into "no
// row", and the column is NOT NULL. Marshaling a []string cannot fail,
// which is why this function has no error return.
func scopesJSON(scopes []string) datatypes.JSON {
	if scopes == nil {
		scopes = []string{}
	}
	raw, _ := json.Marshal(scopes)
	return raw
}

// parseScopes decodes a stored scopes column back into a scope selection. A
// stored value that is not a JSON array of strings is a corrupt row -- the
// column is written only by scopesJSON -- and callers wrap the error as
// ErrStorage.
func parseScopes(stored datatypes.JSON) ([]string, error) {
	var scopes []string
	if err := json.Unmarshal(stored, &scopes); err != nil {
		return nil, err
	}
	return scopes, nil
}
