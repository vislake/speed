package pki

import (
	"context"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// RevokeSigningKey transitions the signing key kid to
// SigningKeyStatusRevoked, of any prior status (pending, active or
// retiring), and publishes EventSigningKeyRevoked -- round 3's addition,
// docs/internal/22-pki.md's "revocation" section: "revoked keys immediately
// reject signing".
//
// Idempotent: revoking an already-revoked key reports (false, nil) and does
// nothing further -- no second cache invalidation, no second event -- the
// same idempotent-no-op shape SigningKeyRepository.Revoke itself documents.
// A caller that needs to know whether THIS call performed the transition
// reads the returned bool; ErrKeyNotFound if kid does not exist at all.
//
// # Immediate exclusion from ActiveSigner and VerificationKeys
//
// This method does not filter Service's own reads directly -- it relies
// entirely on SigningKeyRepository.ListVerifiableByPurpose (repository.go)
// already excluding SigningKeyStatusRevoked from its status set, the same
// query keySet (service.go) loads on every cache miss. Once the affected
// purpose's cache entry is invalidated below, the next ActiveSigner or
// VerificationKeys call reloads from the database and the revoked row is
// simply absent from what comes back -- ActiveSigner then reports
// ErrNoActiveKey if the revoked key was the purpose's active one, exactly
// as if EnsurePurpose had never run for it.
//
// # Cache invalidation reuses the SAME mechanism round 2 built
//
// The task this method fulfills is explicit that revocation "must reuse
// [the key-set cache's] existing event-invalidation mechanism, [and] not
// bypass it with a second cache-clearing path". This method satisfies that
// by publishing EventSigningKeyRevoked on the SAME pkgcore.EventBus every
// other signing-key lifecycle transition publishes on, through attachBus's
// SAME subscription (service.go) that already invalidates cache.go's
// keySetCache for staged/activated/retired -- attachBus now additionally
// subscribes to EventSigningKeyRevoked, so a revoke on one replica
// converges every other replica's cache through the identical bus fan-out,
// never a second, parallel invalidation call. The s.cache.invalidate call
// below is this replica's OWN local application of that exact mechanism
// (the in-memory bus delivers synchronously, so the local write is visible
// before this call returns), not a second path -- see
// onSigningKeyLifecycleEvent in service.go, which is what a REMOTE
// replica's identical event runs through.
func (s *Service) RevokeSigningKey(ctx context.Context, kid, reason string) (bool, error) {
	key, err := s.signingKeys.FindByID(ctx, kid)
	if err != nil {
		return false, err
	}

	now := s.now().UTC()
	changed, err := s.signingKeys.Revoke(ctx, kid, reason, now)
	if err != nil {
		return false, err
	}
	if !changed {
		// Already revoked -- idempotent no-op, matching
		// SigningKeyRepository.Revoke's own documented contract.
		return false, nil
	}

	s.cache.invalidate(key.Purpose)

	observability.FromContext(ctx).Info("pki signing key revoked",
		"kid", kid,
		"purpose", key.Purpose,
	)
	s.publish(ctx, pkgcore.Event{
		Type: EventSigningKeyRevoked,
		Payload: SigningKeyLifecycleEvent{
			Purpose:    key.Purpose,
			KID:        kid,
			OccurredAt: now,
		},
	})
	return true, nil
}

// RevokeCertificate transitions certificateID's certificate, in the
// caller's ctx tenant, to CertificateStatusRevoked, records a
// CertificateRevocation ledger entry (model.go) and publishes
// EventCertificateRevoked -- round 3's addition.
//
// Idempotent: revoking an already-revoked certificate reports (false, nil)
// without writing a second ledger row or publishing a second event, the
// identical shape RevokeSigningKey documents above. ErrRecordNotFound
// (dbkit's own, via CertificateRepository.FindByID) when certificateID does
// not name a certificate of ctx's tenant.
//
// The ledger write is a second, non-atomic statement after the certificate
// update -- see CertificateRevocation's own model.go doc comment for the
// full "why this is safe" argument. A failure there is logged, not
// returned: the certificate itself is by then correctly revoked (the
// record CAService.VerifyCertificate actually trusts), and the accepted
// risk is a CRL that omits this one serial until the ledger write is
// retried or the row is reconstructed -- never an incorrect "not revoked"
// answer, matching notes.recordNoteCreatedAudit's identical
// log-not-return choice for its own post-commit side write.
func (s *CAService) RevokeCertificate(ctx context.Context, certificateID, reason string) (bool, error) {
	cert, err := s.certificates.FindByID(ctx, certificateID)
	if err != nil {
		return false, err
	}
	if cert.Status == CertificateStatusRevoked {
		return false, nil
	}

	now := time.Now().UTC()
	cert.Status = CertificateStatusRevoked
	cert.RevokedAt = &now
	cert.RevocationReason = reason
	if err := s.certificates.Update(ctx, cert); err != nil {
		return false, fmt.Errorf("pki: revoke certificate %q: %w", certificateID, err)
	}

	tenant := cert.GetTenantID()
	if err := s.revocations.Create(ctx, &CertificateRevocation{
		ID:               uuid.NewString(),
		CertificateID:    cert.ID,
		AuthorityID:      cert.AuthorityID,
		Serial:           cert.Serial,
		TenantID:         string(tenant),
		RevokedAt:        now,
		RevocationReason: reason,
	}); err != nil {
		observability.FromContext(ctx).Error("pki certificate revocation ledger write failed",
			"certificate_id", cert.ID,
			"authority_id", cert.AuthorityID,
			"error", err,
		)
	}

	observability.FromContext(ctx).Info("pki certificate revoked",
		"certificate_id", cert.ID,
		"authority_id", cert.AuthorityID,
	)
	s.publish(ctx, pkgcore.Event{
		Type: EventCertificateRevoked,
		Payload: CertificateRevokedEvent{
			TenantID:         string(tenant),
			CertificateID:    cert.ID,
			AuthorityID:      cert.AuthorityID,
			Serial:           cert.Serial,
			RevocationReason: reason,
			OccurredAt:       now,
		},
	})
	return true, nil
}

// VerifyCertificate verifies certificateID's certificate (in the caller's
// ctx tenant) against its full issuing chain, up to and including a
// self-signed root, and returns the parsed leaf on success -- round 3's
// addition, the "revoked certificates fail chain verification" half of
// docs/internal/22-pki.md's "revocation" section.
//
// It refuses with ErrCertificateRevoked, before any cryptographic
// verification runs, when:
//
//   - the certificate itself is CertificateStatusRevoked, or
//   - any authority in its chain (its direct issuer, or that issuer's own
//     issuer, up to the root) is AuthorityStatusRevoked.
//
// The second case defends a status value this round's own public API never
// writes -- AGENTS.md's Known limitations and model.go's own
// AuthorityStatus doc comment both record that no method here ever sets
// AuthorityStatusRevoked -- but the chain-verification path checks it
// anyway, so a future round that DOES add authority revocation (or a row
// seeded directly, as this round's own tests do) is correctly refused from
// day one, never silently trusted because "nothing sets this yet" quietly
// became "nothing here needs to check it".
//
// ErrAuthorityNotFound if the chain names an authority id that does not
// exist (a data-integrity fault, not a normal-operation case). Any other
// cryptographic verification failure (expired, malformed, signature
// mismatch) is an unwrapped error, matching this file's and ca.go's own
// convention of leaving genuine invariant violations uncoded.
func (s *CAService) VerifyCertificate(ctx context.Context, certificateID string) (*x509.Certificate, error) {
	cert, err := s.certificates.FindByID(ctx, certificateID)
	if err != nil {
		return nil, err
	}
	if cert.Status == CertificateStatusRevoked {
		return nil, ErrCertificateRevoked.WithParam("certificate_id", certificateID)
	}
	leaf, err := parseCertificatePEM(cert.CertificatePEM)
	if err != nil {
		return nil, fmt.Errorf("pki: parse certificate %q: %w", certificateID, err)
	}

	roots := x509.NewCertPool()
	intermediates := x509.NewCertPool()

	// Walk the chain from the issuing authority up to the root, cycle-
	// guarded the same way ExportAuthorityChainJWKS (jwks.go) is: ParentID
	// values are application-generated and this round adds no constraint
	// preventing a corrupt cycle, so the loop must not be able to spin
	// forever on one.
	seen := make(map[string]bool)
	authorityID := cert.AuthorityID
	for authorityID != "" {
		if seen[authorityID] {
			return nil, fmt.Errorf("pki: authority chain cycle detected at %q", authorityID)
		}
		seen[authorityID] = true

		authority, err := s.authorities.FindByID(ctx, authorityID)
		if err != nil {
			return nil, err
		}
		if authority.Status == AuthorityStatusRevoked {
			return nil, ErrCertificateRevoked.WithParam("authority_id", authorityID)
		}
		authorityCert, err := parseCertificatePEM(authority.CertificatePEM)
		if err != nil {
			return nil, fmt.Errorf("pki: parse authority %q certificate: %w", authorityID, err)
		}
		if authority.Type == AuthorityTypeRoot {
			roots.AddCert(authorityCert)
		} else {
			intermediates.AddCert(authorityCert)
		}
		if authority.ParentID == nil {
			break
		}
		authorityID = *authority.ParentID
	}

	// ExtKeyUsageAny: this module's own certificates carry no ExtKeyUsage
	// extension at all (ca.go's IssueCertificate template), so the default
	// VerifyOptions{} KeyUsages (ExtKeyUsageServerAuth) would be checking a
	// constraint neither this module nor its callers ever declared.
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		return nil, fmt.Errorf("pki: verify certificate %q: %w", certificateID, err)
	}
	return leaf, nil
}
