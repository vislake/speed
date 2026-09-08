package attestation

import (
	"context"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pki"
)

// The app's CA chain and attestation certificate parameters. The root and
// the issuing intermediate are the platform's own authorities, created
// once per database by EnsureAuthorityChain and found again by these fixed
// subject names on every later boot; the intermediate's certificates are
// the ones this package issues every tenant's "simulation.attestation"
// end-entity certificate under.
const (
	attestationRootSubjectCN         = "speed Reference App Root CA"
	attestationIntermediateSubjectCN = "speed Reference App Simulation CA"
	attestationCertificatePurpose    = "simulation.attestation"
	attestationRootLifetime          = 10 * 365 * 24 * time.Hour
	attestationIssuerLifetime        = 5 * 365 * 24 * time.Hour
	attestationCertificateLifetime   = 365 * 24 * time.Hour
)

// MaxAttestedBytes is the largest object this package attests (and the
// gate serves through an attestation): 20 MiB, mirroring the photo serve
// bound cmd/server's own surfaces already enforce (cases_photos.go's
// maxPhotoBytes) -- an honest simulation output stays far below it, and a
// larger object is refused rather than buffered in full. An attestation
// requires the whole content in memory twice (once to digest at signing,
// once at the gate's serve-time digest), which is exactly why the bound
// exists. Exported so cmd/server's gate read (sharing_resolver.go) caps
// itself with the very same bound the signing path attests under.
const MaxAttestedBytes = 20 << 20

// Service attests smile-simulation outputs through go/pki's X.509 layer
// and gates their public shares on the attestation -- the reference app's
// real consumer of the layer (doc.go's package comment has the full
// account).
//
// The zero value is not ready to use; construct one with NewService. A
// Service is usable only after its two boot steps have run, in order:
// EnsureSchema (the app table) and EnsureAuthorityChain (the app's CA
// chain in pki_authorities, once per database); cmd/server runs both at
// every boot, right after Kernel.Bootstrap applied pki's migrations.
type Service struct {
	db *gorm.DB

	// chain is the pki CAService every certificate this package issues is
	// signed through, and every verification and signing operation runs
	// against -- the app never reaches around it with a hand-copied
	// revocation or validity check (doc.go's "What is attested" section).
	chain *pki.CAService

	// certificates reads the tenant's own pki_certificates rows for the
	// issue-vs-reuse decision. It is a read of the app's own rows, never a
	// second enforcement point: the guards themselves live in the pki
	// methods (SignCertificate refuses a revoked row, chain or expired
	// certificate), and a wrong guess here only means the signing call
	// answers the coded refusal and the attestation is retried.
	certificates *pki.CertificateRepository

	// content opens an output's bytes for the owning tenant (content.go) --
	// the storage read both the signing-time digest and the gate's
	// serve-time digest need.
	content ContentOpener

	store *AttestationStore

	// issuingAuthorityID is the id of the app's intermediate authority,
	// chosen by EnsureAuthorityChain. Empty until that boot step ran.
	issuingAuthorityID string
}

// NewService returns a Service that issues and verifies certificates
// through chain, reads its own tenant certificate rows through
// certificates, opens output bytes through content and persists
// attestations in store. Constructing one performs no I/O; call
// EnsureSchema and EnsureAuthorityChain once each, before first use
// (cmd/server's wiring does this at every boot).
func NewService(chain *pki.CAService, certificates *pki.CertificateRepository, content ContentOpener, store *AttestationStore, db *gorm.DB) *Service {
	return &Service{
		chain:        chain,
		certificates: certificates,
		content:      content,
		store:        store,
		db:           db,
	}
}

// EnsureSchema creates the attestations table and its index if they do
// not already exist -- see AttestationStore.EnsureSchema. Call it once,
// before any attestation runs (cmd/server's wiring does this at every
// boot, immediately before EnsureAuthorityChain).
func (s *Service) EnsureSchema(ctx context.Context) error {
	return s.store.EnsureSchema(ctx)
}

// EnsureAuthorityChain makes sure this database carries the app's CA
// chain -- one root authority and one issuing intermediate, both found by
// their fixed subject names (attestationRootSubjectCN /
// attestationIntermediateSubjectCN) -- creating whichever half is missing
// through CAService.CreateRootCA / CreateIntermediateCA, and records the
// intermediate as this Service's issuing authority. Idempotent per
// database: a later boot finds both rows and changes nothing, so a
// restart never mints a second chain. A concurrent first boot of two
// replicas sharing one database can each mint a chain inside the race
// window; every attestation row records the certificate that signed it
// and verification walks that certificate's own chain, so the duplication
// is cosmetic -- recorded in go/pki/AGENTS.md's consumer record.
//
// Authorities are platform rows, so ctx needs no tenant.
func (s *Service) EnsureAuthorityChain(ctx context.Context) error {
	authorities := pki.NewAuthorityRepository(s.db)

	rootID, err := s.findAuthority(ctx, authorities, pki.AuthorityTypeRoot, attestationRootSubjectCN)
	if err != nil {
		return err
	}
	if rootID == "" {
		root, createErr := s.chain.CreateRootCA(ctx, pki.RootCAParams{
			Subject:  pkix.Name{CommonName: attestationRootSubjectCN},
			NotAfter: time.Now().Add(attestationRootLifetime),
		})
		if createErr != nil {
			return fmt.Errorf("attestation: create the app root CA: %w", createErr)
		}
		rootID = root.ID
	}

	intermediateID, err := s.findAuthority(ctx, authorities, pki.AuthorityTypeIntermediate, attestationIntermediateSubjectCN)
	if err != nil {
		return err
	}
	if intermediateID == "" {
		intermediate, createErr := s.chain.CreateIntermediateCA(ctx, rootID, pki.IntermediateCAParams{
			Subject:  pkix.Name{CommonName: attestationIntermediateSubjectCN},
			NotAfter: time.Now().Add(attestationIssuerLifetime),
		})
		if createErr != nil {
			return fmt.Errorf("attestation: create the app intermediate CA: %w", createErr)
		}
		intermediateID = intermediate.ID
	}

	s.issuingAuthorityID = intermediateID
	return nil
}

// findAuthority searches the authorities table for one row of the given
// type whose Subject equals the fixed "CN=<commonName>" rendering of cn.
// The deterministic "lowest CreatedAt, then smallest id" pick keeps every
// replica of a deployment that raced a first boot choosing the same
// pre-existing chain. Returns "" when no row matches.
func (s *Service) findAuthority(ctx context.Context, authorities *pki.AuthorityRepository, authorityType string, commonName string) (string, error) {
	wantSubject := (&pkix.Name{CommonName: commonName}).String()
	all, err := authorities.ListAll(ctx)
	if err != nil {
		return "", fmt.Errorf("attestation: list authorities: %w", err)
	}
	var best *pki.Authority
	for i := range all {
		row := &all[i]
		if row.Type != authorityType || row.Subject != wantSubject {
			continue
		}
		if best == nil || row.CreatedAt.Before(best.CreatedAt) || (row.CreatedAt.Equal(best.CreatedAt) && row.ID < best.ID) {
			best = row
		}
	}
	if best == nil {
		return "", nil
	}
	return best.ID, nil
}

// EnsureAttested makes sure objectID's output -- an AI output of the
// tenant in ctx -- carries a valid attestation, attesting it when it does
// not: the object's bytes are opened under the ctx tenant (an object
// another tenant owns is refused before anything is written) and
// digested, then EnsureAttestedContent applies the write-or-refresh
// rules below. It is the observation hook every succeeded-output read
// drives (cmd/server/smilesim.go); its callers log and swallow its
// error, since a failed attestation leaves the output refused by the
// sharing gate until a later observation retries, never breaks the read
// that drove it.
func (s *Service) EnsureAttested(ctx context.Context, objectID string) error {
	rc, err := s.content.OpenContent(ctx, objectID)
	if err != nil {
		return fmt.Errorf("attestation: open output %q: %w", objectID, err)
	}
	content, readErr := io.ReadAll(io.LimitReader(rc, MaxAttestedBytes+1))
	closeErr := rc.Close()
	if readErr != nil {
		return fmt.Errorf("attestation: read output %q: %w", objectID, readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("attestation: close output %q: %w", objectID, closeErr)
	}
	return s.EnsureAttestedContent(ctx, objectID, content)
}

// EnsureAttestedContent makes sure objectID's output -- an AI output of
// the tenant in ctx -- carries a valid attestation over content (the
// object's live bytes, already read by the caller), attesting it when it
// does not:
//
//   - no attestation row exists: the tenant's current certificate is
//     reused while active and a fresh "simulation.attestation"
//     certificate is issued otherwise, the canonical message (object id,
//     digest, tenant) is signed with the certificate's key through
//     CAService.SignCertificate, and the row is written;
//   - a row exists under an active certificate: a no-op -- repeated
//     observations (every poll, enumeration or content read of a
//     succeeded job) cost one row lookup;
//   - a row exists under a certificate that is no longer usable (revoked,
//     or past its NotAfter): a fresh certificate is issued, the message
//     re-signed under it, and the row replaced -- the recovery path that
//     makes a revoked certificate's outputs shareable again the next time
//     they are observed.
//
// Content over MaxAttestedBytes is refused rather than attested -- an
// honest output stays far below the bound (the constant's own doc
// comment). Issuance needs the app chain bootstrapped: calling this
// before EnsureAuthorityChain has run (or after it failed) is an error,
// never a silent skip.
func (s *Service) EnsureAttestedContent(ctx context.Context, objectID string, content []byte) error {
	if s.issuingAuthorityID == "" {
		return fmt.Errorf("attestation: authority chain not bootstrapped -- run EnsureAuthorityChain before attesting")
	}
	tenant, ok := pkgcore.TenantFromContext(ctx)
	if !ok {
		return pkgcore.ErrNoTenant
	}
	if int64(len(content)) > MaxAttestedBytes {
		return fmt.Errorf("attestation: output %q exceeds the %d-byte attestation bound", objectID, MaxAttestedBytes)
	}

	row, hasRow, err := s.store.getByObject(ctx, objectID)
	if err != nil {
		return fmt.Errorf("attestation: look up existing attestation for object %q: %w", objectID, err)
	}
	if hasRow {
		usable, checkErr := s.certificateUsable(ctx, row.CertificateID)
		if checkErr != nil {
			return fmt.Errorf("attestation: check current certificate %q for object %q: %w", row.CertificateID, objectID, checkErr)
		}
		if usable {
			return nil
		}
	}

	certificateID, err := s.activeCertificateForTenant(ctx)
	if err != nil {
		return err
	}
	message, err := newMessage(objectID, string(tenant), content)
	if err != nil {
		return err
	}
	signature, err := s.chain.SignCertificate(ctx, certificateID, message)
	if err != nil {
		return fmt.Errorf("attestation: sign output %q with certificate %q: %w", objectID, certificateID, err)
	}

	record := &attestationRecord{
		ObjectID:      objectID,
		CertificateID: certificateID,
		Message:       string(message),
		Signature:     hex.EncodeToString(signature),
	}
	if err := s.store.put(ctx, record); err != nil {
		return fmt.Errorf("attestation: store attestation for object %q: %w", objectID, err)
	}
	return nil
}

// activeCertificateForTenant returns the certificate id EnsureAttested
// should sign with: the tenant's current certificate (the one its most
// recent attestation row names) when that row is still usable, otherwise
// a freshly issued one. Issuing through CAService.IssueCertificate runs
// the chain's own revoked-issuer refusal -- the app's intermediate
// signing nothing once revoked -- and the returned certificate is active
// by construction, so a concurrent caller's own row write decides which
// certificate the surviving row names (store.go's put documents the
// last-write-wins arbitration).
func (s *Service) activeCertificateForTenant(ctx context.Context) (string, error) {
	current, hasCurrent, err := s.store.latestCertificate(ctx)
	if err != nil {
		return "", fmt.Errorf("attestation: look up the tenant's current certificate: %w", err)
	}
	if hasCurrent {
		usable, checkErr := s.certificateUsable(ctx, current)
		if checkErr != nil {
			return "", fmt.Errorf("attestation: check current certificate %q: %w", current, checkErr)
		}
		if usable {
			return current, nil
		}
	}

	tenant, ok := pkgcore.TenantFromContext(ctx)
	if !ok {
		return "", pkgcore.ErrNoTenant
	}
	certificate, err := s.chain.IssueCertificate(ctx, s.issuingAuthorityID, pki.CertificateParams{
		Purpose:  attestationCertificatePurpose,
		Subject:  pkix.Name{CommonName: string(tenant) + " smile-simulation attestation"},
		NotAfter: time.Now().Add(attestationCertificateLifetime),
	})
	if err != nil {
		return "", fmt.Errorf("attestation: issue the tenant's simulation-attestation certificate: %w", err)
	}
	return certificate.ID, nil
}

// certificateUsable reports whether the certificate named by id can still
// sign and vouch for this tenant's attestations: the row exists, its
// Status is active (revocation is the terminal state RevokeCertificate
// writes), and its NotAfter has not passed. This is a read of the row for
// the issue-vs-reuse DECISION only; enforcement stays in the pki layer
// (SignCertificate and VerifyCertificate run the same refusals for real
// at every sign and every gate check).
func (s *Service) certificateUsable(ctx context.Context, certificateID string) (bool, error) {
	certificate, err := s.certificates.FindByID(ctx, certificateID)
	if err != nil {
		if errors.Is(err, dbkit.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	if certificate.Status != pki.CertificateStatusActive {
		return false, nil
	}
	if !time.Now().Before(certificate.NotAfter) {
		return false, nil
	}
	return true, nil
}

// CheckContent is the share gate's half of this package: given the live
// content bytes about to be served for objectID under the tenant in ctx,
// it refuses (ErrAttestationFailed, wrapped with the refusing stage) an
// attested output whose attestation does not verify, and passes an object
// with no attestation row -- the uploaded photos and any other plain
// object shares are untouched by this layer. The gate in
// cmd/server/sharing_resolver.go runs it after reading the content, so
// the digest comparison is over the exact bytes the visitor would have
// received.
//
// The refusal stages, all answering the same ErrAttestationFailed: the
// row's certificate fails CAService.VerifyCertificate (revoked, or a
// revoked chain member, or outside its validity window), the signature
// over the stored message does not verify with the leaf's public key, the
// message names another object or tenant, or the live digest differs from
// the attested one.
func (s *Service) CheckContent(ctx context.Context, objectID string, content []byte) error {
	row, hasRow, err := s.store.getByObject(ctx, objectID)
	if err != nil {
		return fmt.Errorf("attestation: look up attestation for object %q: %w", objectID, err)
	}
	if !hasRow {
		return nil
	}

	leaf, err := s.chain.VerifyCertificate(ctx, row.CertificateID)
	if err != nil {
		return fmt.Errorf("attestation: verify certificate %q of object %q: %w", row.CertificateID, objectID, err)
	}
	signature, err := hex.DecodeString(row.Signature)
	if err != nil {
		return fmt.Errorf("%w: stored signature of object %q is not valid hex", ErrAttestationFailed, objectID)
	}
	tenant, ok := pkgcore.TenantFromContext(ctx)
	if !ok {
		return pkgcore.ErrNoTenant
	}
	if err := verifyContent(leaf, []byte(row.Message), signature, objectID, string(tenant), content); err != nil {
		return err
	}
	return nil
}
