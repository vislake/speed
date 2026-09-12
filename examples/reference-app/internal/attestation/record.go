package attestation

import (
	"time"

	"github.com/vislake/speed/go/dbkit"
)

// attestationsTable is this package's persisted table name.
const attestationsTable = "smilesim_attestations"

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
