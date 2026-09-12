package attestation

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/vislake/speed/go/dbkit"
)

// AttestationStore is the tenant-scoped data-access type for
// attestationRecord -- the app-side mirror of smilesim's SimulationStore
// shape (internal/smilesim/simulation_store.go): it embeds
// dbkit.Repository[attestationRecord] instead of holding a plain
// *gorm.DB (root CLAUDE.md's multi-tenant isolation rule), and the two
// lookups Repository[T]'s deliberately minimal surface cannot express live
// here as their own methods, each running inside dbkit.WithTenantSession
// with the tenant half of the WHERE clause injected by dbkit's
// tenant-scoping plugin from the ctx tenant -- never written by hand.
//
// The zero value is not ready to use; construct one with
// NewAttestationStore. The table behind it is the attestation module's
// migrations (internal/attestation/migrations), applied by the assembly
// before anything can read or write.
type AttestationStore struct {
	*dbkit.Repository[attestationRecord]

	// db is the *gorm.DB the embedded Repository was built over, kept here
	// for the WithTenantSession calls below -- the very connection the
	// embedded Repository already uses, never a second pool (the identical
	// shape SimulationStore documents for its own db field).
	db *gorm.DB
}

// NewAttestationStore returns an AttestationStore backed by db, expected
// to come from dbkit.Open. It performs no I/O; the table and its
// per-tenant newest-row index are the attestation module's migrations
// (internal/attestation/migrations), which the assembly applies before
// anything can attest -- the host's attestation component carries the set
// and the database component applies it during the Verify stage. Tests
// apply the same set through dbtest.Migrate.
func NewAttestationStore(db *gorm.DB) *AttestationStore {
	return &AttestationStore{Repository: dbkit.NewRepository[attestationRecord](db), db: db}
}

// getByObject returns ctx's tenant's attestation row for objectID, and
// whether one exists -- false with a nil error means no attestation is on
// file under that tenant (the object was never registered as an AI output
// by this tenant). The lookup runs inside dbkit.WithTenantSession, so the
// answer can never name another tenant's row.
func (s *AttestationStore) getByObject(ctx context.Context, objectID string) (attestationRecord, bool, error) {
	var row attestationRecord
	err := dbkit.WithTenantSession(ctx, s.db, func(tx *gorm.DB) error {
		return tx.Where("object_id = ?", objectID).First(&row).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return attestationRecord{}, false, nil
	}
	if err != nil {
		return attestationRecord{}, false, err
	}
	return row, true, nil
}

// latestCertificate returns the certificate id of ctx's tenant's most
// recent attestation row -- the tenant's current "simulation.attestation"
// certificate, which EnsureAttested reuses while it is active -- and
// whether the tenant has any attestation row at all. The lookup runs
// inside dbkit.WithTenantSession with the tenant half of the WHERE clause
// injected by the tenant-scoping plugin; the created_at-descending order
// makes the newest row's certificate the answer.
func (s *AttestationStore) latestCertificate(ctx context.Context) (string, bool, error) {
	var row attestationRecord
	err := dbkit.WithTenantSession(ctx, s.db, func(tx *gorm.DB) error {
		return tx.Order("created_at DESC").Limit(1).First(&row).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return row.CertificateID, true, nil
}

// put writes one attestation row for ctx's tenant -- a first attestation
// of objectID or, when a row for the object already exists, a guarded
// replacement of its certificate/message/signature/created_at columns:
// the ON CONFLICT (object_id) DO UPDATE is the database-arbitrated
// last-write-wins that makes a concurrent re-attestation of one object
// converge on one row rather than erroring on the primary key. The row's
// tenant comes from the ctx, never from a parameter: the tenant-scoping
// plugin forces the tenant_id column to the ctx tenant on every create,
// overwriting whatever the caller populated -- the identical
// overwrite-the-caller guarantee every Repository write carries -- and
// the DO UPDATE never touches the tenant_id column, so a row can only
// ever be replaced under the tenant it was created under. The ctx tenant
// is already bound to the object before any caller reaches put: every
// write path first reads the object's content through storage, which
// refuses a tenant that does not own the object.
func (s *AttestationStore) put(ctx context.Context, record *attestationRecord) error {
	return dbkit.WithTenantSession(ctx, s.db, func(tx *gorm.DB) error {
		return tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "object_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"certificate_id", "message", "signature", "created_at",
			}),
		}).Create(record).Error
	})
}
