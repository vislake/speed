package attestation

import (
	"time"

	"github.com/vislake/speed/go/dbkit"
)

// attestationsTable is this package's persisted table name.
const attestationsTable = "smilesim_attestations"

// createAttestationsTableSQL is executed imperatively, with a plain CREATE
// TABLE IF NOT EXISTS -- the exact bootstrapping pattern
// internal/smilesim's own stores use and document (simulation_store.go's
// createSimulationsTableSQL: this app's own bookkeeping tables do not go
// through dbkit.MigrationRegistry's cross-module machinery). The statement
// is portable across both dbkit dialects: VARCHAR/TIMESTAMP columns,
// application-generated values, no PostgreSQL- or SQLite-specific syntax.
// The column set and types match the GORM model tags below exactly.
const createAttestationsTableSQL = `CREATE TABLE IF NOT EXISTS ` + attestationsTable + ` (
	object_id      VARCHAR(64)  NOT NULL PRIMARY KEY,
	tenant_id      VARCHAR(64)  NOT NULL,
	certificate_id VARCHAR(36)  NOT NULL,
	message        VARCHAR(512) NOT NULL,
	signature      VARCHAR(256) NOT NULL,
	created_at     TIMESTAMP    NOT NULL
)`

// createAttestationsTenantCreatedIndexSQL backs latestCertificate's
// per-tenant newest-row lookup (store.go). Kept as its own statement
// (CREATE INDEX IF NOT EXISTS is accepted by both SQLite and PostgreSQL)
// because the composite index cannot be declared portably inside the
// CREATE TABLE above.
const createAttestationsTenantCreatedIndexSQL = `CREATE INDEX IF NOT EXISTS idx_smilesim_attestations_tenant_created ON ` + attestationsTable + ` (tenant_id, created_at)`

// attestationRecord is the durable record of one output's attestation:
// that the platform vouches for the bytes of objectID (a go/storage object
// id) under the tenant, by certificateID, at the moment created_at
// records. One row per object -- object_id is the primary key -- holding
// the CURRENT attestation; a re-attestation (a rotated certificate, a
// re-signed output) replaces the row's certificate/message/signature via
// the guarded upsert in store.go's put.
//
// message is the canonical JSON bytes of attestedMessage (message.go),
// the exact bytes that were signed; signature is the raw Ed25519
// signature over message, hex-encoded (the portable TEXT spelling of a
// byte column). The message and the signature are kept as written so the
// gate verifies the signature over the stored bytes and then parses the
// message -- a stored row that was tampered with fails one of the two.
//
// The row is tenant data, following the exact shape of the reference
// app's other tenant-domain models (notes.Note, smilesim's
// simulationRecord): it embeds dbkit.TenantModel for its tenant_id column
// and GetTenantID method, with the tenant filter injected by dbkit's
// tenant-scoping plugin -- never written by hand. ObjectID is the primary
// key and is application-generated (a go/storage object id, globally
// unique), so no composite (tenant_id, object_id) key is needed, the same
// reasoning simulationRecord's own doc comment gives for JobID.
type attestationRecord struct {
	// ObjectID is the go/storage object id of the attested output.
	ObjectID string `gorm:"column:object_id;primaryKey;size:64"`

	// TenantModel promotes the tenant_id column and GetTenantID method
	// (satisfying dbkit.TenantScoped).
	dbkit.TenantModel

	// CertificateID is the pki_certificates row whose key signed the
	// message -- the tenant's current "simulation.attestation"
	// certificate at the time of this attestation.
	CertificateID string `gorm:"column:certificate_id;size:36;not null"`

	// Message is the canonical JSON bytes of the attested message, the
	// exact bytes signature was made over.
	Message string `gorm:"column:message;size:512;not null"`

	// Signature is the Ed25519 signature over Message, hex-encoded.
	Signature string `gorm:"column:signature;size:256;not null"`

	// CreatedAt records when this attestation was written -- the
	// per-tenant newest-row ordering key latestCertificate reads.
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
}

// TableName pins attestationRecord to attestationsTable, so it does not
// depend on GORM's pluralization of the (unexported) type name.
func (attestationRecord) TableName() string { return attestationsTable }

// compile-time check that attestationRecord satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = attestationRecord{}
