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
	// Service.Create (capped at the Service's configured lifetime --
	// WithMaxAPIKeyLifetime's value when the host set one, MaxAPIKeyLifetime
	// otherwise) and by whatever authenticates a request with it.
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

// tableAPIKeyHashIndex is the integration_api_key_hash_index table name.
const tableAPIKeyHashIndex = "integration_api_key_hash_index"

// apiKeyHashIndex is the narrow, deliberately non-tenant-scoped row that
// resolves a presented API key's owning tenant before any tenant is known at
// all -- this round's answer to the gap keygen.go's own hashAPIKeyToken doc
// comment named ("go/integration ships no Authenticate/Verify method yet ...
// there is no lookup path today that could ever feed this function
// attacker-influenced input"). Service.Authenticate is that lookup path, and
// this is how it resolves a tenant from a raw key alone -- mirroring
// go/sharing's shareTokenIndex (go/sharing/model.go) almost exactly, for the
// identical reason: an inbound API-key-authenticated request, like an
// anonymous share-link visitor, carries no separate tenant claim of its
// own -- the bearer credential IS the only thing identifying both who is
// calling and which tenant issued it, the same "sk_..." convention Stripe
// and GitHub use (keygen.go's own doc comment already cites them for the
// prefix choice alone; this round extends the analogy to how such a key is
// actually verified).
//
// # Why this needed a new table rather than the existing (tenant_id, hash)
// # unique index
//
// migrations/{sqlite,postgres}/0001_create_integration_api_keys.sql's own
// comment on uq_integration_api_keys_tenant_hash assumed a future
// authentication lookup would already know its tenant ("scoped by tenant
// first since every lookup this module ever performs already knows its
// tenant from request context") -- a reasonable guess at the time, since no
// round had built the lookup yet to test it against a real caller. It did
// not hold: dbkit's tenant-scope GORM plugin fails a TenantScoped model's
// query closed the moment its context carries no tenant
// (go/dbkit/tenant_scope.go's tenantScopeBeforeQuery), and there is no
// legitimate way for go/integration to run a raw, tenant-less query against
// APIKey itself -- that model implements dbkit.TenantScoped, so a `db.Table`/
// `db.Model`/`db.Raw` workaround would be exactly the bypass root
// CLAUDE.md's "Do not use db.Table/db.Model/db.Raw to work around the
// Repository" rule forbids, and go/integration holds no
// pkgcore.WithSystemContext grant (that escape hatch is restricted to
// admin/compliance/jobs/authn). A second, genuinely non-tenant-scoped table
// mapping hash -> tenant, exactly like sharing's shareTokenIndex, is the
// only path the existing architecture leaves open for "resolve a tenant from
// a credential alone" -- so this round adds one, without touching
// uq_integration_api_keys_tenant_hash, Hash's stored format, or any existing
// Service method's signature. migrations/postgres/0001's own comment is left
// as-is (an honest historical record of round 1's assumption); this type's
// migration (0006) records the correction in its own comment instead of
// editing an already-shipped file.
//
// # Data domain
//
// Platform data (docs/internal/04-data-and-tenancy.md), NOT tenant data, and
// deliberately so -- see shareTokenIndex's own doc comment for the identical
// reasoning applied here: implements no dbkit.TenantScoped, reached only
// through dbkit.Open()'s plain *gorm.DB (APIKeyRepository.tenantForHash),
// never dbkit.Repository[T], and its isolation suite is
// tenancytest.AssertNotTenantScoped, not AssertIsolated.
//
// # Deliberately narrow, and never updated after Create
//
// Two columns, nothing else -- no Scopes, no ExpiresAt, no RevokedAt. A row
// here answers exactly one question ("which tenant does this hash belong
// to") and nothing further; every other question about the key it names --
// is it revoked, expired, what scopes does it carry -- is still answered
// exclusively by the ordinary tenant-scoped APIKey row, reached only after
// this lookup hands back a tenant to attach to ctx (Service.Authenticate).
// APIKeyRepository.createWithHashIndex inserts this row in the same database
// transaction as its APIKey, mirroring
// (*sharing.ShareRepository).createWithTokenIndex exactly, so a key is never
// left reachable by its own tenant's ordinary List/Rotate/Revoke calls while
// being permanently unreachable by Authenticate. Service.Revoke never
// touches this row: it stays in place after revocation (and after expiry),
// because Authenticate needs it to resolve a tenant and reach the ordinary
// tenant-scoped read even for a key that has since been revoked or expired
// -- exactly how Authenticate is meant to answer that case (a refusal that
// is outward-identical to "no such key", never a dead end before the
// ordinary read is ever reached; see errors.go's ErrAuthenticationFailed).
// The one deliberate exception is the API-key expiry sweep
// (apikey_sweep.go): it removes this row together with its APIKey row, in
// one transaction (APIKeyRepository.deleteWithHashIndex), once the key's
// ExpiresAt has passed -- outward-safe, since Authenticate answers a swept
// key the identical ErrAuthenticationFailed it always did, now at this
// lookup instead of the ordinary read (see SweepExpiredAPIKeys' own doc
// comment).
type apiKeyHashIndex struct {
	// Hash is the exact same value APIKey.Hash stores -- hashAPIKeyToken's
	// hex-encoded SHA-256 of the raw key -- and the primary key here: two
	// keys hashing to the same value is cryptographically negligible (32
	// bytes of crypto/rand entropy, keygen.go), so the constraint is cheap
	// insurance, not a meaningfully defended invariant.
	Hash string `gorm:"column:hash;primaryKey;size:64"`

	// TenantID is the plain, unenforced tenant identifier this row exists to
	// answer -- unenforced in the identical sense every other platform-data
	// table's tenant_id column is (go/sharing's shareTokenIndex, go/jobs's
	// jobRecord, go/config's row): a real column, never filtered or
	// populated by dbkit's tenant-scope plugin, because this type implements
	// no TenantScoped.
	TenantID string `gorm:"column:tenant_id;size:64;not null"`
}

// TableName names the integration_api_key_hash_index table.
func (apiKeyHashIndex) TableName() string { return tableAPIKeyHashIndex }
