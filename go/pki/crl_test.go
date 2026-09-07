package pki

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// parseCRLPEM reverses encodeCRLPEM, for tests that need to inspect the
// generated document's own fields (RevokedCertificateEntries, Number,
// NextUpdate) rather than treating it as an opaque string.
func parseCRLPEM(t *testing.T, crlPEM string) *x509.RevocationList {
	t.Helper()
	block, _ := pem.Decode([]byte(crlPEM))
	if block == nil {
		t.Fatalf("parseCRLPEM: no PEM block found in %q", crlPEM)
	}
	rl, err := x509.ParseRevocationList(block.Bytes)
	if err != nil {
		t.Fatalf("x509.ParseRevocationList: %v", err)
	}
	return rl
}

func TestCAService_GenerateCRL_EmptyWhenNoRevocations(t *testing.T) {
	ca := newTestCAService(t)
	ctx := context.Background()

	root, err := ca.CreateRootCA(ctx, RootCAParams{
		Subject:  pkix.Name{CommonName: "speed Root CA"},
		NotAfter: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRootCA: %v", err)
	}

	authority, err := ca.GenerateCRL(ctx, root.ID, 0)
	if err != nil {
		t.Fatalf("GenerateCRL: %v", err)
	}
	if authority.CRLNumber != 1 {
		t.Errorf("CRLNumber = %d, want 1", authority.CRLNumber)
	}
	if authority.CRLPEM == "" {
		t.Fatal("CRLPEM is empty, want a generated document even with zero revocations")
	}
	if authority.CRLIssuedAt == nil || authority.CRLNextUpdate == nil {
		t.Errorf("CRLIssuedAt/CRLNextUpdate = %v/%v, want both set", authority.CRLIssuedAt, authority.CRLNextUpdate)
	}

	rl := parseCRLPEM(t, authority.CRLPEM)
	if len(rl.RevokedCertificateEntries) != 0 {
		t.Errorf("RevokedCertificateEntries = %d, want 0", len(rl.RevokedCertificateEntries))
	}
}

// TestCAService_GenerateCRL_ListsRevokedCertificates_AndVerifiesSignature is
// the round's central CRL proof: it revokes a real certificate, generates
// the issuing authority's CRL, and verifies the CRL's OWN signature against
// the issuer's certificate through the standard library's own
// x509.RevocationList.CheckSignatureFrom -- never a hand-rolled check --
// while also confirming the revoked certificate's serial appears in the
// document.
func TestCAService_GenerateCRL_ListsRevokedCertificates_AndVerifiesSignature(t *testing.T) {
	ca := newTestCAService(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	authority, cert := issueTestCertificate(t, ca, ctx)
	if _, err := ca.RevokeCertificate(ctx, cert.ID, "compromised"); err != nil {
		t.Fatalf("RevokeCertificate: %v", err)
	}

	got, err := ca.GenerateCRL(ctx, authority.ID, 0)
	if err != nil {
		t.Fatalf("GenerateCRL: %v", err)
	}

	rl := parseCRLPEM(t, got.CRLPEM)
	if len(rl.RevokedCertificateEntries) != 1 {
		t.Fatalf("RevokedCertificateEntries = %d, want 1", len(rl.RevokedCertificateEntries))
	}
	wantSerial, ok := new(big.Int).SetString(cert.Serial, 16)
	if !ok {
		t.Fatalf("could not parse test fixture serial %q", cert.Serial)
	}
	if rl.RevokedCertificateEntries[0].SerialNumber.Cmp(wantSerial) != 0 {
		t.Errorf("revoked entry serial = %s, want %s", rl.RevokedCertificateEntries[0].SerialNumber, wantSerial)
	}

	issuerCert, err := parseCertificatePEM(authority.CertificatePEM)
	if err != nil {
		t.Fatalf("parse issuer certificate: %v", err)
	}
	if err := rl.CheckSignatureFrom(issuerCert); err != nil {
		t.Errorf("CheckSignatureFrom(issuer): %v -- the CRL does not verify against its own issuer's certificate", err)
	}
}

func TestCAService_GenerateCRL_IncrementsCRLNumber(t *testing.T) {
	ca := newTestCAService(t)
	ctx := context.Background()

	root, err := ca.CreateRootCA(ctx, RootCAParams{
		Subject:  pkix.Name{CommonName: "speed Root CA"},
		NotAfter: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRootCA: %v", err)
	}

	first, err := ca.GenerateCRL(ctx, root.ID, 0)
	if err != nil {
		t.Fatalf("GenerateCRL(first): %v", err)
	}
	second, err := ca.GenerateCRL(ctx, root.ID, 0)
	if err != nil {
		t.Fatalf("GenerateCRL(second): %v", err)
	}
	if first.CRLNumber != 1 || second.CRLNumber != 2 {
		t.Errorf("CRLNumber sequence = %d, %d, want 1, 2", first.CRLNumber, second.CRLNumber)
	}
}

// TestCAService_GenerateCRL_ConcurrentCalls_EveryCallLandsItsOwnNumber is
// the P2-3 regression proof: concurrent GenerateCRL calls for one authority
// must each land its own CRL at its own number -- the register advances by
// exactly one per successful call, however many calls overlap. Before the
// fix the persist step was a blind full-row save of a read-modify-write
// cycle, so racing calls that read the same CRLNumber both wrote the same
// nextNumber: the loser's document silently overwrote the winner's, the
// register advanced once where N calls had run, and a loser whose
// revocation snapshot was older than the winner's dropped that revocation
// from the persisted CRL until a later tick. The fix arbitrates the write
// on the crl_number register (repository.go's UpdateCRLIfCurrent), so
// exactly one racing call's conditional UPDATE lands per number and every
// loser retries at the fresh number.
//
// The barrier shape and trial repetition mirror
// TestCAService_RevokeCertificate_ConcurrentDoubleRevoke_ExactlyOneWinner's
// identical rig (revocation_test.go): 8 goroutines released through a
// closed channel, no sleeps, 25 trials. Each trial asserts every call
// returned successfully and the final CRLNumber is exactly initial+8 --
// under the unguarded code most trials lost at least one update -- plus
// the stored document's own Number field agreeing with the column, the
// invariant a verifier actually relies on.
func TestCAService_GenerateCRL_ConcurrentCalls_EveryCallLandsItsOwnNumber(t *testing.T) {
	const (
		goroutines = 8
		trials     = 25
	)
	ctx := context.Background()

	for trial := 0; trial < trials; trial++ {
		ca := newTestCAService(t)
		root, err := ca.CreateRootCA(ctx, RootCAParams{
			Subject:  pkix.Name{CommonName: "speed Root CA"},
			NotAfter: time.Now().Add(24 * time.Hour),
		})
		if err != nil {
			t.Fatalf("trial %d: CreateRootCA: %v", trial, err)
		}

		errs := make([]error, goroutines)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = ca.GenerateCRL(ctx, root.ID, 0)
			}(i)
		}
		close(start)
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("trial %d: GenerateCRL goroutine %d: %v", trial, i, err)
			}
		}

		got, err := ca.authorities.FindByID(ctx, root.ID)
		if err != nil {
			t.Fatalf("trial %d: FindByID: %v", trial, err)
		}
		if want := root.CRLNumber + goroutines; got.CRLNumber != want {
			t.Fatalf("trial %d: CRLNumber = %d after %d concurrent GenerateCRL calls (was %d), want %d -- at least one call's update was lost, two calls persisted the same number", trial, got.CRLNumber, goroutines, root.CRLNumber, want)
		}

		// The persisted document's own Number must agree with the register
		// column: a verifier keys its caching on the document's embedded
		// number, and the row claims a number the document does not carry
		// whenever a blind save races the guard.
		rl := parseCRLPEM(t, got.CRLPEM)
		if rl.Number == nil || rl.Number.Int64() != got.CRLNumber {
			t.Errorf("trial %d: stored CRL document Number = %v, want %d (the authority row's CRLNumber)", trial, rl.Number, got.CRLNumber)
		}
	}
}

func TestCAService_GenerateCRL_AuthorityNotFound(t *testing.T) {
	ca := newTestCAService(t)
	if _, err := ca.GenerateCRL(context.Background(), "does-not-exist", 0); !apperrIs(err, ErrAuthorityNotFound) {
		t.Errorf("GenerateCRL(missing authority) error = %v, want ErrAuthorityNotFound", err)
	}
}

// TestCAService_GenerateCRL_WrapsSignerFailureAsErrSignerUnavailable proves
// this round's real first trigger of ErrSignerUnavailable: a Signer.Sign
// failure that is not itself a coded *apperr.Error is folded into it.
func TestCAService_GenerateCRL_WrapsSignerFailureAsErrSignerUnavailable(t *testing.T) {
	db := newTestDB(t)
	local := NewLocalSigner(db)
	failing := &signFailingSigner{Signer: local}
	ca := NewCAService(failing, "local", NewAuthorityRepository(db), NewCertificateRepository(db), NewCertificateRevocationRepository(db))
	ctx := context.Background()

	root, err := ca.CreateRootCA(ctx, RootCAParams{
		Subject:  pkix.Name{CommonName: "speed Root CA"},
		NotAfter: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRootCA: %v", err)
	}

	failing.fail = true
	if _, err := ca.GenerateCRL(ctx, root.ID, 0); !apperrIs(err, ErrSignerUnavailable) {
		t.Errorf("GenerateCRL(signer failing) error = %v, want ErrSignerUnavailable", err)
	}
}

func TestCAService_RegenerateAllCRLs_RegeneratesEveryAuthority(t *testing.T) {
	ca := newTestCAService(t)
	ctx := context.Background()

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

	regenerated, err := ca.RegenerateAllCRLs(ctx)
	if err != nil {
		t.Fatalf("RegenerateAllCRLs: %v", err)
	}
	ids := map[string]bool{}
	for _, id := range regenerated {
		ids[id] = true
	}
	if !ids[root.ID] || !ids[intermediate.ID] {
		t.Errorf("RegenerateAllCRLs = %v, want both %q and %q", ids, root.ID, intermediate.ID)
	}

	gotRoot, err := ca.authorities.FindByID(ctx, root.ID)
	if err != nil {
		t.Fatalf("FindByID(root): %v", err)
	}
	if gotRoot.CRLPEM == "" {
		t.Error("root authority's CRLPEM is empty after RegenerateAllCRLs")
	}
	gotIntermediate, err := ca.authorities.FindByID(ctx, intermediate.ID)
	if err != nil {
		t.Fatalf("FindByID(intermediate): %v", err)
	}
	if gotIntermediate.CRLPEM == "" {
		t.Error("intermediate authority's CRLPEM is empty after RegenerateAllCRLs")
	}
}

func TestCAService_EnqueueCRLRegenerate_NoQueueWired(t *testing.T) {
	ca := newTestCAService(t)
	if err := ca.EnqueueCRLRegenerate(context.Background()); err == nil {
		t.Fatal("EnqueueCRLRegenerate with no queue wired succeeded, want an error")
	}
}

func TestCAService_EnqueueCRLRegenerate_Enqueues(t *testing.T) {
	ca := newTestCAService(t)
	queue := &recordingQueue{}
	ca.attachQueue(queue)
	windowA := time.Date(2026, 9, 7, 10, 15, 0, 0, time.UTC)
	ca.now = func() time.Time { return windowA }

	if err := ca.EnqueueCRLRegenerate(context.Background()); err != nil {
		t.Fatalf("EnqueueCRLRegenerate: %v", err)
	}
	if len(queue.tasks) != 1 {
		t.Fatalf("queue received %d task(s), want 1", len(queue.tasks))
	}
	if queue.tasks[0].Type != taskTypeCRLRegenerate {
		t.Errorf("task Type = %q, want %q", queue.tasks[0].Type, taskTypeCRLRegenerate)
	}
	if queue.tasks[0].TenantID != platformCRLRegenerateTenantID {
		t.Errorf("task TenantID = %q, want %q", queue.tasks[0].TenantID, platformCRLRegenerateTenantID)
	}
	if want := crlRegenerateIdempotencyKey(crlRegenerateWindowStart(windowA, ca.crlRegenerateWindow)); queue.tasks[0].IdempotencyKey != want {
		t.Errorf("task IdempotencyKey = %q, want %q (the windowed key of the enqueue's clock read)", queue.tasks[0].IdempotencyKey, want)
	}
}

func TestCrlRegenerateHandler(t *testing.T) {
	ca := newTestCAService(t)
	ctx := context.Background()
	root, err := ca.CreateRootCA(ctx, RootCAParams{
		Subject:  pkix.Name{CommonName: "speed Root CA"},
		NotAfter: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRootCA: %v", err)
	}

	h := crlRegenerateHandler{ca: ca}
	if h.Type() != taskTypeCRLRegenerate {
		t.Errorf("Type() = %q, want %q", h.Type(), taskTypeCRLRegenerate)
	}

	if _, err = h.Handle(ctx, &jobs.Job{}, nil); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, err := ca.authorities.FindByID(ctx, root.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.CRLPEM == "" {
		t.Error("Handle did not generate a CRL for the root authority")
	}

	if _, err = h.Handle(ctx, &jobs.Job{Payload: []byte(`{}`)}, nil); err == nil {
		t.Error("Handle(non-empty payload) succeeded, want a task-shape rejection")
	}
}

// signFailingSigner wraps a real Signer and, when fail is true, makes Sign
// return a plain (non-*apperr.Error) failure -- the shape
// TestCAService_GenerateCRL_WrapsSignerFailureAsErrSignerUnavailable needs
// to prove GenerateCRL's own wrapping.
type signFailingSigner struct {
	Signer
	fail bool
}

func (s *signFailingSigner) Sign(ctx context.Context, keyRef string, input []byte) ([]byte, error) {
	if s.fail {
		return nil, errors.New("signer: simulated network failure")
	}
	return s.Signer.Sign(ctx, keyRef, input)
}

// recordingQueue (job_test.go) is reused here -- it already records every
// Enqueue call, which is all EnqueueCRLRegenerate's own test needs.
