package wechat

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

// nonceAlphabet mirrors sign.go's own generateNonce charset: real WeChat
// Pay "nonce" values are ASCII strings, never arbitrary binary -- the
// JSON envelope's resource.nonce field is a JSON string, which must be
// valid UTF-8, so the raw bytes AES-GCM actually uses as its nonce are
// exactly the ASCII bytes of that string. Generating a raw random 12-byte
// sequence here (crypto/rand.Read into an unrestricted []byte) would very
// likely NOT be valid UTF-8, and encoding/json would silently mangle it on
// marshal -- a fixture bug this const avoids, not a workaround for a
// production one: decryptResource's own []byte(nonce) conversion is
// exactly what a genuine WeChat Pay ASCII nonce already round-trips
// through correctly.
const nonceTestAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// encryptResourceForTest is decryptResource's inverse, used only to build
// realistic AEAD_AES_256_GCM fixtures the way a real WeChat Pay server
// would produce one -- this package's own locally-constructed fixture
// strategy (doc.go's own testing-strategy section).
func encryptResourceForTest(t *testing.T, plaintext []byte, associatedData string, apiV3Key []byte) (nonce, ciphertextB64 string) {
	t.Helper()
	block, err := aes.NewCipher(apiV3Key)
	if err != nil {
		t.Fatalf("new AES cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("new GCM: %v", err)
	}

	nonceBytes := make([]byte, gcm.NonceSize())
	idx := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(idx); err != nil {
		t.Fatalf("generate nonce: %v", err)
	}
	for i, b := range idx {
		nonceBytes[i] = nonceTestAlphabet[int(b)%len(nonceTestAlphabet)]
	}

	ciphertext := gcm.Seal(nil, nonceBytes, plaintext, []byte(associatedData))
	return string(nonceBytes), base64.StdEncoding.EncodeToString(ciphertext)
}

func TestDecryptResource_RoundTrip(t *testing.T) {
	apiV3Key := testAPIv3Key()
	plaintext := []byte(`{"out_trade_no":"ORD1","trade_state":"SUCCESS"}`)
	nonce, ciphertextB64 := encryptResourceForTest(t, plaintext, "transaction", apiV3Key)

	got, err := decryptResource(algorithmAEADAES256GCM, nonce, "transaction", ciphertextB64, apiV3Key)
	if err != nil {
		t.Fatalf("decryptResource: %v", err)
	}
	if string(got) != string(plaintext) {
		t.Errorf("decryptResource = %q, want %q", got, plaintext)
	}
}

func TestDecryptResource_WrongKey(t *testing.T) {
	apiV3Key := testAPIv3Key()
	wrongKey := []byte("11111111111111111111111111111111")[:32]
	nonce, ciphertextB64 := encryptResourceForTest(t, []byte(`{"a":1}`), "transaction", apiV3Key)

	if _, err := decryptResource(algorithmAEADAES256GCM, nonce, "transaction", ciphertextB64, wrongKey); err == nil {
		t.Error("decryptResource accepted the wrong APIv3 key")
	}
}

func TestDecryptResource_TamperedCiphertext(t *testing.T) {
	apiV3Key := testAPIv3Key()
	nonce, ciphertextB64 := encryptResourceForTest(t, []byte(`{"a":1}`), "transaction", apiV3Key)

	raw, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		t.Fatalf("decode ciphertext: %v", err)
	}
	raw[0] ^= 0xFF // flip a bit after encryption
	tampered := base64.StdEncoding.EncodeToString(raw)

	if _, err := decryptResource(algorithmAEADAES256GCM, nonce, "transaction", tampered, apiV3Key); err == nil {
		t.Error("decryptResource accepted tampered ciphertext")
	}
}

func TestDecryptResource_WrongAssociatedData(t *testing.T) {
	apiV3Key := testAPIv3Key()
	nonce, ciphertextB64 := encryptResourceForTest(t, []byte(`{"a":1}`), "transaction", apiV3Key)

	if _, err := decryptResource(algorithmAEADAES256GCM, nonce, "wrong-context", ciphertextB64, apiV3Key); err == nil {
		t.Error("decryptResource accepted the wrong associated_data")
	}
}

func TestDecryptResource_UnsupportedAlgorithm(t *testing.T) {
	apiV3Key := testAPIv3Key()
	nonce, ciphertextB64 := encryptResourceForTest(t, []byte(`{"a":1}`), "transaction", apiV3Key)

	if _, err := decryptResource("AEAD_AES_128_GCM", nonce, "transaction", ciphertextB64, apiV3Key); err == nil {
		t.Error("decryptResource accepted an unsupported algorithm")
	}
}

func TestDecryptResource_InvalidKeyLength(t *testing.T) {
	nonce, ciphertextB64 := encryptResourceForTest(t, []byte(`{"a":1}`), "transaction", testAPIv3Key())
	if _, err := decryptResource(algorithmAEADAES256GCM, nonce, "transaction", ciphertextB64, []byte("too-short")); err == nil {
		t.Error("decryptResource accepted a non-32-byte APIv3 key")
	}
}
