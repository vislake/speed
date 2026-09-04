package pki

import (
	"context"
	"testing"
	"time"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	db := newTestDB(t)
	signer := NewLocalSigner(db)
	svc := NewService(signer, "local", NewSigningKeyRepository(db), DefaultCacheTTL, DefaultPropagationWindow, DefaultRenewalLeadTime)
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

func TestService_EnsurePurpose_RejectsEmptyPurpose(t *testing.T) {
	svc := newTestService(t)
	if err := svc.EnsurePurpose(context.Background(), "", AlgorithmEd25519, time.Minute); err == nil {
		t.Fatalf("EnsurePurpose(empty purpose) succeeded, want an error")
	}
}

func TestService_EnsurePurpose_RejectsNonPositiveMaxCredentialLifetime(t *testing.T) {
	svc := newTestService(t)
	if err := svc.EnsurePurpose(context.Background(), "authn.access_token", AlgorithmEd25519, 0); err == nil {
		t.Fatalf("EnsurePurpose(zero maxCredentialLifetime) succeeded, want an error")
	}
}

// TestService_EnsurePurpose_CreatesAnActiveKeySynchronously pins this
// round's deliberately simplified behavior: EnsurePurpose does not stage a
// pending key and wait for a propagation window (round 2) -- it creates a
// key and marks it active in the same call. See Service's own doc comment.
func TestService_EnsurePurpose_CreatesAnActiveKeySynchronously(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); err != nil {
		t.Fatalf("EnsurePurpose: %v", err)
	}

	kid, algorithm, sign, err := svc.ActiveSigner(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("ActiveSigner: %v", err)
	}
	if kid == "" {
		t.Fatalf("ActiveSigner returned an empty kid")
	}
	if algorithm != AlgorithmEd25519 {
		t.Errorf("ActiveSigner algorithm = %q, want %q", algorithm, AlgorithmEd25519)
	}
	if _, err := sign(ctx, []byte("hello")); err != nil {
		t.Errorf("the returned sign function failed: %v", err)
	}
}

// TestService_EnsurePurpose_Idempotent proves a second call for a purpose
// that already has an active key is a no-op: it must not create a second
// row (which the migration's partial unique index would refuse anyway) or
// return an error.
func TestService_EnsurePurpose_Idempotent(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); err != nil {
		t.Fatalf("EnsurePurpose (first call): %v", err)
	}
	kid1, _, _, err := svc.ActiveSigner(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("ActiveSigner (first): %v", err)
	}

	if err = svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); err != nil {
		t.Fatalf("EnsurePurpose (second call): %v", err)
	}
	kid2, _, _, err := svc.ActiveSigner(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("ActiveSigner (second): %v", err)
	}
	if kid1 != kid2 {
		t.Errorf("EnsurePurpose called twice produced two different active keys: %q then %q", kid1, kid2)
	}
}

func TestService_ActiveSigner_NoActiveKey(t *testing.T) {
	svc := newTestService(t)
	if _, _, _, err := svc.ActiveSigner(context.Background(), "authn.access_token"); !apperrIs(err, ErrNoActiveKey) {
		t.Errorf("ActiveSigner(no key ever created) error = %v, want ErrNoActiveKey", err)
	}
}

// TestService_VerificationKeys_ReturnsNonRevokedKeys proves VerificationKeys
// returns every key for the purpose that is not revoked -- this round's
// simplified "all still-verifiable keys" answer, per Service's own doc
// comment on what round 1 does and does not implement.
func TestService_VerificationKeys_ReturnsNonRevokedKeys(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); err != nil {
		t.Fatalf("EnsurePurpose: %v", err)
	}
	// A revoked key for the same purpose must be excluded.
	if err := svc.signingKeys.Create(ctx, newTestSigningKey("kid-revoked", "authn.access_token", SigningKeyStatusRevoked)); err != nil {
		t.Fatalf("seed revoked key: %v", err)
	}

	keys, err := svc.VerificationKeys(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("VerificationKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("VerificationKeys returned %d keys, want 1 (the active one, revoked excluded)", len(keys))
	}
	if keys[0].Algorithm != AlgorithmEd25519 {
		t.Errorf("VerificationKeys[0].Algorithm = %q, want %q", keys[0].Algorithm, AlgorithmEd25519)
	}
	if keys[0].Public == nil {
		t.Errorf("VerificationKeys[0].Public is nil, want a parsed public key")
	}
}
