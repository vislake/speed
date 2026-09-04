package vault

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/pki"
)

// defaultMountPath is Vault's own default Transit secrets engine mount
// point, used when Config.MountPath is empty.
const defaultMountPath = "transit"

// transitClient is the subset of *vaultapi.Logical this package calls,
// declared as its own interface so unit tests can inject a scripted fake
// without a real Vault server -- docs/internal/22-pki.md's testing-strategy
// note that AWS KMS gets its SDK interface stubbed for unit tests applies
// equally well here, for the request/response shaping logic that does not
// need a real Transit engine's network behaviour to prove. *vaultapi.Logical
// already has every one of these
// methods with this exact signature, so it satisfies transitClient
// structurally -- no adapter type is needed anywhere in this package.
type transitClient interface {
	WriteWithContext(ctx context.Context, path string, data map[string]interface{}) (*vaultapi.Secret, error)
	ReadWithContext(ctx context.Context, path string) (*vaultapi.Secret, error)
	DeleteWithContext(ctx context.Context, path string) (*vaultapi.Secret, error)
}

// signer is this package's pki.Signer implementation, covering both Mode
// values -- see each method's own doc comment for how the two diverge.
type signer struct {
	logical         transitClient
	mountPath       string
	mode            Mode
	wrappingKeyName string
}

// NewSigner returns a pki.Signer backed by cfg's Vault Transit engine
// mount. Nothing is dialed here: the underlying Vault client connects
// lazily, on first use, exactly like every other built-in seam's client in
// this codebase (go-redis, and the S3/SMTP clients pkgcore's own
// builtin_implementations.go and objectstore/s3 build). An unusable
// configuration -- an empty Address or Token, or ModeEnvelope without
// WrappingKeyName -- returns an error rather than panicking, which is a
// deliberate departure from pkgcore's own S3/SMTP constructors (which
// panic on an unusable Config): those are called only from trusted,
// hand-written host wiring, while this constructor is also reachable from
// registerFromConfig (register.go), itself reachable from
// pkgcore.SeamRegistry.Build -- a call site whose contract is "return an
// error", never "panic", the same reason objectstore.s3's own
// objectStoreFromConfig checks its required fields before ever calling the
// panicking NewObjectStore.
func NewSigner(cfg Config) (pki.Signer, error) {
	if cfg.Address == "" {
		return nil, fmt.Errorf("pki/signer/vault: NewSigner requires a non-empty Config.Address")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("pki/signer/vault: NewSigner requires a non-empty Config.Token")
	}
	if cfg.Mode == ModeEnvelope && cfg.WrappingKeyName == "" {
		return nil, fmt.Errorf("pki/signer/vault: NewSigner requires a non-empty Config.WrappingKeyName in ModeEnvelope")
	}

	vc := vaultapi.DefaultConfig()
	if vc.Error != nil {
		return nil, fmt.Errorf("pki/signer/vault: %w", vc.Error)
	}
	vc.Address = cfg.Address
	client, err := vaultapi.NewClient(vc)
	if err != nil {
		return nil, fmt.Errorf("pki/signer/vault: %w", err)
	}
	client.SetToken(cfg.Token)
	if cfg.Namespace != "" {
		client.SetNamespace(cfg.Namespace)
	}

	mountPath := cfg.MountPath
	if mountPath == "" {
		mountPath = defaultMountPath
	}

	return &signer{
		logical:         client.Logical(),
		mountPath:       mountPath,
		mode:            cfg.Mode,
		wrappingKeyName: cfg.WrappingKeyName,
	}, nil
}

// GenerateKey implements pki.Signer. Only pki.AlgorithmEd25519 is
// supported -- Vault Transit's ed25519 key type direct-signs the complete
// message (docs/internal/22-pki.md's Signer section: PureEdDSA, RFC 8037's
// JWT EdDSA), matching pki.Signer.Sign's own documented, algorithm-
// dependent input contract exactly.
func (s *signer) GenerateKey(ctx context.Context, algorithm string) (string, crypto.PublicKey, error) {
	if algorithm != pki.AlgorithmEd25519 {
		return "", nil, pki.ErrAlgorithmUnsupportedBySigner.WithParam("algorithm", algorithm)
	}
	if s.mode == ModeDirectSign {
		return s.generateKeyDirect(ctx)
	}
	return s.generateKeyEnvelope(ctx)
}

// generateKeyDirect creates a new, non-exportable ed25519 Transit key and
// returns its name as keyRef. Vault refuses to DELETE a key whose
// deletion_allowed is not explicitly set, so this also flips that setting
// right away -- otherwise Destroy could never succeed for a key this
// method created.
func (s *signer) generateKeyDirect(ctx context.Context) (string, crypto.PublicKey, error) {
	name := "pki-" + uuid.NewString()
	keyPath := s.mountPath + "/keys/" + name

	if _, err := s.logical.WriteWithContext(ctx, keyPath, map[string]interface{}{
		"type":       "ed25519",
		"exportable": false,
	}); err != nil {
		return "", nil, fmt.Errorf("pki/signer/vault: create transit key %q: %w", name, err)
	}
	if _, err := s.logical.WriteWithContext(ctx, keyPath+"/config", map[string]interface{}{
		"deletion_allowed": true,
	}); err != nil {
		return "", nil, fmt.Errorf("pki/signer/vault: enable deletion for transit key %q: %w", name, err)
	}

	pub, err := s.readPublicKey(ctx, name)
	if err != nil {
		return "", nil, err
	}
	return name, pub, nil
}

// generateKeyEnvelope generates a real ed25519 key pair in this process,
// then immediately wraps the private half with Vault Transit's encrypt
// operation -- see Mode's own doc comment for why the resulting ciphertext
// is the keyRef this returns, rather than a name pointing at storage this
// package keeps.
func (s *signer) generateKeyEnvelope(ctx context.Context) (string, crypto.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, fmt.Errorf("pki/signer/vault: generate ed25519 key: %w", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", nil, fmt.Errorf("pki/signer/vault: marshal private key: %w", err)
	}

	ciphertext, err := s.encrypt(ctx, pkcs8)
	if err != nil {
		return "", nil, err
	}
	return ciphertext, pub, nil
}

// Sign implements pki.Signer. input is the complete message, per
// pki.Signer.Sign's own algorithm-dependent contract for
// pki.AlgorithmEd25519.
func (s *signer) Sign(ctx context.Context, keyRef string, input []byte) ([]byte, error) {
	if s.mode == ModeDirectSign {
		return s.signDirect(ctx, keyRef, input)
	}
	return s.signEnvelope(ctx, keyRef, input)
}

// signDirect asks Vault Transit to sign input with the key named keyRef.
// The private key never leaves Vault for this call -- it is a single API
// round trip, exactly the direct-sign contract docs/internal/22-pki.md's
// Signer section describes.
func (s *signer) signDirect(ctx context.Context, keyRef string, input []byte) ([]byte, error) {
	secret, err := s.logical.WriteWithContext(ctx, s.mountPath+"/sign/"+keyRef, map[string]interface{}{
		"input": base64.StdEncoding.EncodeToString(input),
	})
	if err != nil {
		return nil, fmt.Errorf("pki/signer/vault: sign with %q: %w", keyRef, err)
	}
	if secret == nil {
		return nil, pki.ErrKeyNotFound
	}
	encoded, ok := secret.Data["signature"].(string)
	if !ok {
		return nil, fmt.Errorf("pki/signer/vault: sign response for %q has no \"signature\" field", keyRef)
	}
	return decodeVaultSignature(encoded)
}

// signEnvelope decrypts keyRef back into a private key for the duration of
// this call only, signs locally, and lets the decrypted key go out of
// scope -- the same "decrypted key never outlives one call" discipline
// pki.LocalSigner.Sign documents for its own dbkit-encrypted column.
func (s *signer) signEnvelope(ctx context.Context, keyRef string, input []byte) ([]byte, error) {
	priv, err := s.decryptPrivateKey(ctx, keyRef)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(priv, input), nil
}

// Public implements pki.Signer.
func (s *signer) Public(ctx context.Context, keyRef string) (crypto.PublicKey, error) {
	if s.mode == ModeDirectSign {
		return s.readPublicKey(ctx, keyRef)
	}
	priv, err := s.decryptPrivateKey(ctx, keyRef)
	if err != nil {
		return nil, err
	}
	return priv.Public(), nil
}

// Destroy implements pki.Signer.
func (s *signer) Destroy(ctx context.Context, keyRef string) error {
	if s.mode == ModeDirectSign {
		if _, err := s.logical.DeleteWithContext(ctx, s.mountPath+"/keys/"+keyRef); err != nil {
			return fmt.Errorf("pki/signer/vault: delete transit key %q: %w", keyRef, err)
		}
		return nil
	}

	// ModeEnvelope: keyRef IS the ciphertext, held by the CALLER (typically
	// pki_signing_keys.key_ref or an equivalent column), not by this
	// Signer and not by Vault -- Vault's Transit encrypt/decrypt endpoints
	// are stateless with respect to any one ciphertext, so there is
	// nothing named by keyRef for Vault to delete. Validating that keyRef
	// still decrypts (reporting pki.ErrKeyNotFound if it does not, the
	// same failure every other keyRef-taking method on this type reports)
	// and otherwise no-op-ing is the honest behaviour: the caller dropping
	// its own row is what actually destroys this key.
	if _, err := s.decryptPrivateKey(ctx, keyRef); err != nil {
		return err
	}
	return nil
}

// readPublicKey reads Transit key name's current public key.
func (s *signer) readPublicKey(ctx context.Context, name string) (ed25519.PublicKey, error) {
	secret, err := s.logical.ReadWithContext(ctx, s.mountPath+"/keys/"+name)
	if err != nil {
		return nil, fmt.Errorf("pki/signer/vault: read transit key %q: %w", name, err)
	}
	if secret == nil {
		return nil, pki.ErrKeyNotFound
	}
	pub, err := parseTransitPublicKey(secret.Data)
	if err != nil {
		return nil, fmt.Errorf("pki/signer/vault: parse public key for %q: %w", name, err)
	}
	return pub, nil
}

// encrypt wraps plaintext with WrappingKeyName, returning Vault's own
// "vault:vN:<base64>" ciphertext string verbatim as the opaque handle.
func (s *signer) encrypt(ctx context.Context, plaintext []byte) (string, error) {
	secret, err := s.logical.WriteWithContext(ctx, s.mountPath+"/encrypt/"+s.wrappingKeyName, map[string]interface{}{
		"plaintext": base64.StdEncoding.EncodeToString(plaintext),
	})
	if err != nil {
		return "", fmt.Errorf("pki/signer/vault: encrypt: %w", err)
	}
	if secret == nil {
		return "", fmt.Errorf("pki/signer/vault: encrypt returned no secret")
	}
	ciphertext, ok := secret.Data["ciphertext"].(string)
	if !ok {
		return "", fmt.Errorf("pki/signer/vault: encrypt response has no \"ciphertext\" field")
	}
	return ciphertext, nil
}

// decryptPrivateKey unwraps keyRef (a Vault ciphertext string) and parses
// the result as a PKCS8-encoded ed25519 private key.
func (s *signer) decryptPrivateKey(ctx context.Context, keyRef string) (ed25519.PrivateKey, error) {
	secret, err := s.logical.WriteWithContext(ctx, s.mountPath+"/decrypt/"+s.wrappingKeyName, map[string]interface{}{
		"ciphertext": keyRef,
	})
	if err != nil {
		return nil, fmt.Errorf("pki/signer/vault: decrypt: %w", err)
	}
	if secret == nil {
		return nil, pki.ErrKeyNotFound
	}
	encoded, ok := secret.Data["plaintext"].(string)
	if !ok {
		return nil, fmt.Errorf("pki/signer/vault: decrypt response has no \"plaintext\" field")
	}
	pkcs8, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("pki/signer/vault: decode decrypted plaintext: %w", err)
	}
	priv, err := x509.ParsePKCS8PrivateKey(pkcs8)
	if err != nil {
		return nil, fmt.Errorf("pki/signer/vault: parse decrypted private key: %w", err)
	}
	edPriv, ok := priv.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("pki/signer/vault: decrypted key is %T, want ed25519.PrivateKey", priv)
	}
	return edPriv, nil
}

// decodeVaultSignature strips Vault Transit's "vault:v<version>:" envelope
// off a sign response's signature field and base64-decodes the remainder
// into the raw signature bytes crypto/ed25519.Verify expects.
func decodeVaultSignature(encoded string) ([]byte, error) {
	parts := strings.SplitN(encoded, ":", 3)
	if len(parts) != 3 || parts[0] != "vault" {
		return nil, fmt.Errorf("pki/signer/vault: unexpected signature format %q", encoded)
	}
	sig, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("pki/signer/vault: decode signature: %w", err)
	}
	return sig, nil
}

// parseTransitPublicKey extracts the latest version's public key from a
// `GET transit/keys/<name>` response's Data. Vault's own JSON shape is
// {"latest_version": <number>, "keys": {"<version>": {"public_key": "<base64>", ...}}}.
func parseTransitPublicKey(data map[string]interface{}) (ed25519.PublicKey, error) {
	keys, ok := data["keys"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("response has no \"keys\" field")
	}

	latest, err := latestVersionString(data["latest_version"])
	if err != nil {
		return nil, err
	}

	versionRaw, ok := keys[latest]
	if !ok {
		return nil, fmt.Errorf("no key data for version %q", latest)
	}
	versionData, ok := versionRaw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected key version shape for %q", latest)
	}
	encoded, ok := versionData["public_key"].(string)
	if !ok {
		return nil, fmt.Errorf("no \"public_key\" field for version %q", latest)
	}

	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode public_key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// latestVersionString normalizes Vault's "latest_version" field -- decoded
// by encoding/json as a float64 in the ordinary case, but handled for
// json.Number and string too so this does not silently misbehave if a
// future client version or a test fixture decodes it differently.
func latestVersionString(v interface{}) (string, error) {
	switch value := v.(type) {
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64), nil
	case json.Number:
		return value.String(), nil
	case string:
		return value, nil
	default:
		return "", fmt.Errorf("unexpected type for \"latest_version\": %T", v)
	}
}

// compile-time check that *signer satisfies pki.Signer.
var _ pki.Signer = (*signer)(nil)
