package vault

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"testing"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pki"
)

// fakeTransitClient is a scripted transitClient double, letting this file
// pin the exact Vault Transit request/response shapes this package's
// GenerateKey/Sign/Public/Destroy build and parse, without a real Vault
// server -- see doc.go's own note on why no offline-runnable Example
// against a live Transit engine exists in this package.
type fakeTransitClient struct {
	write  func(ctx context.Context, path string, data map[string]interface{}) (*vaultapi.Secret, error)
	read   func(ctx context.Context, path string) (*vaultapi.Secret, error)
	delete func(ctx context.Context, path string) (*vaultapi.Secret, error)
}

func (f *fakeTransitClient) WriteWithContext(ctx context.Context, path string, data map[string]interface{}) (*vaultapi.Secret, error) {
	if f.write == nil {
		return nil, fmt.Errorf("unexpected Write to %q", path)
	}
	return f.write(ctx, path, data)
}

func (f *fakeTransitClient) ReadWithContext(ctx context.Context, path string) (*vaultapi.Secret, error) {
	if f.read == nil {
		return nil, fmt.Errorf("unexpected Read of %q", path)
	}
	return f.read(ctx, path)
}

func (f *fakeTransitClient) DeleteWithContext(ctx context.Context, path string) (*vaultapi.Secret, error) {
	if f.delete == nil {
		return nil, fmt.Errorf("unexpected Delete of %q", path)
	}
	return f.delete(ctx, path)
}

// compile-time check that *fakeTransitClient satisfies transitClient.
var _ transitClient = (*fakeTransitClient)(nil)

// isKeyNotFound reports whether err is (a decorated instance of)
// pki.ErrKeyNotFound, matching on Code the way every *apperr.Error sentinel
// in this codebase must be compared (see go/pki/errors.go's own doc
// comment) -- WithParam/WithCause always derive a new pointer, so a plain
// errors.Is against the package-level sentinel is not the right check here.
func isKeyNotFound(err error) bool {
	found, ok := apperr.As(err)
	return ok && found.Code == pki.ErrKeyNotFound.Code
}

func TestSigner_GenerateKey_RejectsUnsupportedAlgorithm(t *testing.T) {
	s := &signer{logical: &fakeTransitClient{}, mountPath: "transit", mode: ModeDirectSign}
	_, _, err := s.GenerateKey(context.Background(), "ecdsa-p256")
	found, ok := apperr.As(err)
	if !ok || found.Code != pki.ErrAlgorithmUnsupportedBySigner.Code {
		t.Fatalf("GenerateKey(unsupported) error = %v, want ErrAlgorithmUnsupportedBySigner", err)
	}
}

func TestSigner_DirectMode_GenerateKey_CreatesKeyEnablesDeletionAndReadsPublicKey(t *testing.T) {
	realPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}

	var createdPath, configuredPath, readPath string
	fake := &fakeTransitClient{
		write: func(_ context.Context, path string, data map[string]interface{}) (*vaultapi.Secret, error) {
			switch {
			case data["type"] == "ed25519":
				createdPath = path
				if data["exportable"] != false {
					t.Errorf("create transit key: exportable = %v, want false", data["exportable"])
				}
				return &vaultapi.Secret{}, nil
			case data["deletion_allowed"] == true:
				configuredPath = path
				return &vaultapi.Secret{}, nil
			default:
				return nil, fmt.Errorf("unexpected write data %v to %q", data, path)
			}
		},
		read: func(_ context.Context, path string) (*vaultapi.Secret, error) {
			readPath = path
			return &vaultapi.Secret{Data: map[string]interface{}{
				"latest_version": float64(1),
				"keys": map[string]interface{}{
					"1": map[string]interface{}{
						"public_key": base64.StdEncoding.EncodeToString(realPub),
					},
				},
			}}, nil
		},
	}

	s := &signer{logical: fake, mountPath: "transit", mode: ModeDirectSign}
	keyRef, pub, err := s.GenerateKey(context.Background(), pki.AlgorithmEd25519)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if keyRef == "" {
		t.Fatal("GenerateKey returned an empty keyRef")
	}
	if !pub.(ed25519.PublicKey).Equal(realPub) {
		t.Errorf("GenerateKey public key = %x, want %x", pub, realPub)
	}
	if createdPath != "transit/keys/"+keyRef {
		t.Errorf("create path = %q, want %q", createdPath, "transit/keys/"+keyRef)
	}
	if configuredPath != "transit/keys/"+keyRef+"/config" {
		t.Errorf("deletion_allowed path = %q, want %q", configuredPath, "transit/keys/"+keyRef+"/config")
	}
	if readPath != "transit/keys/"+keyRef {
		t.Errorf("public key read path = %q, want %q", readPath, "transit/keys/"+keyRef)
	}
}

func TestSigner_DirectMode_Sign_DecodesVaultSignatureEnvelope(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	message := []byte("pki vault direct-sign round trip")
	realSig := ed25519.Sign(priv, message)

	var signedPath string
	var gotInput string
	fake := &fakeTransitClient{
		write: func(_ context.Context, path string, data map[string]interface{}) (*vaultapi.Secret, error) {
			signedPath = path
			gotInput, _ = data["input"].(string)
			return &vaultapi.Secret{Data: map[string]interface{}{
				"signature": "vault:v1:" + base64.StdEncoding.EncodeToString(realSig),
			}}, nil
		},
	}

	s := &signer{logical: fake, mountPath: "transit", mode: ModeDirectSign}
	sig, err := s.Sign(context.Background(), "my-key", message)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !ed25519.Verify(pub, message, sig) {
		t.Error("ed25519.Verify failed for the decoded signature")
	}
	if signedPath != "transit/sign/my-key" {
		t.Errorf("sign path = %q, want %q", signedPath, "transit/sign/my-key")
	}
	wantInput := base64.StdEncoding.EncodeToString(message)
	if gotInput != wantInput {
		t.Errorf("sign input = %q, want %q", gotInput, wantInput)
	}
}

func TestSigner_DirectMode_Sign_UnknownKeyRef(t *testing.T) {
	fake := &fakeTransitClient{
		write: func(context.Context, string, map[string]interface{}) (*vaultapi.Secret, error) {
			return nil, nil
		},
	}
	s := &signer{logical: fake, mountPath: "transit", mode: ModeDirectSign}
	_, err := s.Sign(context.Background(), "does-not-exist", []byte("x"))
	if !isKeyNotFound(err) {
		t.Errorf("Sign(unknown keyRef) error = %v, want ErrKeyNotFound", err)
	}
}

func TestSigner_DirectMode_Destroy_DeletesTheTransitKey(t *testing.T) {
	var deletedPath string
	fake := &fakeTransitClient{
		delete: func(_ context.Context, path string) (*vaultapi.Secret, error) {
			deletedPath = path
			return &vaultapi.Secret{}, nil
		},
	}
	s := &signer{logical: fake, mountPath: "transit", mode: ModeDirectSign}
	if err := s.Destroy(context.Background(), "my-key"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if deletedPath != "transit/keys/my-key" {
		t.Errorf("delete path = %q, want %q", deletedPath, "transit/keys/my-key")
	}
}

func TestSigner_EnvelopeMode_GenerateKeyThenSignAndPublic_RoundTrip(t *testing.T) {
	// A fake "wrapping key" that really encrypts/decrypts, so this test
	// exercises the same ciphertext-round-trips-through-keyRef path a real
	// Vault Transit encrypt/decrypt pair would.
	wrap := newFakeWrap(t)
	fake := &fakeTransitClient{
		write: func(_ context.Context, path string, data map[string]interface{}) (*vaultapi.Secret, error) {
			switch path {
			case "transit/encrypt/wrap-key":
				plaintext, err := base64.StdEncoding.DecodeString(data["plaintext"].(string))
				if err != nil {
					return nil, err
				}
				return &vaultapi.Secret{Data: map[string]interface{}{
					"ciphertext": wrap.encrypt(plaintext),
				}}, nil
			case "transit/decrypt/wrap-key":
				plaintext, err := wrap.decrypt(data["ciphertext"].(string))
				if err != nil {
					return nil, err
				}
				return &vaultapi.Secret{Data: map[string]interface{}{
					"plaintext": base64.StdEncoding.EncodeToString(plaintext),
				}}, nil
			default:
				return nil, fmt.Errorf("unexpected write path %q", path)
			}
		},
	}

	s := &signer{logical: fake, mountPath: "transit", mode: ModeEnvelope, wrappingKeyName: "wrap-key"}
	ctx := context.Background()

	keyRef, pub, err := s.GenerateKey(ctx, pki.AlgorithmEd25519)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if keyRef == "" {
		t.Fatal("GenerateKey returned an empty keyRef")
	}
	// The keyRef must be a real Vault ciphertext envelope, per Mode's own
	// doc comment -- not the raw key material.
	if got := keyRef[:len("vault:v1:")]; got != "vault:v1:" {
		t.Errorf("keyRef = %q, want it to start with the vault:v1: envelope", keyRef)
	}

	gotPub, err := s.Public(ctx, keyRef)
	if err != nil {
		t.Fatalf("Public: %v", err)
	}
	if !gotPub.(ed25519.PublicKey).Equal(pub.(ed25519.PublicKey)) {
		t.Errorf("Public() = %x, want %x", gotPub, pub)
	}

	message := []byte("pki vault envelope round trip")
	sig, err := s.Sign(ctx, keyRef, message)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !ed25519.Verify(pub.(ed25519.PublicKey), message, sig) {
		t.Error("ed25519.Verify failed for the envelope-mode signature")
	}
}

func TestSigner_EnvelopeMode_Destroy_ValidatesThenNoOps(t *testing.T) {
	wrap := newFakeWrap(t)
	var decryptCalls int
	fake := &fakeTransitClient{
		write: func(_ context.Context, path string, data map[string]interface{}) (*vaultapi.Secret, error) {
			switch path {
			case "transit/encrypt/wrap-key":
				plaintext, err := base64.StdEncoding.DecodeString(data["plaintext"].(string))
				if err != nil {
					return nil, err
				}
				return &vaultapi.Secret{Data: map[string]interface{}{"ciphertext": wrap.encrypt(plaintext)}}, nil
			case "transit/decrypt/wrap-key":
				decryptCalls++
				plaintext, err := wrap.decrypt(data["ciphertext"].(string))
				if err != nil {
					return nil, err
				}
				return &vaultapi.Secret{Data: map[string]interface{}{"plaintext": base64.StdEncoding.EncodeToString(plaintext)}}, nil
			default:
				return nil, fmt.Errorf("unexpected write path %q", path)
			}
		},
	}
	s := &signer{logical: fake, mountPath: "transit", mode: ModeEnvelope, wrappingKeyName: "wrap-key"}
	ctx := context.Background()

	keyRef, _, err := s.GenerateKey(ctx, pki.AlgorithmEd25519)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if err := s.Destroy(ctx, keyRef); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if decryptCalls == 0 {
		t.Error("Destroy did not validate keyRef by attempting a decrypt")
	}
}

func TestSigner_EnvelopeMode_Sign_InvalidKeyRef(t *testing.T) {
	fake := &fakeTransitClient{
		write: func(context.Context, string, map[string]interface{}) (*vaultapi.Secret, error) {
			return nil, nil
		},
	}
	s := &signer{logical: fake, mountPath: "transit", mode: ModeEnvelope, wrappingKeyName: "wrap-key"}
	_, err := s.Sign(context.Background(), "not-a-real-ciphertext", []byte("x"))
	if !isKeyNotFound(err) {
		t.Errorf("Sign(invalid keyRef) error = %v, want ErrKeyNotFound", err)
	}
}

func TestNewSigner_Validation(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "missing address", cfg: Config{Token: "t"}},
		{name: "missing token", cfg: Config{Address: "https://vault.example.com"}},
		{name: "envelope mode missing wrapping key", cfg: Config{Address: "https://vault.example.com", Token: "t", Mode: ModeEnvelope}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewSigner(tt.cfg); err == nil {
				t.Fatal("NewSigner() error = nil, want one")
			}
		})
	}
}

func TestNewSigner_ValidConfig_DialsNothing(t *testing.T) {
	// NewSigner must succeed without any network access -- the Vault client
	// connects lazily, exactly like this codebase's other built-in seam
	// clients.
	if _, err := NewSigner(Config{
		Address:         "https://vault.invalid.example:8200",
		Token:           "t",
		Mode:            ModeDirectSign,
		WrappingKeyName: "unused-in-direct-mode",
	}); err != nil {
		t.Fatalf("NewSigner() error = %v, want nil", err)
	}
}

// fakeWrap is a tiny, test-only stand-in for a Vault Transit wrapping key:
// it "encrypts" by base64-armoring plaintext behind a "vault:v1:" prefix
// (Vault's own envelope format) and "decrypts" by reversing that, letting
// the envelope-mode tests exercise the real request/response shaping code
// without needing real AES-GCM.
type fakeWrap struct{ t *testing.T }

func newFakeWrap(t *testing.T) *fakeWrap { return &fakeWrap{t: t} }

func (w *fakeWrap) encrypt(plaintext []byte) string {
	return "vault:v1:" + base64.StdEncoding.EncodeToString(plaintext)
}

func (w *fakeWrap) decrypt(ciphertext string) ([]byte, error) {
	const prefix = "vault:v1:"
	if len(ciphertext) < len(prefix) || ciphertext[:len(prefix)] != prefix {
		return nil, fmt.Errorf("fakeWrap: not a recognized ciphertext: %q", ciphertext)
	}
	return base64.StdEncoding.DecodeString(ciphertext[len(prefix):])
}

// pkcs8Roundtrip is exercised implicitly through GenerateKey/Sign above;
// this direct call pins that x509.MarshalPKCS8PrivateKey/ParsePKCS8PrivateKey
// agree on ed25519.PrivateKey, guarding against a stdlib behaviour change.
func TestPKCS8Roundtrip_Ed25519(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		t.Fatalf("ParsePKCS8PrivateKey: %v", err)
	}
	if !parsed.(ed25519.PrivateKey).Equal(priv) {
		t.Error("PKCS8 round trip did not preserve the private key")
	}
}
