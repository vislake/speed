package pki

import (
	"context"
	"crypto/x509/pkix"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// --- Service.RevokeSigningKey ------------------------------------------------

func TestService_RevokeSigningKey_TransitionsToRevoked(t *testing.T) {
	svc, rec := newTestServiceWithClock(t)
	svc.bus.Subscribe(EventSigningKeyRevoked, rec.record)
	ctx := context.Background()

	if err := svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); err != nil {
		t.Fatalf("EnsurePurpose: %v", err)
	}
	kid, _, _, err := svc.ActiveSigner(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("ActiveSigner: %v", err)
	}

	changed, err := svc.RevokeSigningKey(ctx, kid, "compromised")
	if err != nil {
		t.Fatalf("RevokeSigningKey: %v", err)
	}
	if !changed {
		t.Errorf("RevokeSigningKey(first call) changed = false, want true")
	}

	key, err := svc.signingKeys.FindByID(ctx, kid)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if key.Status != SigningKeyStatusRevoked {
		t.Errorf("Status = %q, want %q", key.Status, SigningKeyStatusRevoked)
	}
	if key.RevokedAt == nil {
		t.Error("RevokedAt is nil, want set")
	}
	if key.RevocationReason != "compromised" {
		t.Errorf("RevocationReason = %q, want %q", key.RevocationReason, "compromised")
	}

	// rec is also subscribed to EventSigningKeyActivated (newTestServiceWithClock),
	// which the preceding EnsurePurpose call already fired once -- count only
	// the revoked-type events this call itself is responsible for.
	revokedCount := 0
	for _, evt := range rec.events {
		if evt.Type == EventSigningKeyRevoked {
			revokedCount++
		}
	}
	if revokedCount != 1 {
		t.Errorf("published events = %v, want exactly one EventSigningKeyRevoked", rec.typesOf())
	}
}

func TestService_RevokeSigningKey_IsIdempotent(t *testing.T) {
	svc, rec := newTestServiceWithClock(t)
	svc.bus.Subscribe(EventSigningKeyRevoked, rec.record)
	ctx := context.Background()

	if err := svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); err != nil {
		t.Fatalf("EnsurePurpose: %v", err)
	}
	kid, _, _, err := svc.ActiveSigner(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("ActiveSigner: %v", err)
	}

	if _, err = svc.RevokeSigningKey(ctx, kid, "compromised"); err != nil {
		t.Fatalf("RevokeSigningKey(first): %v", err)
	}
	changed, err := svc.RevokeSigningKey(ctx, kid, "compromised again")
	if err != nil {
		t.Fatalf("RevokeSigningKey(second): %v", err)
	}
	if changed {
		t.Errorf("RevokeSigningKey(already revoked) changed = true, want false")
	}
	revokedCount := 0
	for _, evt := range rec.events {
		if evt.Type == EventSigningKeyRevoked {
			revokedCount++
		}
	}
	if revokedCount != 1 {
		t.Errorf("published %d EventSigningKeyRevoked across two revoke calls, want exactly 1 (idempotent no-op publishes nothing)", revokedCount)
	}

	key, err := svc.signingKeys.FindByID(ctx, kid)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if key.RevocationReason != "compromised" {
		t.Errorf("RevocationReason = %q after a no-op second call, want the FIRST call's reason %q unchanged", key.RevocationReason, "compromised")
	}
}

func TestService_RevokeSigningKey_UnknownKID(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.RevokeSigningKey(context.Background(), "does-not-exist", "reason"); !apperrIs(err, ErrKeyNotFound) {
		t.Errorf("RevokeSigningKey(unknown kid) error = %v, want ErrKeyNotFound", err)
	}
}

// TestService_RevokeSigningKey_InvalidatesTheCacheAndExcludesFromReads is the
// round's central proof: a revoked key must be immediately excluded from
// ActiveSigner and VerificationKeys, THROUGH the same cache-invalidation
// mechanism round 2 built (the process-local keySetCache, cache.go),
// exercised here with a non-trivial cache TTL so a stale cache entry would
// actually be observable if invalidation were bypassed.
func TestService_RevokeSigningKey_InvalidatesTheCacheAndExcludesFromReads(t *testing.T) {
	db := newTestDB(t)
	signer := NewLocalSigner(db)
	svc := NewService(signer, "local", NewSigningKeyRepository(db), time.Hour, DefaultPropagationWindow, DefaultRenewalLeadTime)
	t.Cleanup(func() { _ = svc.Close() })
	ctx := context.Background()

	if err := svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); err != nil {
		t.Fatalf("EnsurePurpose: %v", err)
	}
	kid, _, _, err := svc.ActiveSigner(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("ActiveSigner (before revoke): %v", err)
	}

	// Populate the cache for this purpose -- a call to VerificationKeys is
	// enough to load and cache the key set with the hour-long TTL above, so
	// a bypassed invalidation would leave this exact call's result stale.
	if _, err = svc.VerificationKeys(ctx, "authn.access_token"); err != nil {
		t.Fatalf("VerificationKeys (before revoke): %v", err)
	}

	if _, err = svc.RevokeSigningKey(ctx, kid, "incident response"); err != nil {
		t.Fatalf("RevokeSigningKey: %v", err)
	}

	// ActiveSigner must now report ErrNoActiveKey -- the SAME answer it
	// would give if EnsurePurpose had never been called -- because the
	// long-TTL cache was invalidated, not merely because the TTL happened
	// to expire (it has not: this test runs in well under an hour).
	if _, _, _, err = svc.ActiveSigner(ctx, "authn.access_token"); !apperrIs(err, ErrNoActiveKey) {
		t.Errorf("ActiveSigner(after revoke) error = %v, want ErrNoActiveKey", err)
	}

	keys, err := svc.VerificationKeys(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("VerificationKeys(after revoke): %v", err)
	}
	for _, k := range keys {
		if k.KID == kid {
			t.Errorf("VerificationKeys(after revoke) still includes the revoked kid %q", kid)
		}
	}
}

// --- CAService.RevokeCertificate ---------------------------------------------

func TestCAService_RevokeCertificate_TransitionsToRevoked(t *testing.T) {
	ca, rec := newTestCAServiceWithBus(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	_, cert := issueTestCertificate(t, ca, ctx)

	changed, err := ca.RevokeCertificate(ctx, cert.ID, "compromised")
	if err != nil {
		t.Fatalf("RevokeCertificate: %v", err)
	}
	if !changed {
		t.Errorf("RevokeCertificate(first call) changed = false, want true")
	}

	got, err := ca.certificates.FindByID(ctx, cert.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.Status != CertificateStatusRevoked {
		t.Errorf("Status = %q, want %q", got.Status, CertificateStatusRevoked)
	}
	if got.RevokedAt == nil {
		t.Error("RevokedAt is nil, want set")
	}

	if len(rec.events) != 1 || rec.events[0].Type != EventCertificateRevoked {
		t.Errorf("published events = %v, want exactly one EventCertificateRevoked", rec.typesOf())
	}
}

func TestCAService_RevokeCertificate_IsIdempotent(t *testing.T) {
	ca, _ := newTestCAServiceWithBus(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	_, cert := issueTestCertificate(t, ca, ctx)

	if _, err := ca.RevokeCertificate(ctx, cert.ID, "first"); err != nil {
		t.Fatalf("RevokeCertificate(first): %v", err)
	}
	changed, err := ca.RevokeCertificate(ctx, cert.ID, "second")
	if err != nil {
		t.Fatalf("RevokeCertificate(second): %v", err)
	}
	if changed {
		t.Errorf("RevokeCertificate(already revoked) changed = true, want false")
	}

	revocations, err := ca.revocations.ListByAuthority(ctx, cert.AuthorityID)
	if err != nil {
		t.Fatalf("ListByAuthority: %v", err)
	}
	if len(revocations) != 1 {
		t.Errorf("revocation ledger has %d rows after two revoke calls, want exactly 1", len(revocations))
	}
}

func TestCAService_RevokeCertificate_WritesRevocationLedgerEntry(t *testing.T) {
	ca, _ := newTestCAServiceWithBus(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	_, cert := issueTestCertificate(t, ca, ctx)

	if _, err := ca.RevokeCertificate(ctx, cert.ID, "key compromise"); err != nil {
		t.Fatalf("RevokeCertificate: %v", err)
	}

	revocations, err := ca.revocations.ListByAuthority(ctx, cert.AuthorityID)
	if err != nil {
		t.Fatalf("ListByAuthority: %v", err)
	}
	if len(revocations) != 1 {
		t.Fatalf("ListByAuthority = %d rows, want 1", len(revocations))
	}
	rev := revocations[0]
	if rev.CertificateID != cert.ID {
		t.Errorf("CertificateID = %q, want %q", rev.CertificateID, cert.ID)
	}
	if rev.Serial != cert.Serial {
		t.Errorf("Serial = %q, want %q", rev.Serial, cert.Serial)
	}
	if rev.TenantID != "tenant-acme" {
		t.Errorf("TenantID = %q, want %q", rev.TenantID, "tenant-acme")
	}
	if rev.RevocationReason != "key compromise" {
		t.Errorf("RevocationReason = %q, want %q", rev.RevocationReason, "key compromise")
	}
}

func TestCAService_RevokeCertificate_NotFound(t *testing.T) {
	ca := newTestCAService(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))
	if _, err := ca.RevokeCertificate(ctx, "does-not-exist", "reason"); err == nil {
		t.Fatalf("RevokeCertificate(missing certificate) succeeded, want an error")
	}
}

// --- CAService.VerifyCertificate ---------------------------------------------

func TestCAService_VerifyCertificate_ValidChain_Succeeds(t *testing.T) {
	ca := newTestCAService(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	_, cert := issueTestCertificate(t, ca, ctx)

	leaf, err := ca.VerifyCertificate(ctx, cert.ID)
	if err != nil {
		t.Fatalf("VerifyCertificate: %v", err)
	}
	if leaf == nil {
		t.Fatal("VerifyCertificate returned a nil leaf on success")
	}
}

func TestCAService_VerifyCertificate_RevokedCertificate_Refused(t *testing.T) {
	ca := newTestCAService(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	_, cert := issueTestCertificate(t, ca, ctx)
	if _, err := ca.RevokeCertificate(ctx, cert.ID, "compromised"); err != nil {
		t.Fatalf("RevokeCertificate: %v", err)
	}

	if _, err := ca.VerifyCertificate(ctx, cert.ID); !apperrIs(err, ErrCertificateRevoked) {
		t.Errorf("VerifyCertificate(revoked certificate) error = %v, want ErrCertificateRevoked", err)
	}
}

// TestCAService_VerifyCertificate_RevokedAuthorityInChain_Refused proves the
// chain-verification path defends against AuthorityStatusRevoked even
// though no method in this round's own public API ever writes it -- see
// VerifyCertificate's own doc comment. The row is seeded directly, the same
// precedent round 1's TestService_VerificationKeys_ReturnsNonRevokedKeys
// sets for SigningKeyStatusRevoked.
func TestCAService_VerifyCertificate_RevokedAuthorityInChain_Refused(t *testing.T) {
	ca := newTestCAService(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	authority, cert := issueTestCertificate(t, ca, ctx)

	// Seed the issuing authority as revoked directly through the
	// repository -- no public CAService method ever performs this
	// transition this round.
	authority.Status = AuthorityStatusRevoked
	if err := ca.authorities.Update(ctx, authority); err != nil {
		t.Fatalf("seed revoked authority: %v", err)
	}

	if _, err := ca.VerifyCertificate(ctx, cert.ID); !apperrIs(err, ErrCertificateRevoked) {
		t.Errorf("VerifyCertificate(revoked issuing authority) error = %v, want ErrCertificateRevoked", err)
	}
}

func TestCAService_VerifyCertificate_CertificateNotFound(t *testing.T) {
	ca := newTestCAService(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))
	if _, err := ca.VerifyCertificate(ctx, "does-not-exist"); err == nil {
		t.Fatalf("VerifyCertificate(missing certificate) succeeded, want an error")
	}
}

// --- test helpers -------------------------------------------------------

// newTestCAServiceWithBus returns a CAService like newTestCAService (ca_test.go),
// but with a real in-memory EventBus wired directly and a recorder already
// subscribed to EventCertificateRevoked -- mirroring
// newTestServiceWithClock's identical shape for Service.
func newTestCAServiceWithBus(t *testing.T) (*CAService, *eventRecorder) {
	t.Helper()
	ca := newTestCAService(t)
	rec := newEventRecorder()
	ca.bus = pkgcore.NewMemoryEventBus()
	ca.bus.Subscribe(EventCertificateRevoked, rec.record)
	return ca, rec
}

// issueTestCertificate builds a full root -> intermediate -> end-entity
// chain and returns the issuing (intermediate) authority and the issued
// certificate -- the shared fixture every revocation/verification test in
// this file needs, since VerifyCertificate walks a real chain.
func issueTestCertificate(t *testing.T, ca *CAService, ctx context.Context) (*Authority, *Certificate) {
	t.Helper()
	root, err := ca.CreateRootCA(ctx, RootCAParams{
		Subject:  pkix.Name{CommonName: "speed Root CA"},
		NotAfter: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRootCA: %v", err)
	}
	intermediate, err := ca.CreateIntermediateCA(ctx, root.ID, IntermediateCAParams{
		Subject:  pkix.Name{CommonName: "speed Intermediate CA"},
		NotAfter: time.Now().Add(12 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateIntermediateCA: %v", err)
	}
	cert, err := ca.IssueCertificate(ctx, intermediate.ID, CertificateParams{
		Purpose:  "tenant.jwt_signing",
		Subject:  pkix.Name{CommonName: "tenant leaf"},
		NotAfter: time.Now().Add(1 * time.Hour),
	})
	if err != nil {
		t.Fatalf("IssueCertificate: %v", err)
	}
	return intermediate, cert
}
