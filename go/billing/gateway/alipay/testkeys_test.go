package alipay

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
)

// generateTestKeyPair returns a freshly generated 2048-bit RSA key pair,
// PEM-encoded exactly like a real Alipay open-platform key export (PKCS#8
// private key, PKIX public key) -- this package's own sanctioned
// locally-constructed-fixture strategy (doc.go's own testing-strategy
// section), since Alipay ships no published third-party signature test
// vector the way Stripe's SDK does.
func generateTestKeyPair(t *testing.T) (privPEM, pubPEM []byte, priv *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	privBytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	privPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})

	pubBytes, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	pubPEM = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})

	return privPEM, pubPEM, key
}
