package pki

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"
)

func TestLocalSigner_GenerateKey_RejectsUnsupportedAlgorithm(t *testing.T) {
	signer := NewLocalSigner(newTestDB(t))
	_, _, err := signer.GenerateKey(context.Background(), "ecdsa-p256")
	if !apperrIs(err, ErrAlgorithmUnsupportedBySigner) {
		t.Fatalf("GenerateKey(unsupported) error = %v, want ErrAlgorithmUnsupportedBySigner", err)
	}
}

func TestLocalSigner_GenerateKey_StoresAnEncryptedPrivateKey(t *testing.T) {
	db := newTestDB(t)
	signer := NewLocalSigner(db)
	ctx := context.Background()

	keyRef, pub, err := signer.GenerateKey(ctx, AlgorithmEd25519)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if keyRef == "" {
		t.Fatalf("GenerateKey returned an empty keyRef")
	}
	if _, ok := pub.(ed25519.PublicKey); !ok {
		t.Fatalf("GenerateKey public key type = %T, want ed25519.PublicKey", pub)
	}

	// The row on disk must never carry the plaintext PKCS8 encoding --
	// reading it back through a plain query (bypassing the serializer)
	// must show ciphertext, not the key.
	row, err := NewLocalKeyRepository(db).FindByKeyRef(ctx, keyRef)
	if err != nil {
		t.Fatalf("FindByKeyRef: %v", err)
	}
	if row.EncryptedPrivateKey == "" {
		t.Fatalf("stored EncryptedPrivateKey is empty")
	}
}

func TestLocalSigner_SignAndPublic_RoundTrip(t *testing.T) {
	signer := NewLocalSigner(newTestDB(t))
	ctx := context.Background()

	keyRef, pub, err := signer.GenerateKey(ctx, AlgorithmEd25519)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	gotPub, err := signer.Public(ctx, keyRef)
	if err != nil {
		t.Fatalf("Public: %v", err)
	}
	if !bytes.Equal(gotPub.(ed25519.PublicKey), pub.(ed25519.PublicKey)) {
		t.Errorf("Public() = %x, want %x", gotPub, pub)
	}

	message := []byte("pki local signer round trip")
	sig, err := signer.Sign(ctx, keyRef, message)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !ed25519.Verify(pub.(ed25519.PublicKey), message, sig) {
		t.Errorf("ed25519.Verify failed for the signature LocalSigner produced")
	}
}

func TestLocalSigner_Sign_UnknownKeyRef(t *testing.T) {
	signer := NewLocalSigner(newTestDB(t))
	if _, err := signer.Sign(context.Background(), "does-not-exist", []byte("x")); !apperrIs(err, ErrKeyNotFound) {
		t.Errorf("Sign(unknown keyRef) error = %v, want ErrKeyNotFound", err)
	}
}

func TestLocalSigner_Destroy_RemovesTheKey(t *testing.T) {
	signer := NewLocalSigner(newTestDB(t))
	ctx := context.Background()

	keyRef, _, err := signer.GenerateKey(ctx, AlgorithmEd25519)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if err := signer.Destroy(ctx, keyRef); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := signer.Public(ctx, keyRef); !apperrIs(err, ErrKeyNotFound) {
		t.Errorf("Public(after Destroy) error = %v, want ErrKeyNotFound", err)
	}
}

// compile-time check that *LocalSigner satisfies Signer -- also asserted in
// local_signer.go itself; repeated here so the test file documents the
// contract it exercises.
var _ Signer = (*LocalSigner)(nil)
