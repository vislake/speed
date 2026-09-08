package attestation

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

// TestNewMessage_CanonicalAndDeterministic pins the byte stability the
// whole design rests on: the same object, content and tenant always
// marshal to the same message bytes, so the message a signature is made
// over at attestation time is exactly the message the gate parses and
// re-verifies at serve time, and the digest inside is the hex SHA-256 of
// the content.
func TestNewMessage_CanonicalAndDeterministic(t *testing.T) {
	content := []byte("simulated smile bytes")
	first, err := newMessage("obj-1", "tenant-acme", content)
	if err != nil {
		t.Fatalf("newMessage: %v", err)
	}
	second, err := newMessage("obj-1", "tenant-acme", content)
	if err != nil {
		t.Fatalf("newMessage again: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("newMessage is not deterministic:\n%q\n%q", first, second)
	}

	parsed, err := parseMessage(first)
	if err != nil {
		t.Fatalf("parseMessage: %v", err)
	}
	if parsed.ObjectID != "obj-1" || parsed.TenantID != "tenant-acme" {
		t.Errorf("parsed message = %+v, want obj-1 / tenant-acme", parsed)
	}
	if len(parsed.ContentSHA256) != 64 {
		t.Errorf("ContentSHA256 = %q, want a 64-hex-char SHA-256", parsed.ContentSHA256)
	}

	// A different digest must change the message -- the content is bound,
	// not the object id alone.
	other, err := newMessage("obj-1", "tenant-acme", []byte("other bytes"))
	if err != nil {
		t.Fatalf("newMessage other content: %v", err)
	}
	if string(other) == string(first) {
		t.Fatalf("different content produced the same message")
	}
}

// testLeaf builds a self-signed end-entity-shaped certificate over a fresh
// ed25519 key, the parsed-leaf shape verifyContent consumes (in the real
// flow the leaf comes from CAService.VerifyCertificate).
func testLeaf(t *testing.T, pub ed25519.PublicKey, priv ed25519.PrivateKey) *x509.Certificate {
	t.Helper()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "test leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return leaf
}

// TestVerifyContent_HappyPath pins the gate's success shape: a signature
// over the stored message verifies with the leaf's public key and the
// live content digest matches the attested one.
func TestVerifyContent_HappyPath(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	leaf := testLeaf(t, pub, priv)

	content := []byte("the exact bytes about to be served")
	message, err := newMessage("obj-1", "tenant-acme", content)
	if err != nil {
		t.Fatalf("newMessage: %v", err)
	}
	signature := ed25519.Sign(priv, message)

	if err := verifyContent(leaf, message, signature, "obj-1", "tenant-acme", content); err != nil {
		t.Fatalf("verifyContent(happy path): %v", err)
	}
}

// TestVerifyContent_Refusals pins each gate stage: a signature made over a
// different message, a message naming another object or tenant, content
// whose digest drifted from the attested one, and a leaf whose key is not
// ed25519 -- every stage answers the single ErrAttestationFailed shape,
// carrying the stage only in the wrapped text.
func TestVerifyContent_Refusals(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	leaf := testLeaf(t, pub, priv)

	content := []byte("attested bytes")
	message, err := newMessage("obj-1", "tenant-acme", content)
	if err != nil {
		t.Fatalf("newMessage: %v", err)
	}
	signature := ed25519.Sign(priv, message)

	// Tampered signature (over different bytes).
	wrongSig := ed25519.Sign(priv, []byte("something else"))
	if verifyErr := verifyContent(leaf, message, wrongSig, "obj-1", "tenant-acme", content); !errors.Is(verifyErr, ErrAttestationFailed) {
		t.Errorf("wrong signature error = %v, want ErrAttestationFailed", verifyErr)
	}

	// Message naming another object.
	otherObject, err := newMessage("obj-2", "tenant-acme", content)
	if err != nil {
		t.Fatalf("newMessage: %v", err)
	}
	otherSig := ed25519.Sign(priv, otherObject)
	if verifyErr := verifyContent(leaf, otherObject, otherSig, "obj-1", "tenant-acme", content); !errors.Is(verifyErr, ErrAttestationFailed) {
		t.Errorf("other-object message error = %v, want ErrAttestationFailed", verifyErr)
	}

	// Message naming another tenant.
	otherTenant, err := newMessage("obj-1", "tenant-globex", content)
	if err != nil {
		t.Fatalf("newMessage: %v", err)
	}
	tenantSig := ed25519.Sign(priv, otherTenant)
	if verifyErr := verifyContent(leaf, otherTenant, tenantSig, "obj-1", "tenant-acme", content); !errors.Is(verifyErr, ErrAttestationFailed) {
		t.Errorf("other-tenant message error = %v, want ErrAttestationFailed", verifyErr)
	}

	// Content whose bytes drifted from the attested digest -- the tamper
	// refusal the sharing gate exists for.
	if verifyErr := verifyContent(leaf, message, signature, "obj-1", "tenant-acme", []byte("tampered bytes")); !errors.Is(verifyErr, ErrAttestationFailed) {
		t.Errorf("drifted content error = %v, want ErrAttestationFailed", verifyErr)
	}

	// A malformed message never verifies.
	if verifyErr := verifyContent(leaf, []byte("{not json"), signature, "obj-1", "tenant-acme", content); !errors.Is(verifyErr, ErrAttestationFailed) {
		t.Errorf("malformed message error = %v, want ErrAttestationFailed", verifyErr)
	}

	// The refusal shape is single and the stage is not on the wire.
	if verifyErr := verifyContent(leaf, message, wrongSig, "obj-1", "tenant-acme", content); verifyErr != nil {
		if !strings.Contains(verifyErr.Error(), "signature") {
			t.Errorf("refusal error %q does not name the refusing stage for the log", verifyErr)
		}
	}
}
