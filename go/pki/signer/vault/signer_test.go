package vault

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
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

func TestSigner_GenerateKey_RejectsUnsupportedAlgorithm(t *testing.T) {
	s := &signer{logical: &fakeTransitClient{}, mountPath: "transit", mode: ModeDirectSign}
	_, _, err := s.GenerateKey(context.Background(), "ecdsa-p256")
	if !apperr.HasCode(err, pki.ErrAlgorithmUnsupportedBySigner.Code) {
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
	var gotInput, gotKeyVersion any
	fake := &fakeTransitClient{
		write: func(_ context.Context, path string, data map[string]interface{}) (*vaultapi.Secret, error) {
			signedPath = path
			gotInput, _ = data["input"].(string)
			gotKeyVersion = data["key_version"]
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
	if gotKeyVersion != strconv.Itoa(directSignPinnedKeyVersion) {
		t.Errorf("sign request key_version = %v, want %d (the request must pin the version this package issued the key at, never default to Vault's latest)", gotKeyVersion, directSignPinnedKeyVersion)
	}
}

// TestSigner_DirectMode_Sign_EnvelopeVersionBeyondThePin_IsRefused pins the
// response half of the pin: when a sign answer's envelope names a Transit
// key version other than directSignPinnedKeyVersion, the signature must be
// refused, never returned -- an in-place rotation that actually changed
// which version signed (or a Vault build that ignored the request's
// key_version parameter) must surface as a loud error, not as a signature
// no exported key verifies. Before the pin landed, signDirect accepted any
// well-formed envelope version and returned the signature; this test
// failed on the unpinned code with error = <nil>.
func TestSigner_DirectMode_Sign_EnvelopeVersionBeyondThePin_IsRefused(t *testing.T) {
	message := []byte("pin mismatch probe")
	fake := &fakeTransitClient{
		write: func(context.Context, string, map[string]interface{}) (*vaultapi.Secret, error) {
			return &vaultapi.Secret{Data: map[string]interface{}{
				// v2: a signature the key's SECOND version produced. The
				// request pinned version 1, so this answer can only mean the
				// pin did not govern which version signed.
				"signature": "vault:v2:" + base64.StdEncoding.EncodeToString(make([]byte, 64)),
			}}, nil
		},
	}
	s := &signer{logical: fake, mountPath: "transit", mode: ModeDirectSign}
	sig, err := s.Sign(context.Background(), "my-key", message)
	if err == nil {
		t.Fatalf("Sign(pinned v1, answered v2) = (%d bytes, nil), want an error naming the version mismatch", len(sig))
	}
	for _, want := range []string{"key version 2", "pins version 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Sign(pinned v1, answered v2) error = %q, want it to name %q", err, want)
		}
	}
}

// TestSigner_DirectMode_Public_ServesThePinnedVersionNotTheLatest pins the
// public-key half of the pin: readPublicKey must answer the version
// directSignPinnedKeyVersion's key -- the version the module issued and
// exported -- not the Transit key's latest_version, so an in-place
// rotation cannot make the advertised key diverge from the pinned
// signatures. The fixture carries both versions (Vault keeps every
// version's entry in the keys map) with latest_version = 2, exactly the
// state a rotated key presents. Before the pin landed, Public served the
// latest version's key; this test failed on the unpinned code with the
// rotated key returned.
func TestSigner_DirectMode_Public_ServesThePinnedVersionNotTheLatest(t *testing.T) {
	pinnedPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	rotatedPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}

	fake := &fakeTransitClient{
		read: func(context.Context, string) (*vaultapi.Secret, error) {
			return &vaultapi.Secret{Data: map[string]interface{}{
				"latest_version": float64(2),
				"keys": map[string]interface{}{
					"1": map[string]interface{}{
						"public_key": base64.StdEncoding.EncodeToString(pinnedPub),
					},
					"2": map[string]interface{}{
						"public_key": base64.StdEncoding.EncodeToString(rotatedPub),
					},
				},
			}}, nil
		},
	}
	s := &signer{logical: fake, mountPath: "transit", mode: ModeDirectSign}
	pub, err := s.Public(context.Background(), "my-key")
	if err != nil {
		t.Fatalf("Public: %v", err)
	}
	if !pub.(ed25519.PublicKey).Equal(pinnedPub) {
		t.Errorf("Public() = %x, want the pinned version's key %x (a rotated key's latest version must never be served as this keyRef's key)", pub, pinnedPub)
	}
	if pub.(ed25519.PublicKey).Equal(rotatedPub) {
		t.Error("Public() returned the rotated key's latest-version key, want the pinned version's key")
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
	if !apperr.HasCode(err, pki.ErrKeyNotFound.Code) {
		t.Errorf("Sign(unknown keyRef) error = %v, want ErrKeyNotFound", err)
	}
}

// TestParseVaultSignatureEnvelope_RejectsMalformedVersion pins the version
// field of Vault's "vault:v<N>:" envelope being parsed and validated rather
// than dropped unread: "vault:abc:<valid base64>" (and the other malformed
// shapes below) must fail to parse, because the version is the pin's
// verification signal -- signDirect compares it against
// directSignPinnedKeyVersion -- and a version it cannot read is a signature
// it cannot attribute to the key it exported.
func TestParseVaultSignatureEnvelope_RejectsMalformedVersion(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString(make([]byte, 64))
	for _, envelope := range []string{
		"vault:abc:" + payload,
		"vault:1:" + payload,
		"vault:v0:" + payload,
		"vault:v-3:" + payload,
	} {
		if _, _, err := parseVaultSignatureEnvelope(envelope); err == nil {
			t.Errorf("parseVaultSignatureEnvelope(%q) error = nil, want one (the version must match Vault's own \"v<N>\" spelling with N a positive integer)", envelope)
		}
	}
}

// TestParseVaultSignatureEnvelope_RejectsEmptySignature pins the
// empty-signature-is-never-success half of the seam's failure semantics on
// this side of the twin: a well-formed envelope with nothing after the
// version ("vault:v1:") base64-decodes to zero bytes, and that empty answer
// must be an error -- the vault twin answers the same shape the kmsaws
// twin's own regression test covers (an empty Signature field there).
func TestParseVaultSignatureEnvelope_RejectsEmptySignature(t *testing.T) {
	if _, _, err := parseVaultSignatureEnvelope("vault:v1:"); err == nil {
		t.Error("parseVaultSignatureEnvelope(\"vault:v1:\") error = nil, want one (an empty signature must never decode to success)")
	}
}

// TestParseVaultSignatureEnvelope_ReturnsTheVersionAndSignature pins the
// parse function's full contract now that the version is load-bearing: a
// well-formed envelope yields both the version it names and the decoded
// signature bytes.
func TestParseVaultSignatureEnvelope_ReturnsTheVersionAndSignature(t *testing.T) {
	raw := make([]byte, 64)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	version, sig, err := parseVaultSignatureEnvelope("vault:v3:" + base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("parseVaultSignatureEnvelope: %v", err)
	}
	if version != 3 {
		t.Errorf("version = %d, want 3", version)
	}
	if !bytes.Equal(sig, raw) {
		t.Error("signature bytes differ from the encoded payload")
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
	if !apperr.HasCode(err, pki.ErrKeyNotFound.Code) {
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
