package pki

import (
	"time"

	"gorm.io/datatypes"

	"github.com/vislake/speed/go/dbkit"
)

// Table names, shared between the model TableName methods and the
// migrations' own header comments.
const (
	tableSigningKeys  = "pki_signing_keys"
	tableAuthorities  = "pki_authorities"
	tableCertificates = "pki_certificates"
	tableLocalKeys    = "pki_local_keys"
)

// The Algorithm vocabulary. Only Ed25519 exists today -- every Signer
// implementation can sign it directly, per docs/internal/22-pki.md's
// "Ed25519 direct-signs on all three implementations" finding -- but the column and the constant
// space exist so a second algorithm (a JWT alg an HSM only offers, say)
// slots in without a migration. AlgorithmUnsupportedBySigner names the
// failure a mismatched (algorithm, signer) pair produces.
const (
	AlgorithmEd25519 = "ed25519"
)

// The SigningKeyStatus vocabulary: the full five-value state machine
// docs/internal/22-pki.md's "lifecycle state machine" section describes. Getting every value
// into the column now is round 1's explicit job, even though only the
// pending->active transition is driven this round -- see Service's doc
// comment for exactly what that means in practice.
const (
	SigningKeyStatusPending  = "pending"
	SigningKeyStatusActive   = "active"
	SigningKeyStatusRetiring = "retiring"
	SigningKeyStatusRetired  = "retired"
	SigningKeyStatusRevoked  = "revoked"
)

// SigningKey is one row of pki_signing_keys: the key-lifecycle layer's core
// table. authn's future signing keys live here, entirely unrelated to X.509.
//
// # Data domain
//
// Platform data (docs/internal/04-data-and-tenancy.md): a signing key
// belongs to the whole deployment, not to one tenant, so SigningKey does
// NOT implement dbkit.TenantScoped and is reached through SigningKeyRepository's
// plain *gorm.DB rather than dbkit.Repository[T]. Its isolation is proven by
// tenancytest.AssertNotTenantScoped.
//
// # No private key column, ever
//
// SignerName and KeyRef are the only pointers to the actual key material:
// which Signer implementation owns it, and that implementation's own opaque
// handle. Nothing in this row, or in pki_authorities or pki_certificates
// below, ever holds a private key -- see the Signer doc comment for why
// that shape is load-bearing, not incidental.
//
// # ID is the JWT kid
//
// ID is an application-generated value and doubles as the JWT "kid" header
// a consumer's verifier keys its lookup on -- see Service.ActiveSigner.
//
// # Algorithm cross-check
//
// Algorithm records what this key actually is. docs/internal/22-pki.md's
// "authn's signing algorithm" section requires a consumer verifying a token to check
// the token header's alg against this column for the kid in question and
// reject on any mismatch, even while only one algorithm is legal overall --
// that check is the consumer's (authn's, from round 2), not this package's,
// since only the consumer parses tokens.
type SigningKey struct {
	// ID is the kid: an application-generated, globally unique identifier.
	ID string `gorm:"column:id;primaryKey;size:64"`

	// Purpose names what this key signs, for example "authn.access_token".
	// Only one row may hold PurposeStatusActive at a time; see the
	// migration's partial unique index.
	Purpose string `gorm:"column:purpose;size:128;not null"`

	// Algorithm is one of the Algorithm constants.
	Algorithm string `gorm:"column:algorithm;size:32;not null"`

	// SignerName names which Signer implementation owns the private key
	// ("local" this round; "vault" / "kms.aws" from round 4).
	SignerName string `gorm:"column:signer_name;size:64;not null"`

	// KeyRef is that Signer implementation's own opaque handle. Never a key.
	KeyRef string `gorm:"column:key_ref;size:255;not null"`

	// Status is one of the SigningKeyStatus constants.
	Status string `gorm:"column:status;size:16;not null"`

	// PublicKey is the DER SubjectPublicKeyInfo encoding
	// (x509.MarshalPKIXPublicKey), not sensitive -- verification needs
	// nothing else.
	PublicKey []byte `gorm:"column:public_key;not null"`

	NotBefore time.Time `gorm:"column:not_before;not null"`
	NotAfter  time.Time `gorm:"column:not_after;not null"`

	ActivatedAt *time.Time `gorm:"column:activated_at"`
	RetiredAt   *time.Time `gorm:"column:retired_at"`
	RevokedAt   *time.Time `gorm:"column:revoked_at"`
	// RevocationReason is "" until RevokedAt is set.
	RevocationReason string `gorm:"column:revocation_reason;size:255;not null;default:''"`

	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

// TableName names the pki_signing_keys table.
func (SigningKey) TableName() string { return tableSigningKeys }

// The AuthorityType vocabulary.
const (
	AuthorityTypeRoot         = "root"
	AuthorityTypeIntermediate = "intermediate"
)

// The AuthorityStatus vocabulary. Only AuthorityStatusActive is ever written
// this round -- revocation is round 3 -- but the column carries the full,
// eventual value set from day one, the same discipline SigningKey's Status
// column follows.
const (
	AuthorityStatusActive  = "active"
	AuthorityStatusRevoked = "revoked"
)

// Authority is one row of pki_authorities: one link of the internal CA
// chain, root or intermediate.
//
// # Data domain
//
// Platform data, for the identical reason SigningKey is: a CA belongs to
// the deployment, not to a tenant. Reached through AuthorityRepository's
// plain *gorm.DB; isolation proven by tenancytest.AssertNotTenantScoped.
//
// # ParentID is NULL for a root, an authority id for an intermediate
//
// A root authority signs its own certificate (CAService.CreateRootCA) and
// carries a nil ParentID. An intermediate's ParentID names the Authority
// that issued it. There is no uniqueness constraint keyed on ParentID (an
// authority may issue any number of intermediates beneath it), which is why
// this column is a genuine nullable pointer rather than the empty-string
// sentinel org/config use for a column a unique index also covers.
//
// # Same no-private-key rule as SigningKey
//
// SignerName/KeyRef point at the private key the same way SigningKey's do;
// CertificatePEM is the PEM encoding of this authority's own certificate,
// safe to expose (it is a certificate, not a secret).
type Authority struct {
	ID string `gorm:"column:id;primaryKey;size:36"`

	// Type is AuthorityTypeRoot or AuthorityTypeIntermediate.
	Type string `gorm:"column:type;size:16;not null"`

	// ParentID is nil for a root authority, else the issuing Authority's ID.
	ParentID *string `gorm:"column:parent_id;size:36"`

	// Subject is the certificate's subject, rendered as a pkix.Name string
	// (RFC 2253 form, e.g. "CN=speed Root CA").
	Subject string `gorm:"column:subject;size:255;not null"`

	// Serial is the certificate's serial number, lower-case hex encoded.
	// It is 16 bytes of crypto/rand -- NEVER a timestamp, the exact
	// anti-pattern docs/internal/22-pki.md's diagnosis names as a real
	// collision risk under concurrent issuance (see ca.go's newSerialNumber).
	Serial string `gorm:"column:serial;size:64;not null"`

	// CertificatePEM is this authority's own certificate, PEM encoded.
	CertificatePEM string `gorm:"column:certificate_pem;not null"`

	SignerName string `gorm:"column:signer_name;size:64;not null"`
	KeyRef     string `gorm:"column:key_ref;size:255;not null"`

	// Status is one of the AuthorityStatus constants.
	Status string `gorm:"column:status;size:16;not null;default:'active'"`

	NotBefore time.Time `gorm:"column:not_before;not null"`
	NotAfter  time.Time `gorm:"column:not_after;not null"`

	RevokedAt        *time.Time `gorm:"column:revoked_at"`
	RevocationReason string     `gorm:"column:revocation_reason;size:255;not null;default:''"`

	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

// TableName names the pki_authorities table.
func (Authority) TableName() string { return tableAuthorities }

// The CertificateStatus vocabulary. Only CertificateStatusActive is ever
// written this round; see Authority's identical note.
const (
	CertificateStatusActive  = "active"
	CertificateStatusRevoked = "revoked"
)

// Certificate is one row of pki_certificates: an end-entity certificate
// issued by one of this deployment's authorities.
//
// # Data domain
//
// Tenant data (docs/internal/04-data-and-tenancy.md): a certificate is
// meaningful only inside the tenant it was issued for, so Certificate
// implements dbkit.TenantScoped (via the embedded dbkit.TenantModel), is
// reached only through CertificateRepository (which embeds
// dbkit.Repository[Certificate]), and its isolation is proven by
// tenancytest.AssertIsolated. This is the one table of the four that is
// tenant-scoped -- getting this split right, rather than folding platform
// CAs and tenant certificates into one table the way the diagnosed system
// did, is the whole point of round 1's table design (docs/internal/22-pki.md,
// the "data model" section's opening rule: "one table must never mix two
// data domains").
//
// It embeds dbkit.TenantModel rather than declaring TenantID directly: ID
// is an application-generated UUID, already globally unique, so a plain
// indexed tenant_id column is enough (the same reasoning org.OrgNode and
// notification.InboxMessage record for their own TenantModel embeds). Do
// NOT redeclare a same-named TenantID field to add a primaryKey tag --
// dbkit's tenant_scope.go doc comment documents exactly how that silently
// breaks GetTenantID.
//
// # KeyDelivered
//
// KeyDelivered records a fact that has real security consequences: some
// consumers need the private key itself, not just a signing operation
// (docs/internal/22-pki.md's DBaaS diagnosis needed to hand JWKS-embedded
// private keys to a data-plane cluster). Once true, this platform no longer
// holds the only copy of the key, and Signer-side protection (envelope or
// direct-sign) has nothing left to protect: the honest mitigation is a
// short validity period plus rotation, not stronger encryption at rest.
// This round adds the column; the delivery path itself is explicitly out
// of scope (docs/internal/22-pki.md, the "deliberately out of scope"
// section: "no generic private-key export API").
type Certificate struct {
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes the tenant_id column and GetTenantID.
	dbkit.TenantModel

	// AuthorityID names the Authority that signed this certificate.
	// Deliberately not a foreign key: cross-module (and, per this file's
	// own repository-boundary discipline, cross-data-domain) foreign keys
	// are forbidden, so a caller resolves it through AuthorityRepository.
	AuthorityID string `gorm:"column:authority_id;size:36;not null"`

	// Purpose names what this certificate is for, in the same vocabulary
	// SigningKey.Purpose uses.
	Purpose string `gorm:"column:purpose;size:128;not null"`

	// Subject is the certificate's subject, RFC 2253 form.
	Subject string `gorm:"column:subject;size:255;not null"`

	// SANs holds the subject alternative names as a JSON array of strings.
	// datatypes.JSON, never a native array column -- the backend coding
	// standard bans PostgreSQL-only array types outright, and no query
	// ever filters into this column's structure.
	SANs datatypes.JSON `gorm:"column:sans"`

	// Serial is 16 bytes of crypto/rand, hex encoded -- see Authority.Serial.
	Serial string `gorm:"column:serial;size:64;not null"`

	// CertificatePEM is this certificate, PEM encoded.
	CertificatePEM string `gorm:"column:certificate_pem;not null"`

	SignerName string `gorm:"column:signer_name;size:64;not null"`
	KeyRef     string `gorm:"column:key_ref;size:255;not null"`

	// Status is one of the CertificateStatus constants.
	Status string `gorm:"column:status;size:16;not null;default:'active'"`

	// KeyDelivered is true once the private key itself has left this
	// platform's custody. See the type's own doc comment above.
	KeyDelivered bool `gorm:"column:key_delivered;not null;default:false"`

	NotBefore time.Time `gorm:"column:not_before;not null"`
	NotAfter  time.Time `gorm:"column:not_after;not null"`

	RevokedAt        *time.Time `gorm:"column:revoked_at"`
	RevocationReason string     `gorm:"column:revocation_reason;size:255;not null;default:''"`

	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

// TableName names the pki_certificates table.
func (Certificate) TableName() string { return tableCertificates }

// compile-time check that Certificate satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = Certificate{}

// LocalKey is one row of pki_local_keys: the LocalSigner implementation's
// own private-key store. Only LocalSigner ever reads or writes this table --
// the vault and kmsaws implementations (round 4) never touch it, because
// their key material never lives in this database at all.
//
// # Data domain
//
// Platform data: a locally-held private key is not owned by a tenant, and
// separating it from pki_signing_keys / pki_authorities / pki_certificates
// keeps "no business table ever holds a private key" true in the schema,
// not just in prose (docs/internal/22-pki.md's own framing of this table).
// Reached through LocalKeyRepository's plain *gorm.DB; isolation proven by
// tenancytest.AssertNotTenantScoped.
//
// # EncryptedPrivateKey
//
// Sealed with dbkit's field-level AES-256-GCM encryption
// (RegisterLocalKeySerializer / LocalKeySerializerName) -- authenticated and
// randomized, unlike the ECB-mode, no-IV cipher the diagnosed system used
// for the equivalent column. The plaintext form is the algorithm's standard
// private-key encoding (crypto/x509.MarshalPKCS8PrivateKey for Ed25519).
//
// # NotAfter
//
// Nullable, and NOT populated by this round's code paths: LocalSigner's
// GenerateKey (the only write path this round drives) takes no expiry
// parameter, by the Signer interface's own frozen shape -- the caller that
// knows a key's intended lifetime is the key-lifecycle layer (Service),
// which records NotAfter on the OWNING row (pki_signing_keys /
// pki_authorities / pki_certificates) instead. This column and its index
// exist now, ahead of any writer, because docs/internal/22-pki.md is
// explicit that round 2's expiry-scan job depends on scanning
// pki_local_keys directly (across every owning table at once, without a
// three-way join) -- "get the table structure right now or round 2 has to
// be redone". Populating it is round 2's job; leaving the column and index
// unused until then is the deliberately accepted state.
type LocalKey struct {
	// KeyRef is the opaque handle LocalSigner.GenerateKey returns, and the
	// same value a SigningKey/Authority/Certificate row's KeyRef names when
	// SignerName is "local".
	KeyRef string `gorm:"column:key_ref;primaryKey;size:64"`

	Algorithm string `gorm:"column:algorithm;size:32;not null"`

	// EncryptedPrivateKey is written and read only via the
	// LocalKeySerializerName gorm serializer (RegisterLocalKeySerializer):
	// at rest the column holds AES-256-GCM ciphertext; in memory, after a
	// successful read, this field holds the decrypted plaintext -- the
	// algorithm's standard PKCS#8 DER encoding (x509.MarshalPKCS8PrivateKey
	// for Ed25519) -- exactly the same on-read-decrypted convention
	// notification.VerifiedContact.Address documents for its own encrypted
	// column.
	EncryptedPrivateKey string `gorm:"column:encrypted_private_key;serializer:pki_local_key_enc;not null"`

	// NotAfter -- see the type's own doc comment above for why it exists
	// unpopulated this round.
	NotAfter *time.Time `gorm:"column:not_after"`

	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

// TableName names the pki_local_keys table.
func (LocalKey) TableName() string { return tableLocalKeys }

// SigningKey, Authority and LocalKey deliberately do NOT implement
// dbkit.TenantScoped -- the negative half of the data-domain split this
// file's doc comments describe. Go has no negative interface assertion, so
// that property is proven instead by tenancytest.AssertNotTenantScoped in
// repository_test.go for all three.
