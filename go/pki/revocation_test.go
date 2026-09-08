package pki

import (
	"context"
	"crypto/x509/pkix"
	"fmt"
	"sync"
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

// TestService_RevokeSigningKey_InvalidatesTheCacheAndExcludesFromReads is
// the central proof of immediate exclusion: a revoked key must be
// immediately excluded from ActiveSigner and VerificationKeys, THROUGH the
// same cache-invalidation mechanism every lifecycle transition uses (the
// process-local keySetCache, cache.go), exercised here with a non-trivial
// cache TTL so a stale cache entry would actually be observable if
// invalidation were bypassed.
func TestService_RevokeSigningKey_InvalidatesTheCacheAndExcludesFromReads(t *testing.T) {
	db := newTestDB(t)
	signer := NewLocalSigner(db)
	svc := NewService(signer, "local", NewSigningKeyRepository(db), time.Hour, DefaultPropagationWindow, DefaultRenewalLeadTime, DefaultExpiryScanWindow)
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

// TestCAService_RevokeCertificate_IsIdempotent pins the sequential
// idempotent re-revoke contract end to end: a second call on an
// already-revoked certificate reports (false, nil) and leaves both the
// ledger and the event stream exactly as the first call left them: one
// ledger row, one EventCertificateRevoked, and the FIRST call's revocation
// reason unchanged on both the certificate row and the ledger row.
func TestCAService_RevokeCertificate_IsIdempotent(t *testing.T) {
	ca, rec := newTestCAServiceWithBus(t)
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
	if len(rec.events) != 1 {
		t.Errorf("published %d EventCertificateRevoked across two sequential revoke calls, want exactly 1 (an idempotent re-revoke publishes nothing)", len(rec.events))
	}
	if revocations[0].RevocationReason != "first" {
		t.Errorf("ledger row RevocationReason = %q after an idempotent second call, want the first call's reason %q unchanged", revocations[0].RevocationReason, "first")
	}

	got, err := ca.certificates.FindByID(ctx, cert.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.RevocationReason != "first" {
		t.Errorf("certificate row RevocationReason = %q after an idempotent second call, want the first call's reason %q unchanged", got.RevocationReason, "first")
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
// though no method in this module's public API ever writes it -- see
// VerifyCertificate's own doc comment. The row is seeded directly, the same
// precedent TestService_VerificationKeys_ReturnsNonRevokedKeys sets for
// SigningKeyStatusRevoked.
func TestCAService_VerifyCertificate_RevokedAuthorityInChain_Refused(t *testing.T) {
	ca := newTestCAService(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	authority, cert := issueTestCertificate(t, ca, ctx)

	// Seed the issuing authority as revoked directly through the
	// repository -- no public CAService method ever performs this
	// transition.
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

// --- ledger single-winner and honest-failure contract --------------------

// TestCAService_RevokeCertificate_ConcurrentDoubleRevoke_ExactlyOneWinner
// proves the running -> revoked transition is single-winner under concurrent
// RevokeCertificate calls for the same still-active certificate: N racing
// calls must converge on exactly one (true, nil) answer, one ledger row and
// one EventCertificateRevoked, with every loser reporting (false, nil) --
// the property the ledger's UNIQUE(certificate_id) arbitration exists for.
//
// The barrier shape (a closed start channel plus a WaitGroup, no sleeps)
// follows the org and notification concurrency tests' identical rig; the
// trial is repeated because one race is a scheduling accident and twenty-five
// are a property. 8 goroutines race per trial so that several FindByID
// reads routinely complete before the first winner's certificate update
// commits -- the overlap the old check-then-act revoke turned into a second
// ledger row and a second event.
func TestCAService_RevokeCertificate_ConcurrentDoubleRevoke_ExactlyOneWinner(t *testing.T) {
	const (
		goroutines = 8
		trials     = 25
	)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	for trial := 0; trial < trials; trial++ {
		ca, rec := newTestCAServiceWithBus(t)
		_, cert := issueTestCertificate(t, ca, ctx)

		changed := make([]bool, goroutines)
		errs := make([]error, goroutines)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				changed[i], errs[i] = ca.RevokeCertificate(ctx, cert.ID, "compromised")
			}(i)
		}
		close(start)
		wg.Wait()

		winners := 0
		for i, err := range errs {
			if err != nil {
				t.Fatalf("trial %d: RevokeCertificate goroutine %d: %v", trial, i, err)
			}
			if changed[i] {
				winners++
			}
		}
		if winners != 1 {
			t.Fatalf("trial %d: %d of %d concurrent RevokeCertificate calls reported changed = true, want exactly 1", trial, winners, goroutines)
		}

		got, err := ca.certificates.FindByID(ctx, cert.ID)
		if err != nil {
			t.Fatalf("trial %d: FindByID: %v", trial, err)
		}
		if got.Status != CertificateStatusRevoked {
			t.Fatalf("trial %d: certificate Status = %q after the race, want %q", trial, got.Status, CertificateStatusRevoked)
		}

		revocations, err := ca.revocations.ListByAuthority(ctx, cert.AuthorityID)
		if err != nil {
			t.Fatalf("trial %d: ListByAuthority: %v", trial, err)
		}
		if len(revocations) != 1 {
			t.Fatalf("trial %d: revocation ledger has %d rows after %d concurrent revoke calls, want exactly 1", trial, len(revocations), goroutines)
		}

		if len(rec.events) != 1 {
			t.Fatalf("trial %d: published %d EventCertificateRevoked, want exactly 1 (only the ledger insert winner publishes)", trial, len(rec.events))
		}
	}
}

// TestCAService_RevokeCertificate_ConcurrentDifferentReasons_CertificateAndLedgerNeverDisagree
// proves that under concurrent RevokeCertificate calls carrying DIFFERENT
// reasons for one still-active certificate, the certificate row and its
// ledger row always end up agreeing about when and why the revocation
// happened.
//
// Racing identical reasons cannot expose this class of bug: a losing
// caller's blind full-row certificate update would overwrite the
// certificate row with the LOSER's own RevocationReason/RevokedAt after
// the ledger winner's insert had already recorded the WINNER's -- one
// ledger row, one event and exactly one true answer would all hold, while
// the two tables silently disagreed about the metadata. With every racer
// carrying a distinct reason, any such loser-overwrite is visible as a
// certificate-row/ledger-row mismatch, and the run fails.
//
// The barrier shape and trial repetition mirror
// ConcurrentDoubleRevoke_ExactlyOneWinner's identical rig: 8 goroutines
// race per trial so several FindByID reads routinely complete before the
// first winner's certificate transition commits, and 25 trials make one
// scheduling accident into a property. Each goroutine further asserts, per
// trial: exactly one (true, nil) answer, one ledger row, one
// EventCertificateRevoked, and -- the point of the distinct reasons --
// certificate row, ledger row and event payload all carrying the same
// RevocationReason and timestamp.
func TestCAService_RevokeCertificate_ConcurrentDifferentReasons_CertificateAndLedgerNeverDisagree(t *testing.T) {
	const (
		goroutines = 8
		trials     = 25
	)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	for trial := 0; trial < trials; trial++ {
		ca, rec := newTestCAServiceWithBus(t)
		_, cert := issueTestCertificate(t, ca, ctx)

		changed := make([]bool, goroutines)
		errs := make([]error, goroutines)
		reasons := make([]string, goroutines)
		for i := 0; i < goroutines; i++ {
			reasons[i] = fmt.Sprintf("reason-%d", i)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				changed[i], errs[i] = ca.RevokeCertificate(ctx, cert.ID, reasons[i])
			}(i)
		}
		close(start)
		wg.Wait()

		winners := 0
		for i, err := range errs {
			if err != nil {
				t.Fatalf("trial %d: RevokeCertificate goroutine %d (reason %q): %v", trial, i, reasons[i], err)
			}
			if changed[i] {
				winners++
			}
		}
		if winners != 1 {
			t.Fatalf("trial %d: %d of %d concurrent RevokeCertificate calls reported changed = true, want exactly 1", trial, winners, goroutines)
		}

		got, err := ca.certificates.FindByID(ctx, cert.ID)
		if err != nil {
			t.Fatalf("trial %d: FindByID: %v", trial, err)
		}
		if got.Status != CertificateStatusRevoked {
			t.Fatalf("trial %d: certificate Status = %q after the race, want %q", trial, got.Status, CertificateStatusRevoked)
		}
		if got.RevokedAt == nil {
			t.Fatalf("trial %d: certificate RevokedAt is nil after the race", trial)
		}

		revocations, err := ca.revocations.ListByAuthority(ctx, cert.AuthorityID)
		if err != nil {
			t.Fatalf("trial %d: ListByAuthority: %v", trial, err)
		}
		if len(revocations) != 1 {
			t.Fatalf("trial %d: revocation ledger has %d rows after %d concurrent revoke calls, want exactly 1", trial, len(revocations), goroutines)
		}
		rev := revocations[0]

		if rev.RevocationReason != got.RevocationReason {
			t.Fatalf("trial %d: certificate row and ledger row disagree about the reason: certificate = %q, ledger = %q -- a losing concurrent revoke overwrote the certificate row with its own reason after the ledger winner recorded another", trial, got.RevocationReason, rev.RevocationReason)
		}
		if !rev.RevokedAt.Equal(*got.RevokedAt) {
			t.Fatalf("trial %d: certificate row and ledger row disagree about the time: certificate = %v, ledger = %v", trial, *got.RevokedAt, rev.RevokedAt)
		}

		if len(rec.events) != 1 {
			t.Fatalf("trial %d: published %d EventCertificateRevoked, want exactly 1 (only the ledger insert winner publishes)", trial, len(rec.events))
		}
		evt, ok := rec.events[0].Payload.(CertificateRevokedEvent)
		if !ok {
			t.Fatalf("trial %d: event payload = %T, want CertificateRevokedEvent", trial, rec.events[0].Payload)
		}
		if evt.RevocationReason != got.RevocationReason {
			t.Fatalf("trial %d: event payload and certificate row disagree about the reason: certificate = %q, event = %q", trial, got.RevocationReason, evt.RevocationReason)
		}
		if !evt.OccurredAt.Equal(*got.RevokedAt) {
			t.Fatalf("trial %d: event payload and certificate row disagree about the time: certificate = %v, event = %v", trial, *got.RevokedAt, evt.OccurredAt)
		}
	}
}

// TestCAService_RevokeCertificate_LedgerWriteFailure_ReturnsErrorAndRetryConverges
// proves the ledger write's honest failure contract end to end. A revocation
// whose ledger insert fails must surface as an ERROR -- never as the
// log-and-return-success the old code had, whose already-revoked early
// return then meant no later call could ever retry the lost ledger write.
// The error contract carries the state the caller needs to retry: the
// certificate itself is revoked and its ledger entry is missing. Dropping
// the failure and calling RevokeCertificate again must converge: exactly one
// ledger row (the first call's revocation, reason and all), exactly one
// EventCertificateRevoked (fired once, by the retry that reconstructed the
// row), and (true, nil) -- changed reports whether THIS call won the
// ledger-insert arbitration, and the retry is the call whose insert
// completed the revocation the failed first call left half-done.
//
// The failure is injected without an error seam: a SQLite trigger on the
// test's own database handle ABORTs every ledger insert, and is dropped to
// let the retry through (semgrep exempts test files wholesale).
func TestCAService_RevokeCertificate_LedgerWriteFailure_ReturnsErrorAndRetryConverges(t *testing.T) {
	ca, rec := newTestCAServiceWithBus(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	_, cert := issueTestCertificate(t, ca, ctx)

	db := ca.revocations.db
	if err := db.Exec(`CREATE TRIGGER trg_block_revocation_ledger_insert
		BEFORE INSERT ON pki_certificate_revocations
		BEGIN
			SELECT RAISE(ABORT, 'pki test: revocation ledger insert blocked');
		END`).Error; err != nil {
		t.Fatalf("install blocking trigger: %v", err)
	}

	changed, err := ca.RevokeCertificate(ctx, cert.ID, "first")
	if err == nil {
		t.Fatalf("RevokeCertificate with the ledger insert failing returned (changed=%v, nil) -- a failed ledger write must surface as an error, never as a reported success whose ledger row no later call will retry", changed)
	}

	got, err := ca.certificates.FindByID(ctx, cert.ID)
	if err != nil {
		t.Fatalf("FindByID after the failed revoke: %v", err)
	}
	if got.Status != CertificateStatusRevoked {
		t.Errorf("certificate Status = %q after the failed revoke, want %q -- the certificate row transition commits before the ledger write", got.Status, CertificateStatusRevoked)
	}
	if got.RevocationReason != "first" {
		t.Errorf("certificate row RevocationReason = %q after the failed revoke, want the failed call's reason %q", got.RevocationReason, "first")
	}
	// Row-then-event: a revoke that never recorded its ledger row must not
	// have published the event that announces the row.
	if len(rec.events) != 0 {
		t.Fatalf("failed revoke published %d EventCertificateRevoked, want 0 (the event follows the ledger row, which never landed)", len(rec.events))
	}

	if err = db.Exec(`DROP TRIGGER trg_block_revocation_ledger_insert`).Error; err != nil {
		t.Fatalf("drop blocking trigger: %v", err)
	}

	changed, err = ca.RevokeCertificate(ctx, cert.ID, "second")
	if err != nil {
		t.Fatalf("RevokeCertificate(retry after the ledger write recovered): %v", err)
	}
	if !changed {
		t.Errorf("RevokeCertificate(retry) changed = false, want true -- changed reports whether THIS call's insert won the ledger arbitration, and the retry is the call whose insert reconstructed the missing row (the failed first call never reached the ledger)")
	}

	revocations, err := ca.revocations.ListByAuthority(ctx, cert.AuthorityID)
	if err != nil {
		t.Fatalf("ListByAuthority: %v", err)
	}
	if len(revocations) != 1 {
		t.Fatalf("revocation ledger has %d rows after the failed revoke and its retry, want exactly 1", len(revocations))
	}
	if revocations[0].RevocationReason != "first" {
		t.Errorf("ledger row RevocationReason = %q, want the first call's reason %q -- the reconstructed row must match the certificate row, not the retry's argument", revocations[0].RevocationReason, "first")
	}

	if len(rec.events) != 1 {
		t.Fatalf("published %d EventCertificateRevoked after the failed revoke and its retry, want exactly 1 (fired once, by the retry that inserted the missing row)", len(rec.events))
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
