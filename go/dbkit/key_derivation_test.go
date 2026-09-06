package dbkit

import (
	"bytes"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"testing"
)

func TestDeriveKey_Deterministic(t *testing.T) {
	root := testKey("root-secret")

	got1, err := DeriveKey(root, "speed.dbkit.example.v1")
	if err != nil {
		t.Fatalf("DeriveKey() error = %v", err)
	}
	got2, err := DeriveKey(root, "speed.dbkit.example.v1")
	if err != nil {
		t.Fatalf("DeriveKey() error = %v", err)
	}

	if !bytes.Equal(got1, got2) {
		t.Fatalf("DeriveKey(root, purpose) is not deterministic: %x != %x", got1, got2)
	}
}

func TestDeriveKey_DifferentPurposes_ProduceDifferentKeys(t *testing.T) {
	root := testKey("root-secret")

	keyA, err := DeriveKey(root, "speed.dbkit.purpose-a.v1")
	if err != nil {
		t.Fatalf("DeriveKey() error = %v", err)
	}
	keyB, err := DeriveKey(root, "speed.dbkit.purpose-b.v1")
	if err != nil {
		t.Fatalf("DeriveKey() error = %v", err)
	}

	if bytes.Equal(keyA, keyB) {
		t.Fatalf("DeriveKey produced identical output for two distinct purposes: %x", keyA)
	}
}

func TestDeriveKey_DifferentRootKeys_ProduceDifferentKeys(t *testing.T) {
	const purpose = "speed.dbkit.example.v1"

	keyA, err := DeriveKey(testKey("root-a"), purpose)
	if err != nil {
		t.Fatalf("DeriveKey() error = %v", err)
	}
	keyB, err := DeriveKey(testKey("root-b"), purpose)
	if err != nil {
		t.Fatalf("DeriveKey() error = %v", err)
	}

	if bytes.Equal(keyA, keyB) {
		t.Fatalf("DeriveKey produced identical output for two distinct root keys: %x", keyA)
	}
}

func TestDeriveKey_OutputLength(t *testing.T) {
	got, err := DeriveKey(testKey("root-secret"), "speed.dbkit.example.v1")
	if err != nil {
		t.Fatalf("DeriveKey() error = %v", err)
	}
	if len(got) != derivedKeySize {
		t.Fatalf("len(DeriveKey(...)) = %d, want %d", len(got), derivedKeySize)
	}
	// The output must also satisfy NewCipher/NewBlindIndexer's own 32-byte
	// floor directly -- this is the whole point of a fixed derivedKeySize.
	if len(got) != encryptionKeySize {
		t.Fatalf("len(DeriveKey(...)) = %d, want it to match encryptionKeySize (%d) so it drops straight into NewCipher/NewBlindIndexer", len(got), encryptionKeySize)
	}
}

func TestDeriveKey_RootKeyLength(t *testing.T) {
	tests := []struct {
		name    string
		rootKey []byte
		wantErr bool
	}{
		{name: "exactly 32 bytes", rootKey: testKey("valid-root"), wantErr: false},
		{name: "nil", rootKey: nil, wantErr: true},
		{name: "too short", rootKey: make([]byte, 16), wantErr: true},
		{name: "too long", rootKey: make([]byte, 64), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DeriveKey(tt.rootKey, "speed.dbkit.example.v1")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("DeriveKey() error = nil, want a non-nil error")
				}
				if !errors.Is(err, ErrInvalidKeySize) {
					t.Fatalf("DeriveKey() error = %v, want it to wrap ErrInvalidKeySize", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("DeriveKey() error = %v, want nil", err)
			}
		})
	}
}

func TestDeriveKey_EmptyPurpose_Rejected(t *testing.T) {
	_, err := DeriveKey(testKey("root-secret"), "")
	if err == nil {
		t.Fatal("DeriveKey(root, \"\") error = nil, want a non-nil error")
	}
}

// TestDeriveKey_KnownAnswer_ManualHKDFComputation reproduces DeriveKey's
// output from first principles -- HMAC-SHA256 called directly, never
// through the crypto/hkdf package DeriveKey itself wraps -- so a future
// change that accidentally swaps the hash, the construction (e.g. HKDF-
// Expand-only instead of the full Extract-then-Expand pipeline), or the
// info-string encoding fails this test even if it also broke the
// hand-computation below in the exact same way is astronomically
// unlikely, since the two computations share no code path.
//
// The derivation collapses to one HMAC application because derivedKeySize
// (32) equals SHA-256's own output size: HKDF-Expand's first block, T(1) =
// HMAC-SHA256(PRK, info || 0x01), is already exactly 32 bytes, so no
// second block or truncation is needed -- keeping this "by hand"
// computation short enough to read and trust at a glance.
func TestDeriveKey_KnownAnswer_ManualHKDFComputation(t *testing.T) {
	root := testKey("known-answer-root")
	const purpose = "speed.dbkit.known_answer.v1"

	// RFC 5869 section 2.2: an absent salt is replaced by HashLen
	// (32, for SHA-256) zero-valued octets before HKDF-Extract runs.
	zeroSalt := make([]byte, sha256.Size)
	prkMAC := hmac.New(sha256.New, zeroSalt)
	prkMAC.Write(root)
	prk := prkMAC.Sum(nil)

	// RFC 5869 section 2.3: T(1) = HMAC-Hash(PRK, T(0) | info | 0x01),
	// with T(0) the empty string.
	t1MAC := hmac.New(sha256.New, prk)
	t1MAC.Write([]byte(purpose))
	t1MAC.Write([]byte{0x01})
	want := t1MAC.Sum(nil)
	if len(want) != derivedKeySize {
		t.Fatalf("manual computation produced %d bytes, want %d -- test setup itself is wrong", len(want), derivedKeySize)
	}

	got, err := DeriveKey(root, purpose)
	if err != nil {
		t.Fatalf("DeriveKey() error = %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("DeriveKey(root, purpose) = %x, want the independently hand-computed HKDF-SHA256 output %x", got, want)
	}
}

// TestHKDFPrimitiveMatchesRFC5869TestVector proves the crypto/hkdf.Key
// composition DeriveKey builds on -- SHA-256, full Extract-then-Expand, an
// absent salt replaced by HashLen zero octets -- matches RFC 5869's own
// published test vector (Test Case 3: SHA-256, a zero-length/absent salt
// and info), independently of both DeriveKey and of
// TestDeriveKey_KnownAnswer_ManualHKDFComputation's own from-scratch
// computation above. This is called directly against crypto/hkdf.Key
// rather than through DeriveKey because the RFC vector's 22-byte input
// keying material does not fit DeriveKey's own 32-byte rootKey contract;
// what this test guards is the shared underlying primitive, not DeriveKey's
// own input validation (covered separately by
// TestDeriveKey_RootKeyLength).
func TestHKDFPrimitiveMatchesRFC5869TestVector(t *testing.T) {
	ikm := bytes.Repeat([]byte{0x0b}, 22)
	want := []byte{
		0x8d, 0xa4, 0xe7, 0x75, 0xa5, 0x63, 0xc1, 0x8f,
		0x71, 0x5f, 0x80, 0x2a, 0x06, 0x3c, 0x5a, 0x31,
		0xb8, 0xa1, 0x1f, 0x5c, 0x5e, 0xe1, 0x87, 0x9e,
		0xc3, 0x45, 0x4e, 0x5f, 0x3c, 0x73, 0x8d, 0x2d,
		0x9d, 0x20, 0x13, 0x95, 0xfa, 0xa4, 0xb6, 0x1a,
		0x96, 0xc8,
	}

	got, err := hkdf.Key(sha256.New, ikm, nil, "", len(want))
	if err != nil {
		t.Fatalf("hkdf.Key() error = %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("hkdf.Key(sha256.New, ikm, nil, \"\", 42) = %x, want RFC 5869 Test Case 3's published OKM %x", got, want)
	}
}
