package pki

import (
	"context"
	"crypto"
	"errors"
	"testing"
)

// newReclaimTestService returns a Service over a fresh test database like
// newTestService, plus a LocalKeyRepository over the same database so a
// test can assert on the physical pki_local_keys rows ReclaimRetired is
// supposed to destroy.
func newReclaimTestService(t *testing.T) (*Service, *LocalKeyRepository) {
	t.Helper()
	db := newTestDB(t)
	signer := NewLocalSigner(db)
	svc := NewService(signer, "local", NewSigningKeyRepository(db), DefaultCacheTTL, DefaultPropagationWindow, DefaultRenewalLeadTime, DefaultExpiryScanWindow)
	t.Cleanup(func() { _ = svc.Close() })
	return svc, NewLocalKeyRepository(db)
}

// seedRealKey generates a real key through signer and seeds a SigningKey
// row of the given status pointing at it, returning the keyRef. The row
// carries the caller-supplied signer name, which is what lets a test plant
// a deliberately foreign-owned row whose material still exists locally.
func seedRealKey(t *testing.T, signer Signer, repo *SigningKeyRepository, id, purpose, signerName, status string) string {
	t.Helper()
	ctx := context.Background()
	keyRef, _, err := signer.GenerateKey(ctx, AlgorithmEd25519)
	if err != nil {
		t.Fatalf("generate real key for %s: %v", id, err)
	}
	key := newTestSigningKey(id, purpose, status)
	key.SignerName = signerName
	key.KeyRef = keyRef
	if err := repo.Create(ctx, key); err != nil {
		t.Fatalf("seed %s row: %v", id, err)
	}
	return keyRef
}

// containsKid reports whether ids holds kid.
func containsKid(ids []string, kid string) bool {
	for _, id := range ids {
		if id == kid {
			return true
		}
	}
	return false
}

// TestService_ReclaimRetired_DestroysTheOwnedRetiredKeysMaterial proves the
// reclamation ReclaimRetired exists for: a retired key whose material lives
// in this Service's own signer is physically destroyed -- for LocalSigner,
// the encrypted pki_local_keys row is gone and the repository answers
// ErrKeyNotFound -- and the key's kid is reported in Destroyed.
func TestService_ReclaimRetired_DestroysTheOwnedRetiredKeysMaterial(t *testing.T) {
	svc, localKeys := newReclaimTestService(t)
	ctx := context.Background()

	keyRef := seedRealKey(t, svc.signer, svc.signingKeys, "kid-retired", "authn.access_token", "local", SigningKeyStatusRetired)

	report, err := svc.ReclaimRetired(ctx)
	if err != nil {
		t.Fatalf("ReclaimRetired: %v", err)
	}
	if len(report.Destroyed) != 1 || report.Destroyed[0] != "kid-retired" {
		t.Errorf("Destroyed = %v, want [kid-retired]", report.Destroyed)
	}
	if report.NotOwned != nil {
		t.Errorf("NotOwned = %v, want nil", report.NotOwned)
	}
	if report.Failed != nil {
		t.Errorf("Failed = %v, want nil", report.Failed)
	}

	if _, err := localKeys.FindByKeyRef(ctx, keyRef); !isKeyNotFound(err) {
		t.Errorf("FindByKeyRef after reclaim = %v, want ErrKeyNotFound", err)
	}
}

// TestService_ReclaimRetired_TouchesOnlyRetiredKeys proves the walk's scope:
// a key in every other status -- pending, active, retiring, revoked -- is
// still doing lifecycle work and its material must survive a reclaim even
// when a retired sibling's does not.
func TestService_ReclaimRetired_TouchesOnlyRetiredKeys(t *testing.T) {
	svc, localKeys := newReclaimTestService(t)
	ctx := context.Background()

	seeded := []struct {
		status  string
		kid     string
		purpose string
	}{
		{status: SigningKeyStatusPending, kid: "kid-pending", purpose: "purpose.pending"},
		{status: SigningKeyStatusActive, kid: "kid-active", purpose: "purpose.active"},
		{status: SigningKeyStatusRetiring, kid: "kid-retiring", purpose: "purpose.retiring"},
		{status: SigningKeyStatusRetired, kid: "kid-retired", purpose: "purpose.retired"},
		{status: SigningKeyStatusRevoked, kid: "kid-revoked", purpose: "purpose.revoked"},
	}
	keyRefs := map[string]string{} // status -> keyRef
	for _, row := range seeded {
		keyRefs[row.status] = seedRealKey(t, svc.signer, svc.signingKeys, row.kid, row.purpose, "local", row.status)
	}

	report, err := svc.ReclaimRetired(ctx)
	if err != nil {
		t.Fatalf("ReclaimRetired: %v", err)
	}
	if len(report.Destroyed) != 1 || report.Destroyed[0] != "kid-retired" {
		t.Errorf("Destroyed = %v, want exactly [kid-retired]", report.Destroyed)
	}

	for _, row := range seeded {
		_, err := localKeys.FindByKeyRef(ctx, keyRefs[row.status])
		if row.status == SigningKeyStatusRetired {
			if !isKeyNotFound(err) {
				t.Errorf("retired key %s material still present after reclaim: FindByKeyRef = %v, want ErrKeyNotFound", row.kid, err)
			}
			continue
		}
		if isKeyNotFound(err) {
			t.Errorf("%s key %s material was destroyed by reclaim, want it to survive", row.status, row.kid)
		} else if err != nil {
			t.Errorf("FindByKeyRef for %s key %s: %v", row.status, row.kid, err)
		}
	}
}

// TestService_ReclaimRetired_DoesNotTouchKeysOwnedByAnotherSigner proves
// the signer-name guard: a retired row whose SignerName is not this
// Service's own belongs to a different Signer (a second Service instance
// sharing the pki_signing_keys table), and this Service must never hand
// that row's keyRef to its own Destroy. The planted row deliberately
// points at material this Service's own signer CAN destroy (a real local
// key wearing a foreign signer name), so a guard-less implementation
// deletes it and this test goes red; the guard is what keeps the material
// alive and moves the row to NotOwned instead.
func TestService_ReclaimRetired_DoesNotTouchKeysOwnedByAnotherSigner(t *testing.T) {
	svc, localKeys := newReclaimTestService(t)
	ctx := context.Background()

	foreignKeyRef := seedRealKey(t, svc.signer, svc.signingKeys, "kid-vault-owned", "authn.access_token", "vault", SigningKeyStatusRetired)

	report, err := svc.ReclaimRetired(ctx)
	if err != nil {
		t.Fatalf("ReclaimRetired: %v", err)
	}
	if len(report.NotOwned) != 1 || report.NotOwned[0] != "kid-vault-owned" {
		t.Errorf("NotOwned = %v, want [kid-vault-owned]", report.NotOwned)
	}
	if report.Destroyed != nil {
		t.Errorf("Destroyed = %v, want nil (nothing this Service owns was retired)", report.Destroyed)
	}
	if report.Failed != nil {
		t.Errorf("Failed = %v, want nil", report.Failed)
	}

	if _, err := localKeys.FindByKeyRef(ctx, foreignKeyRef); isKeyNotFound(err) {
		t.Errorf("foreign-owned retired key's material was destroyed by reclaim, want it to survive")
	} else if err != nil {
		t.Errorf("FindByKeyRef for the foreign-owned key: %v", err)
	}
}

// TestService_ReclaimRetired_KeyNotFoundCountsAsAlreadyReclaimed proves the
// convergence rule: a retired row whose material is already gone (Destroy
// answers ErrKeyNotFound) is a reclamation that already happened, not a
// failure -- the key lands in Destroyed, and repeated calls stay quiet.
func TestService_ReclaimRetired_KeyNotFoundCountsAsAlreadyReclaimed(t *testing.T) {
	svc, _ := newReclaimTestService(t)
	ctx := context.Background()

	key := newTestSigningKey("kid-already-gone", "authn.access_token", SigningKeyStatusRetired)
	if err := svc.signingKeys.Create(ctx, key); err != nil {
		t.Fatalf("seed already-reclaimed row: %v", err)
	}

	for run := 0; run < 2; run++ {
		report, err := svc.ReclaimRetired(ctx)
		if err != nil {
			t.Fatalf("ReclaimRetired run %d: %v", run, err)
		}
		if len(report.Destroyed) != 1 || report.Destroyed[0] != "kid-already-gone" {
			t.Errorf("run %d: Destroyed = %v, want [kid-already-gone]", run, report.Destroyed)
		}
		if report.Failed != nil {
			t.Errorf("run %d: Failed = %v, want nil", run, report.Failed)
		}
	}
}

// reclaimStubSigner is a Signer double whose Destroy answers per keyRef
// from a caller-supplied map, recording every keyRef it was asked to
// destroy. It exists only so reclaim_test.go can pin the walk's per-key
// failure isolation without a real provider.
type reclaimStubSigner struct {
	destroyAnswers map[string]error
	destroyed      []string
}

func (s *reclaimStubSigner) GenerateKey(ctx context.Context, algorithm string) (string, crypto.PublicKey, error) {
	panic("reclaimStubSigner.GenerateKey must not be called")
}

func (s *reclaimStubSigner) Sign(ctx context.Context, keyRef string, input []byte) ([]byte, error) {
	panic("reclaimStubSigner.Sign must not be called")
}

func (s *reclaimStubSigner) Public(ctx context.Context, keyRef string) (crypto.PublicKey, error) {
	panic("reclaimStubSigner.Public must not be called")
}

func (s *reclaimStubSigner) Destroy(_ context.Context, keyRef string) error {
	s.destroyed = append(s.destroyed, keyRef)
	return s.destroyAnswers[keyRef]
}

// TestService_ReclaimRetired_ContinuesPastAFailedDestroy proves per-key
// isolation: one key's Destroy failing must neither abort the walk nor be
// misreported as reclaimed -- the failed key lands in Failed, every other
// retired key is still processed, and a later call re-attempts the failed
// one until the provider answers.
func TestService_ReclaimRetired_ContinuesPastAFailedDestroy(t *testing.T) {
	db := newTestDB(t)
	stub := &reclaimStubSigner{
		destroyAnswers: map[string]error{
			"keyref-failing": errors.New("provider.boom"),
			"keyref-absent":  ErrKeyNotFound,
		},
	}
	svc := NewService(stub, "stub", NewSigningKeyRepository(db), DefaultCacheTTL, DefaultPropagationWindow, DefaultRenewalLeadTime, DefaultExpiryScanWindow)
	t.Cleanup(func() { _ = svc.Close() })
	ctx := context.Background()

	for _, seeded := range []struct {
		kid    string
		keyRef string
	}{
		{kid: "kid-failing", keyRef: "keyref-failing"},
		{kid: "kid-ok", keyRef: "keyref-ok"},
		{kid: "kid-absent", keyRef: "keyref-absent"},
	} {
		key := newTestSigningKey(seeded.kid, "authn.access_token", SigningKeyStatusRetired)
		key.SignerName = "stub"
		key.KeyRef = seeded.keyRef
		if err := svc.signingKeys.Create(ctx, key); err != nil {
			t.Fatalf("seed %s: %v", seeded.kid, err)
		}
	}

	report, err := svc.ReclaimRetired(ctx)
	if err != nil {
		t.Fatalf("ReclaimRetired: %v", err)
	}
	if len(report.Failed) != 1 || report.Failed[0] != "kid-failing" {
		t.Errorf("Failed = %v, want [kid-failing]", report.Failed)
	}
	if len(report.Destroyed) != 2 || !containsKid(report.Destroyed, "kid-ok") || !containsKid(report.Destroyed, "kid-absent") {
		t.Errorf("Destroyed = %v, want {kid-ok, kid-absent}", report.Destroyed)
	}
	if len(stub.destroyed) != 3 {
		t.Errorf("Destroy was asked %d times, want 3 (the walk must not stop at the failure)", len(stub.destroyed))
	}

	// The failing key stays retired with its material in place: a second
	// call re-attempts it, and once the provider starts answering, it
	// converges.
	stub.destroyAnswers["keyref-failing"] = nil
	report, err = svc.ReclaimRetired(ctx)
	if err != nil {
		t.Fatalf("ReclaimRetired after recovery: %v", err)
	}
	if report.Failed != nil {
		t.Errorf("Failed after recovery = %v, want nil", report.Failed)
	}
	if len(report.Destroyed) != 3 || !containsKid(report.Destroyed, "kid-failing") {
		t.Errorf("Destroyed after recovery = %v, want all three kids", report.Destroyed)
	}
}

// compile-time check that the stub satisfies Signer.
var _ Signer = (*reclaimStubSigner)(nil)
