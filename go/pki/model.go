package pki

import (
	"time"

	"gorm.io/datatypes"

	"github.com/vislake/speed/go/dbkit"
)

// Table names, shared between the model TableName methods and the
// migrations' own header comments.
const (
	tableSigningKeys           = "pki_signing_keys"
	tableAuthorities           = "pki_authorities"
	tableCertificates          = "pki_certificates"
	tableLocalKeys             = "pki_local_keys"
	tableCertificateRevocations = "pki_certificate_revocations"
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
	// RetiringAt is when this key was demoted from SigningKeyStatusActive to
	// SigningKeyStatusRetiring -- nil until that transition happens. It is
	// the reference point RetiringOverlap is measured from: a key becomes
	// eligible for the retiring->retired transition once
	// time.Now() >= *RetiringAt + RetiringOverlap. See RetiringOverlap's own
	// doc comment for where the duration itself comes from.
	RetiringAt *time.Time `gorm:"column:retiring_at"`
	RetiredAt  *time.Time `gorm:"column:retired_at"`
	RevokedAt  *time.Time `gorm:"column:revoked_at"`
	// RevocationReason is "" until RevokedAt is set.
	RevocationReason string `gorm:"column:revocation_reason;size:255;not null;default:''"`

	// RetiringOverlap is how long this key stays in SigningKeyStatusRetiring
	// (verifiable but not selected as ActiveSigner) once it is demoted from
	// active, before the expiry scan retires it for good.
	//
	// docs/internal/22-pki.md's "retiring overlap period" section is
	// explicit that pki does not know this number -- the consumer that
	// knows the maximum lifetime of a credential signed under this key
	// declares it, once, as EnsurePurpose's maxCredentialLifetime
	// parameter. Recording it HERE, on the key row itself, rather than in a
	// separate purpose-policy table, is what lets the jobs-driven expiry
	// scan (which runs on a schedule, with no caller supplying
	// maxCredentialLifetime on each tick) demote an active key into
	// retiring without having to ask anyone: it reads the value the key
	// already carries. A key EnsurePurpose creates copies
	// maxCredentialLifetime directly; a key the expiry scan stages ahead of
	// rotation copies it from the active key it will eventually replace,
	// since one purpose's overlap requirement does not change key to key.
	RetiringOverlap time.Duration `gorm:"column:retiring_overlap;not null;default:0"`

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

// The AuthorityStatus vocabulary. AuthorityStatusActive was the only value
// round 1 ever wrote; round 3 (this round) does not add a public API that
// writes AuthorityStatusRevoked either -- see ca.go's VerifyCertificate doc
// comment for why the chain-verification path still defends against it
// (a revoked authority can only appear in this round's own tests by direct
// repository seeding, matching round 1's identical precedent for
// SigningKeyStatusRevoked).
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

	// CRLDistributionPoint is the URL this authority's own CRL is served
	// at -- round 3's addition (migration 0006). Empty means "no CRL
	// extension is written into certificates this authority signs", the
	// same convention every NotAfter/validity field in this module already
	// follows for an unset value: never a broken placeholder URL. It is
	// set once, at CreateRootCA/CreateIntermediateCA time (RootCAParams/
	// IntermediateCAParams.CRLDistributionPoint), and read at issuance
	// time by CreateIntermediateCA/IssueCertificate to populate the
	// CHILD certificate's CRLDistributionPoints extension -- a
	// certificate's CRLDP names where to fetch the CRL that lists ITS
	// OWN revocation, i.e. the CRL of the authority that signed it, never
	// the certificate's own (an end-entity certificate has no CRL of its
	// own to distribute).
	CRLDistributionPoint string `gorm:"column:crl_distribution_point;size:500;not null;default:''"`

	// CRLNumber is the CRL sequence number (RFC 5280 §5.2.3) of the most
	// recently generated CRL, monotonically increasing across every call to
	// CAService.GenerateCRL for this authority. 0 means no CRL has been
	// generated yet.
	CRLNumber int64 `gorm:"column:crl_number;not null;default:0"`

	// CRLPEM is the most recently generated CRL, PEM encoded ("X509 CRL"
	// block) -- empty until GenerateCRL runs at least once. Stored here,
	// rather than recomputed on every fetch, because a CRL is meant to be a
	// stable, periodically-refreshed document a verifier caches, not a
	// live query result; see crl.go's GenerateCRL for how it is produced
	// and job.go's crlRegenerateHandler for how a host schedules refreshes.
	CRLPEM string `gorm:"column:crl_pem"`

	CRLIssuedAt   *time.Time `gorm:"column:crl_issued_at"`
	CRLNextUpdate *time.Time `gorm:"column:crl_next_update"`

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
// Nullable, and still NOT populated by any of this round's code paths, even
// though round 2 does add the expiry-scan job docs/internal/22-pki.md
// anticipated when this column and its index were built ahead of any
// writer: LocalSigner.GenerateKey (the only write path that would set it)
// still takes no expiry parameter, by the Signer interface's own frozen
// shape, and round 2's Service (the caller that DOES know a key's intended
// lifetime) has no signer-specific channel back to LocalSigner to hand that
// lifetime to without Service acquiring knowledge of which Signer
// implementation "local" actually is -- exactly the kind of business-code-
// depends-on-concrete-implementation coupling this codebase's architecture
// discipline forbids. Round 2's expiry scan (lifecycle.go) therefore reads
// only the owning rows' own NotAfter (pki_signing_keys, unconditionally;
// pki_authorities/pki_certificates are round 3's scope, since their
// lifecycle is revocation-and-CRL shaped, not pending/active/retiring/
// retired), never pki_local_keys directly. This column and its index remain
// unused until a future round finds a shape for the Signer/Service boundary
// that does not require this coupling -- see AGENTS.md's Known limitations.
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

// CertificateRevocation is one row of pki_certificate_revocations
// (migration 0007, round 3): a denormalized, append-only ledger entry
// written whenever CAService.RevokeCertificate revokes a certificate.
//
// # Why this table exists at all -- the cross-tenant CRL problem
//
// pki_certificates is tenant data (Certificate's own doc comment):
// dbkit.Repository[Certificate] filters every read by the caller's ctx
// tenant, with no cross-tenant escape -- see
// tenancy.WithSystemContext's own doc comment, which is explicit that
// granting a system context does NOT by itself unlock a cross-tenant
// Repository[T] read. But a CRL is a property of the ISSUING AUTHORITY,
// which is platform data: "the CRL for authority X" genuinely means every
// revoked certificate that authority ever signed, regardless of which
// tenant it was issued to. Enumerating that with dbkit.Repository[T] would
// require iterating every tenant the platform has ever provisioned, which
// this module has no way to enumerate (it does not depend on org or any
// tenant directory) and would not scale as one grows.
//
// This table is the same accommodation the codebase already makes
// elsewhere for exactly this shape of problem: `send_records` and
// `platform_blacklist` (go/notification) and `AuditEvent` (go/dbkit/audit)
// are all platform data carrying a real, deliberately UNENFORCED tenant_id
// column, precisely so a platform-wide scan needs no tenant loop. This
// table follows the identical convention: TenantID here is informational
// only (which tenant the revoked certificate happened to belong to, for
// an eventual audit view), never filtered on, and CertificateRevocation
// does NOT implement dbkit.TenantScoped -- proven negatively by
// tenancytest.AssertNotTenantScoped in repository_test.go, the same way
// SigningKey/Authority/LocalKey are proven above.
//
// # Not a foreign key
//
// CertificateID and AuthorityID name rows of pki_certificates and
// pki_authorities respectively, but neither is a real FK constraint: this
// row spans two different data domains (platform vs. tenant) the same way
// Certificate.AuthorityID already does not carry one, for the identical
// reason (this file's own repository-boundary discipline; root CLAUDE.md's
// cross-module-FK rule, which applies here even though this is all one
// module because the two tables sit in different domains).
//
// # Not atomic with the pki_certificates write
//
// RevokeCertificate (ca.go) writes this row as a SECOND statement after
// updating pki_certificates, not inside one shared transaction --
// dbkit.Repository[T] exposes no hook to compose its own transaction with
// a plain *gorm.DB write, and building one by hand would mean reaching
// around Repository[T] with a raw *gorm.DB.Transaction, the exact
// bypass this codebase's multi-tenant isolation discipline forbids for a
// tenant-scoped write. The accepted risk is narrow: a crash between the
// two writes leaves the certificate correctly marked revoked but
// (temporarily) missing from this ledger, so a CRL generated before the
// gap is noticed and retried would omit that one certificate. Both writes
// are individually idempotent (RevokeCertificate treats an
// already-revoked certificate as a no-op, and a lost ledger row is fixed
// by revoking again, which is safe), so the failure mode is bounded
// staleness, never an incorrect "not revoked" answer inside
// pki_certificates itself, which is the record CAService.VerifyCertificate
// actually trusts.
type CertificateRevocation struct {
	// ID is an application-generated UUID.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// CertificateID names the pki_certificates row this entry records the
	// revocation of.
	CertificateID string `gorm:"column:certificate_id;size:36;not null"`

	// AuthorityID names the pki_authorities row that issued the revoked
	// certificate -- CRLRepository.ListByAuthority's query key, and the
	// whole reason this table exists rather than a query against
	// pki_certificates directly.
	AuthorityID string `gorm:"column:authority_id;size:36;not null"`

	// Serial is the revoked certificate's serial number, the same
	// lower-case hex encoding Certificate.Serial stores -- what actually
	// goes into the generated CRL's revoked-certificate entry.
	Serial string `gorm:"column:serial;size:64;not null"`

	// TenantID is informational only -- see the type's own doc comment
	// above. Never filtered on, never enforced.
	TenantID string `gorm:"column:tenant_id;size:64;not null"`

	RevokedAt        time.Time `gorm:"column:revoked_at;not null"`
	RevocationReason string    `gorm:"column:revocation_reason;size:255;not null"`

	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
}

// TableName names the pki_certificate_revocations table.
func (CertificateRevocation) TableName() string { return tableCertificateRevocations }

// SigningKey, Authority, LocalKey and CertificateRevocation deliberately do
// NOT implement dbkit.TenantScoped -- the negative half of the data-domain
// split this file's doc comments describe. Go has no negative interface
// assertion, so that property is proven instead by
// tenancytest.AssertNotTenantScoped in repository_test.go for all four.
