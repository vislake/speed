package pki

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/vislake/speed/go/dbkit"
)

// SigningKeyRepository is the plain, non-tenant-scoped accessor for
// pki_signing_keys. SigningKey is platform data (see its own doc comment),
// so this wraps a bare *gorm.DB rather than embedding dbkit.Repository[T] --
// dbkit's own rule that identity/platform data must NOT implement
// TenantScoped means the generic base is not an option here.
type SigningKeyRepository struct {
	db *gorm.DB
}

// NewSigningKeyRepository returns a SigningKeyRepository over db. db is
// expected to come from dbkit.Open with this module's migrations applied.
func NewSigningKeyRepository(db *gorm.DB) *SigningKeyRepository {
	return &SigningKeyRepository{db: db}
}

// Create inserts key.
func (r *SigningKeyRepository) Create(ctx context.Context, key *SigningKey) error {
	return r.db.WithContext(ctx).Create(key).Error
}

// FindByID returns the row for id, or (nil, ErrKeyNotFound).
func (r *SigningKeyRepository) FindByID(ctx context.Context, id string) (*SigningKey, error) {
	var key SigningKey
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	return &key, nil
}

// FindActiveByPurpose returns the row currently in SigningKeyStatusActive
// for purpose, or (nil, ErrNoActiveKey) when none exists. The migration's
// partial unique index guarantees at most one such row can ever exist.
func (r *SigningKeyRepository) FindActiveByPurpose(ctx context.Context, purpose string) (*SigningKey, error) {
	var key SigningKey
	err := r.db.WithContext(ctx).
		Where("purpose = ? AND status = ?", purpose, SigningKeyStatusActive).
		First(&key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNoActiveKey
	}
	if err != nil {
		return nil, err
	}
	return &key, nil
}

// ListVerifiableByPurpose returns every row for purpose whose Status is
// SigningKeyStatusPending, SigningKeyStatusActive or
// SigningKeyStatusRetiring, in no particular order. This is the query
// Service.VerificationKeys reads: "all still-verifiable keys". A pending
// key's public key is included per docs/internal/22-pki.md's "pending
// exists for the distributed race" section -- it is safe to publish for
// verification before it ever signs anything -- while a retired (or
// revoked) key is excluded: the whole point of the retiring overlap period
// is that it ends, and a key past it must stop being offered for
// verification, not merely stop being selected as ActiveSigner.
func (r *SigningKeyRepository) ListVerifiableByPurpose(ctx context.Context, purpose string) ([]SigningKey, error) {
	var keys []SigningKey
	err := r.db.WithContext(ctx).
		Where("purpose = ? AND status IN ?", purpose, []string{
			SigningKeyStatusPending,
			SigningKeyStatusActive,
			SigningKeyStatusRetiring,
		}).
		Find(&keys).Error
	return keys, err
}

// Update persists every field of key, including its Status transition and
// the lifecycle timestamps (ActivatedAt/RetiringAt/RetiredAt) that go with
// it. Callers are expected to have loaded key from this repository (or
// built it fresh via Create) rather than constructing a partial row by
// hand, since Save writes every column.
func (r *SigningKeyRepository) Update(ctx context.Context, key *SigningKey) error {
	return r.db.WithContext(ctx).Save(key).Error
}

// PromoteToActive atomically promotes the pending key pendingID to
// SigningKeyStatusActive and, when previousActiveID is non-empty, demotes
// that other row from SigningKeyStatusActive to SigningKeyStatusRetiring in
// the SAME transaction -- the pending->active and active->retiring
// transitions docs/internal/22-pki.md's lifecycle diagram draws as one
// arrow (a new key's activation causing the old one's demotion into
// retiring), which is also why this
// module publishes no separate ".retiring" event: EventSigningKeyActivated
// communicates both halves of this one atomic write.
//
// The demotion runs FIRST, deliberately: uq_pki_signing_keys_active_purpose
// (migration 0001) is a partial unique index checked at each statement, not
// deferred to commit, so promoting pendingID to active before previousActiveID
// has left SigningKeyStatusActive would momentarily leave two active rows
// for the same purpose inside this very transaction and be refused by the
// database it is trying to write to. Demoting first briefly leaves the
// purpose with NO active row instead, which the index has nothing to say
// about.
//
// now is used for both RetiringAt (the demoted key) and ActivatedAt (the
// promoted key), so the two rows agree on exactly when the rotation
// happened.
func (r *SigningKeyRepository) PromoteToActive(ctx context.Context, pendingID string, previousActiveID string, now time.Time) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if previousActiveID != "" {
			// A struct (not a map) as the Updates argument, with no .Model()
			// call: GORM infers the table from the struct type and, since
			// every other field is left at its zero value, writes only
			// Status and RetiringAt -- the same partial-update shape the
			// map form gave, without the raw-GORM-bypass entry point
			// (tools/semgrep_rules/raw-gorm-bypass.yml).
			if err := tx.Where("id = ? AND status = ?", previousActiveID, SigningKeyStatusActive).
				Updates(&SigningKey{
					Status:     SigningKeyStatusRetiring,
					RetiringAt: &now,
				}).Error; err != nil {
				return err
			}
		}
		res := tx.Where("id = ? AND status = ?", pendingID, SigningKeyStatusPending).
			Updates(&SigningKey{
				Status:      SigningKeyStatusActive,
				ActivatedAt: &now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrKeyNotFound
		}
		return nil
	})
}

// RetireRetiring marks the retiring key id as SigningKeyStatusRetired.
func (r *SigningKeyRepository) RetireRetiring(ctx context.Context, id string, now time.Time) error {
	// See PromoteToActive's comment: a struct Updates argument replaces the
	// map-plus-.Model() form to stay clear of the raw-GORM-bypass entry
	// point.
	res := r.db.WithContext(ctx).
		Where("id = ? AND status = ?", id, SigningKeyStatusRetiring).
		Updates(&SigningKey{
			Status:    SigningKeyStatusRetired,
			RetiredAt: &now,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrKeyNotFound
	}
	return nil
}

// ListByStatus returns every row in status, in no particular order. The
// expiry scan uses it for both the pending set (promotion candidates) and
// the retiring set (retirement candidates) -- both are expected to stay
// small (at most one row per purpose in the common case), so loading them
// in full is not a scan-scale concern the way ListActiveNearingExpiry's own
// not_after-indexed query is.
func (r *SigningKeyRepository) ListByStatus(ctx context.Context, status string) ([]SigningKey, error) {
	var keys []SigningKey
	err := r.db.WithContext(ctx).Where("status = ?", status).Find(&keys).Error
	return keys, err
}

// ListActiveNearingExpiry returns every SigningKeyStatusActive row whose
// NotAfter is at or before before -- the expiry scan's staging query, read
// through idx_pki_signing_keys_not_after.
func (r *SigningKeyRepository) ListActiveNearingExpiry(ctx context.Context, before time.Time) ([]SigningKey, error) {
	var keys []SigningKey
	err := r.db.WithContext(ctx).
		Where("status = ? AND not_after <= ?", SigningKeyStatusActive, before).
		Find(&keys).Error
	return keys, err
}

// Revoke marks the signing key id as SigningKeyStatusRevoked, recording
// revokedAt and reason -- the same guarded, status-checked update shape
// PromoteToActive/RetireRetiring use, so a concurrent revoke can never
// silently race a rotation and leave the row in an inconsistent state.
//
// Reports (true, nil) when this call performed the transition, and (false,
// nil) when id exists but was already SigningKeyStatusRevoked -- an
// idempotent no-op Service.RevokeSigningKey relies on to avoid publishing a
// second EventSigningKeyRevoked for an already-revoked key. Reports (false,
// ErrKeyNotFound) when id does not exist at all.
func (r *SigningKeyRepository) Revoke(ctx context.Context, id, reason string, now time.Time) (bool, error) {
	res := r.db.WithContext(ctx).
		Where("id = ? AND status != ?", id, SigningKeyStatusRevoked).
		Updates(&SigningKey{
			Status:           SigningKeyStatusRevoked,
			RevokedAt:        &now,
			RevocationReason: reason,
		})
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected > 0 {
		return true, nil
	}
	// RowsAffected == 0 is ambiguous on its own -- either id does not exist,
	// or it does but was already revoked. FindByID tells the two apart.
	if _, err := r.FindByID(ctx, id); err != nil {
		return false, err
	}
	return false, nil
}

// ExistsByPurposeAndStatus reports whether purpose already has a row in
// status. The expiry scan's staging step uses it to avoid staging a second
// pending key for a purpose that already has one in flight.
func (r *SigningKeyRepository) ExistsByPurposeAndStatus(ctx context.Context, purpose, status string) (bool, error) {
	// Count needs .Model() to know the table when nothing else in the chain
	// carries a struct type; a bounded Find into a typed slice gets the same
	// existence answer -- at most one row is ever fetched -- without the
	// raw-GORM-bypass entry point (tools/semgrep_rules/raw-gorm-bypass.yml).
	var keys []SigningKey
	err := r.db.WithContext(ctx).
		Where("purpose = ? AND status = ?", purpose, status).
		Limit(1).
		Find(&keys).Error
	if err != nil {
		return false, err
	}
	count := int64(len(keys))
	return count > 0, nil
}

// ListByPurposeAndStatuses returns every row for purpose whose Status is one
// of statuses, in no particular order. Round 3's addition, for
// Service.ExportJWKS (jwks.go): the key-lifecycle layer's JWKS export
// deliberately carries only SigningKeyStatusActive and
// SigningKeyStatusRetiring keys, per docs/internal/22-pki.md's own
// distinction -- NOT SigningKeyStatusPending, unlike
// ListVerifiableByPurpose's internal-verification query above. A pending
// key's public key is safe to trust for THIS process's own verification
// path before the propagation window elapses (ListVerifiableByPurpose's own
// doc comment explains why), but an external verifier pulling a JWKS
// document has no such relationship to the propagation window at all -- it
// simply should not be told about a key this deployment has not started
// using yet.
func (r *SigningKeyRepository) ListByPurposeAndStatuses(ctx context.Context, purpose string, statuses ...string) ([]SigningKey, error) {
	var keys []SigningKey
	err := r.db.WithContext(ctx).
		Where("purpose = ? AND status IN ?", purpose, statuses).
		Find(&keys).Error
	return keys, err
}

// AuthorityRepository is the plain, non-tenant-scoped accessor for
// pki_authorities. Authority is platform data, for the identical reason
// SigningKeyRepository above documents.
type AuthorityRepository struct {
	db *gorm.DB
}

// NewAuthorityRepository returns an AuthorityRepository over db.
func NewAuthorityRepository(db *gorm.DB) *AuthorityRepository {
	return &AuthorityRepository{db: db}
}

// Create inserts authority.
func (r *AuthorityRepository) Create(ctx context.Context, authority *Authority) error {
	return r.db.WithContext(ctx).Create(authority).Error
}

// FindByID returns the row for id, or (nil, ErrAuthorityNotFound).
func (r *AuthorityRepository) FindByID(ctx context.Context, id string) (*Authority, error) {
	var authority Authority
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&authority).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrAuthorityNotFound
	}
	if err != nil {
		return nil, err
	}
	return &authority, nil
}

// Update persists every field of authority -- the same full-Save shape
// SigningKeyRepository.Update documents. Round 3's addition; GenerateCRL
// wrote refreshed CRLs through it until the CRL-arbitration round replaced
// that write with UpdateCRLIfCurrent's guarded form below -- a full-row
// Save from a stale snapshot could clobber a concurrent writer's committed
// row (a second generator's CRL, or a future round's Status transition).
// What remains: callers that own the row exclusively. The module's own
// tests seed revoked-authority rows through it (revocation_test.go,
// ca_test.go), the one precedent that exists today.
func (r *AuthorityRepository) Update(ctx context.Context, authority *Authority) error {
	return r.db.WithContext(ctx).Save(authority).Error
}

// UpdateCRLIfCurrent persists one freshly generated CRL -- the PEM document
// crlPEM numbered crlNumber, issued at issuedAt and current until nextUpdate
// -- onto the authority id in ONE guarded statement matching only a row
// whose CRLNumber is still expectedNumber, reporting (true, nil) when THIS
// call's write landed and (false, nil) when the row exists but its
// CRLNumber has advanced past expectedNumber (a concurrent
// CAService.GenerateCRL committed a higher-numbered CRL between this
// caller's read and its write).
//
// The crl_number guard is what makes concurrent CRL generation converge
// instead of last-writer-wins -- the same database-arbitrated idiom
// RevokeIfActive uses for the certificate-row transition. Two overlapping
// GenerateCRL calls can both read CRLNumber N, but only the first
// UPDATE ... WHERE crl_number = N matches and commits, so a loser can never
// persist its own snapshot -- and its own nextNumber N+1 -- over the
// winner's committed document and register. GenerateCRL (crl.go) re-reads
// and regenerates on a (false, nil) answer, so the loser's revocation
// snapshot is folded into a fresh, higher-numbered document rather than
// silently dropped: every successful GenerateCRL call advances the register
// by exactly one, however many calls overlap.
//
// The statement writes ONLY the four CRL columns (plus the updated_at
// auto-update timestamp), never a full-row Save: a caller whose snapshot is
// stale must not resurrect its old Status/RevokedAt/RevocationReason values
// over a concurrent writer's committed row -- the exact blind-save
// disagreement RevokeIfActive's own guard exists to prevent on
// pki_certificates, which a CRL write racing a revocation would otherwise
// reintroduce on pki_authorities.
//
// RowsAffected == 0 deliberately does not distinguish "id does not exist"
// from "CRLNumber moved": callers are expected to have loaded the row
// through FindByID first, which answers the former with ErrAuthorityNotFound.
func (r *AuthorityRepository) UpdateCRLIfCurrent(ctx context.Context, id string, expectedNumber, crlNumber int64, crlPEM string, issuedAt, nextUpdate time.Time) (bool, error) {
	// A struct (not a map) as the Updates argument, with no .Model() call:
	// GORM infers the table from the struct type and writes only the four
	// non-zero CRL fields -- the same partial-update shape PromoteToActive
	// documents, clear of the raw-GORM-bypass entry point
	// (tools/semgrep_rules/raw-gorm-bypass.yml).
	res := r.db.WithContext(ctx).
		Where("id = ? AND crl_number = ?", id, expectedNumber).
		Updates(&Authority{
			CRLNumber:     crlNumber,
			CRLPEM:        crlPEM,
			CRLIssuedAt:   &issuedAt,
			CRLNextUpdate: &nextUpdate,
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// ListAll returns every authority, in no particular order. Round 3's
// addition, for CAService.RegenerateAllCRLs (crl.go): the periodic CRL
// job has no per-tenant or per-purpose scope to iterate -- pki_authorities
// is platform data, and every authority's CRL is refreshed on the same
// schedule -- so it needs the full set rather than a filtered query.
func (r *AuthorityRepository) ListAll(ctx context.Context) ([]Authority, error) {
	var authorities []Authority
	err := r.db.WithContext(ctx).Find(&authorities).Error
	return authorities, err
}

// CertificateRepository is the tenant-scoped accessor for pki_certificates.
// Certificate is tenant data, so this embeds dbkit.Repository[Certificate]
// and inherits all three tenant-isolation layers, exactly like every other
// tenant-owned repository in this codebase.
//
// RevokeIfActive, the one statement Repository[T]'s minimal surface cannot
// express, is composed on the same *gorm.DB through dbkit.WithTenantSession
// -- the identical dual shape go/notification's VerifiedContactRepository
// documents for its own conditional status transitions: db is the same
// connection the embedded Repository was built on, kept so a guarded
// UPDATE runs with the isolation plugin's tenant filter and
// WithTenantSession's RLS session variable engaged. Nothing here reaches
// for db.Table, db.Model or db.Raw, and nothing hand-writes a tenant_id
// filter.
type CertificateRepository struct {
	*dbkit.Repository[Certificate]

	// db is the same connection the embedded Repository was built on, kept
	// so RevokeIfActive's guarded conditional UPDATE can be composed on it.
	// Every use routes through WithTenantSession against a TenantScoped
	// destination, exactly as go/notification's VerifiedContactRepository
	// documents for its own identical field -- never a raw query of any
	// other shape.
	db *gorm.DB
}

// NewCertificateRepository returns a CertificateRepository over db. db is
// expected to come from dbkit.Open (for isolation layer 1 underneath).
func NewCertificateRepository(db *gorm.DB) *CertificateRepository {
	return &CertificateRepository{
		Repository: dbkit.NewRepository[Certificate](db),
		db:         db,
	}
}

// RevokeIfActive transitions the certificate id to
// CertificateStatusRevoked in one guarded statement -- the certificate-row
// half of CAService.RevokeCertificate's two-statement transition
// (revocation.go), mirroring SigningKeyRepository.Revoke's identical
// guarded shape -- reporting (true, nil) when THIS call performed the
// transition and (false, nil) when the row exists but is not currently
// CertificateStatusActive (it was already revoked, by this call or a
// concurrent one).
//
// The status guard is what makes concurrent revokes converge instead of
// last-writer-wins: a caller whose FindByID saw an active row but whose
// UPDATE matches zero rows has lost the certificate-row arbitration to a
// concurrent caller, and writing its own reason and timestamp over the
// winner's committed row would be exactly the blind-save disagreement the
// round's follow-up review found (see go/pki/AGENTS.md's round entry).
// RowsAffected == 0 sends RevokeCertificate into its re-read-and-supplement
// path instead.
//
// The statement runs inside dbkit.WithTenantSession, like every
// dbkit.Repository[T] method, so the PostgreSQL RLS session variable is
// set for it; the tenant filter itself is injected by dbkit.Open's
// isolation plugin on the update callback, the same layer org's
// deleteSubtree and notification's conditional status transitions rely on
// (their doc comments say so explicitly) -- which is why db must come from
// dbkit.Open, the expectation NewCertificateRepository already documents.
// Nothing here hand-writes a tenant_id filter.
//
// RowsAffected == 0 deliberately does not distinguish "id does not exist"
// from "id exists but is already revoked": callers are expected to have
// loaded the row through FindByID first, which answers the former with
// ErrRecordNotFound.
func (r *CertificateRepository) RevokeIfActive(ctx context.Context, id, reason string, now time.Time) (bool, error) {
	var moved bool
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		res := tx.
			Where("id = ? AND status = ?", id, CertificateStatusActive).
			Updates(&Certificate{
				Status:           CertificateStatusRevoked,
				RevokedAt:        &now,
				RevocationReason: reason,
			})
		if res.Error != nil {
			return res.Error
		}
		moved = res.RowsAffected > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return moved, nil
}

// CertificateRevocationRepository is the plain, non-tenant-scoped accessor
// for pki_certificate_revocations. CertificateRevocation is platform data
// (see its own model.go doc comment for the full "why this table exists at
// all" argument), so this wraps a bare *gorm.DB, the identical shape
// SigningKeyRepository and AuthorityRepository use. Round 3's addition.
type CertificateRevocationRepository struct {
	db *gorm.DB
}

// NewCertificateRevocationRepository returns a
// CertificateRevocationRepository over db.
func NewCertificateRevocationRepository(db *gorm.DB) *CertificateRevocationRepository {
	return &CertificateRevocationRepository{db: db}
}

// Create inserts rev unconditionally -- the plain single-row write, for a
// caller that asserts no row for rev.CertificateID exists yet, or that
// deliberately wants a duplicate to FAIL as a unique-constraint error
// (migration 0008's uq_pki_certificate_revocations_certificate index
// refuses it). The certificate revocation path itself never calls this:
// CAService.RevokeCertificate uses InsertIfAbsent instead, whose ON
// CONFLICT no-op turns that same index into an arbitration verdict rather
// than an error (see InsertIfAbsent's own doc comment).
func (r *CertificateRevocationRepository) Create(ctx context.Context, rev *CertificateRevocation) error {
	return r.db.WithContext(ctx).Create(rev).Error
}

// InsertIfAbsent inserts rev unless a ledger row for rev.CertificateID
// already exists, reporting (true, nil) when THIS call inserted the row and
// (false, nil) when the row was already present (the insert was a no-op).
//
// The no-op is expressed as INSERT ... ON CONFLICT (certificate_id) DO
// NOTHING with a RowsAffected verdict -- dialect-neutral SQLite and
// PostgreSQL DDL, backed by migration 0008's
// uq_pki_certificate_revocations_certificate unique index -- never as a
// check-then-act read, which two concurrent callers could both pass. That
// database-arbitrated single-winner shape is what
// CAService.RevokeCertificate (revocation.go) builds its revocation
// transition on: of any number of racing calls for one certificate, exactly
// one insert wins, and only the winner publishes EventCertificateRevoked.
func (r *CertificateRevocationRepository) InsertIfAbsent(ctx context.Context, rev *CertificateRevocation) (bool, error) {
	res := r.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "certificate_id"}},
			DoNothing: true,
		}).
		Create(rev)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// ListByAuthority returns every revocation ledger entry for authorityID, in
// no particular order -- the query CAService.GenerateCRL reads to build one
// authority's revoked-certificate list, through
// idx_pki_certificate_revocations_authority_id.
func (r *CertificateRevocationRepository) ListByAuthority(ctx context.Context, authorityID string) ([]CertificateRevocation, error) {
	var revocations []CertificateRevocation
	err := r.db.WithContext(ctx).Where("authority_id = ?", authorityID).Find(&revocations).Error
	return revocations, err
}
