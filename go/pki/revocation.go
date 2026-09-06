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
// caller's ctx tenant, to CertificateStatusRevoked, records its single
// CertificateRevocation ledger row (model.go) -- the row CRL generation
// reads -- and publishes EventCertificateRevoked. Round 3's addition,
// reworked by the ledger-atomicity round into the arbitrated form below.
//
// # One winner per certificate: the ledger insert arbitrates
//
// The certificate update and the ledger write are two separate statements
// (CertificateRevocation's own model.go doc comment explains why they
// cannot share one transaction), and the certificate update itself is an
// unguarded full-row save -- dbkit.Repository[T] exposes no conditional
// update without reaching around it. Single-winner semantics come from the
// LEDGER write instead: this call inserts its row through
// CertificateRevocationRepository.InsertIfAbsent (repository.go), an
// INSERT ... ON CONFLICT (certificate_id) DO NOTHING whose RowsAffected
// verdict, enforced by migration 0008's
// uq_pki_certificate_revocations_certificate unique index, names exactly
// one winning call among any number of concurrent revokes of the same
// certificate. Only the winner publishes EventCertificateRevoked.
//
// The returned bool reports whether THIS call won that arbitration --
// whether this call is the invocation that durably recorded the
// revocation: its ledger row inserted, its event published. Exactly one
// concurrent call can report true for a given certificate; every loser
// reports (false, nil) having changed nothing.
//
// # Retry and reconciliation
//
// Revoking an already-revoked certificate is not a bare no-op. Because the
// ledger write is a separate statement that can fail or be lost after the
// certificate update committed, a call that finds the certificate already
// revoked still attempts the ledger insert. When the row is genuinely
// missing -- a lost write -- the call inserts it, publishes the one event
// the failed call could not, and reports true: the retry is the call that
// completes the revocation. When the row already exists the insert no-ops
// and the call reports (false, nil): a pure idempotent re-revoke that
// writes nothing further. The reconstructed row and its event are built
// from the certificate row's own committed fields, so a retry arriving
// with a different reason never rewrites the original revocation's reason
// or time.
//
// # Ledger-write failures are errors, never log-and-forget
//
// A failed ledger insert is RETURNED, wrapped to state the facts a
// retrying caller needs: the certificate is already revoked (that write
// committed), its ledger row is missing, and calling RevokeCertificate
// again reconstructs the row. The old log-and-return-success behavior was
// the bug this round fixes: a revocation whose ledger write failed was
// reported as success, and every later call then found the certificate
// already revoked and returned before ever reaching the ledger write again
// -- a revoked certificate permanently missing from CRLs.
//
// ErrRecordNotFound (dbkit's own, via CertificateRepository.FindByID) when
// certificateID does not name a certificate of ctx's tenant.
func (s *CAService) RevokeCertificate(ctx context.Context, certificateID, reason string) (bool, error) {
	cert, err := s.certificates.FindByID(ctx, certificateID)
	if err != nil {
		return false, err
	}

	if cert.Status != CertificateStatusRevoked {
		now := time.Now().UTC()
		cert.Status = CertificateStatusRevoked
		cert.RevokedAt = &now
		cert.RevocationReason = reason
		if err = s.certificates.Update(ctx, cert); err != nil {
			return false, fmt.Errorf("pki: revoke certificate %q: %w", certificateID, err)
		}
	}
	if cert.RevokedAt == nil {
		// Status and RevokedAt are always written together by this method's
		// own transition above, so a row arriving here already revoked but
		// timestamp-less was revoked outside it; refusing beats fabricating
		// a ledger time for a revocation this method cannot date.
		return false, fmt.Errorf("pki: revoke certificate %q: certificate is already revoked but has no RevokedAt, its revocation ledger entry cannot be reconstructed", certificateID)
	}

	// The ledger row is built from the certificate row's fields -- its
	// serial, authority, tenant and the committed RevokedAt and
	// RevocationReason -- never from this call's reason argument alone, so
	// the certificate row and its ledger row can never disagree about when
	// or why the revocation happened.
	inserted, err := s.revocations.InsertIfAbsent(ctx, &CertificateRevocation{
		ID:               uuid.NewString(),
		CertificateID:    cert.ID,
		AuthorityID:      cert.AuthorityID,
		Serial:           cert.Serial,
		TenantID:         string(cert.GetTenantID()),
		RevokedAt:        *cert.RevokedAt,
		RevocationReason: cert.RevocationReason,
	})
	if err != nil {
		return false, fmt.Errorf("pki: revoke certificate %q: certificate is revoked but its revocation ledger row could not be recorded -- retry RevokeCertificate to reconstruct the row: %w", certificateID, err)
	}
	if !inserted {
		// Another call's insert won the arbitration for this certificate;
		// that winner publishes the event. This call changed nothing.
		return false, nil
	}

	// Row-then-event: the event is published only by the call whose insert
	// landed the ledger row, so a subscriber never reads an event whose row
	// does not exist, and a revocation is announced exactly once however
	// many times it is retried.
	observability.FromContext(ctx).Info("pki certificate revoked",
		"certificate_id", cert.ID,
		"authority_id", cert.AuthorityID,
	)
	s.publish(ctx, pkgcore.Event{
		Type: EventCertificateRevoked,
		Payload: CertificateRevokedEvent{
			TenantID:         string(cert.GetTenantID()),
			CertificateID:    cert.ID,
			AuthorityID:      cert.AuthorityID,
			Serial:           cert.Serial,
			RevocationReason: cert.RevocationReason,
			OccurredAt:       *cert.RevokedAt,
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
