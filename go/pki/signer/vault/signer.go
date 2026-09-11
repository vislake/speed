package vault

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
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

// directSignPinnedKeyVersion is the Transit key version this package pins
// its direct-sign requests and public-key reads to: the version every keyRef
// ModeDirectSign's GenerateKey issues was created at. Vault numbers a newly
// created Transit key's first version 1 (its create response's
// "latest_version"), and this package is the only creator of the keyRefs its
// direct-sign mode ever receives -- generateKeyDirect always creates a fresh
// "pki-<uuid>" name, and the pki module's rotation protocol never reuses a
// name, creating a new one per lifecycle stage instead (doc.go's
// "in-place Transit key rotation" section). Version 1 is therefore "the
// version the module issued and exported with" for every key this package
// manages, and pinning sign requests (Vault Transit's sign endpoint accepts
// a key_version parameter and defaults to the latest version without one)
// and public-key reads to it is what makes an in-place rotation of the
// Transit key through Vault's own rotate endpoint -- the one way a managed
// name can acquire a version other than 1 -- unable to change which version
// signs or which public key this package answers, silently or otherwise.
const directSignPinnedKeyVersion = 1

// transitClient is the subset of *vaultapi.Logical this package calls,
// declared as its own interface so unit tests can inject a scripted fake
// without a real Vault server -- the stubbed-SDK-interface-for-unit-tests
// approach applies here too, for the request/response shaping logic that
// does not need a real Transit engine's network behaviour to prove. *vaultapi.Logical
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
// this codebase (go-redis, and the SMTP/S3 clients pkgcore's own
// mailer_registry.go and objectstore/s3 build). An unusable
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
// message (PureEdDSA, RFC 8037's JWT EdDSA), matching pki.Signer.Sign's
// own documented, algorithm-dependent input contract exactly.
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
//
// The request pins the signing key to directSignPinnedKeyVersion -- the
// version this package created the key at, which is the version the pki
// module's lifecycle state machine issued and exported -- rather than
// letting Vault default to the key's latest version. An operator who
// rotates the Transit key in place through Vault's own rotate endpoint
// therefore cannot silently move the signatures this keyRef produces onto
// a version nobody exported: the pinned request keeps signing with the
// issued version, the pinned public-key read (readPublicKey) keeps serving
// that same version's key, and an envelope version that disagrees with the
// pin (a Vault that ignored the parameter, or signed with another version
// for any other reason) is refused below rather than returned as a
// signature no exported key verifies. See directSignPinnedKeyVersion's own
// doc comment and doc.go's "in-place Transit key rotation" section for the
// full argument.
func (s *signer) signDirect(ctx context.Context, keyRef string, input []byte) ([]byte, error) {
	secret, err := s.logical.WriteWithContext(ctx, s.mountPath+"/sign/"+keyRef, map[string]interface{}{
		"input":       base64.StdEncoding.EncodeToString(input),
		"key_version": strconv.Itoa(directSignPinnedKeyVersion),
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
	version, sig, err := parseVaultSignatureEnvelope(encoded)
	if err != nil {
		return nil, err
	}
	if version != directSignPinnedKeyVersion {
		return nil, fmt.Errorf(
			"pki/signer/vault: sign with %q answered with key version %d; this signer pins version %d (an in-place Transit key rotation changed which version signs; rotate through the pki module's key lifecycle instead)",
			keyRef, version, directSignPinnedKeyVersion)
	}
	return sig, nil
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

// readPublicKey reads Transit key name's public key at
// directSignPinnedKeyVersion -- the version this package created the key
// at, never the key's latest version. Serving the pinned version is the
// public-key half of the pin signDirect applies to its signing requests:
// after an in-place rotation of the Transit key through Vault's own rotate
// endpoint, this read keeps answering the same public key the module
// exported at issuance and the pinned signatures verify against, so the
// exported key and the signatures can never diverge across the rotation
// (see directSignPinnedKeyVersion's own doc comment and doc.go's
// "in-place Transit key rotation" section). Vault keeps every version's
// public-key entry in the keys map of a key's read response -- versions are
// never dropped from it while the key exists -- so the pinned version's
// entry is present for as long as this keyRef is valid at all.
func (s *signer) readPublicKey(ctx context.Context, name string) (ed25519.PublicKey, error) {
	secret, err := s.logical.ReadWithContext(ctx, s.mountPath+"/keys/"+name)
	if err != nil {
		return nil, fmt.Errorf("pki/signer/vault: read transit key %q: %w", name, err)
	}
	if secret == nil {
		return nil, pki.ErrKeyNotFound
	}
	pub, err := parseTransitPublicKey(secret.Data, directSignPinnedKeyVersion)
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

// parseVaultSignatureEnvelope strips Vault Transit's
// "vault:v<version>:" envelope off a sign response's signature field,
// returning the version it names and the base64-decoded raw signature
// bytes crypto/ed25519.Verify expects. The version is parsed and validated
// (Vault's own "v<N>" spelling, N a positive integer), and the decoded
// signature must be non-empty: an empty signature is never success, the
// failure-semantics half of the Signer seam contract go/pki/signer.go's
// Sign doc comment states, a contract this implementation and the kmsaws
// twin (signer.go's signDirect there) share.
//
// The returned version is the pin's verification half: signDirect pins its
// request to directSignPinnedKeyVersion, and this function's caller
// compares the version Vault actually signed with against that pin,
// refusing a signature produced by any other version. The version is
// therefore not merely parsed-and-validated; it is the signal that
// tells the caller an in-place Transit key rotation -- or a Vault that
// ignored the request's key_version parameter -- changed which version
// signs, so the divergence doc.go's "in-place Transit key rotation"
// section warns about fails loudly instead of shipping a signature no
// exported key verifies.
func parseVaultSignatureEnvelope(encoded string) (version int, sig []byte, err error) {
	parts := strings.SplitN(encoded, ":", 3)
	if len(parts) != 3 || parts[0] != "vault" {
		return 0, nil, fmt.Errorf("pki/signer/vault: unexpected signature format %q", encoded)
	}
	// Vault's own envelope spells the version "v1", "v2", ... -- the "v"
	// prefix is part of the field, not part of the value.
	versionText := parts[1]
	if len(versionText) < 2 || versionText[0] != 'v' {
		return 0, nil, fmt.Errorf("pki/signer/vault: unexpected signature version %q in %q", parts[1], encoded)
	}
	version, err = strconv.Atoi(versionText[1:])
	if err != nil || version < 1 {
		return 0, nil, fmt.Errorf("pki/signer/vault: unexpected signature version %q in %q", parts[1], encoded)
	}
	sig, err = base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return 0, nil, fmt.Errorf("pki/signer/vault: decode signature: %w", err)
	}
	if len(sig) == 0 {
		return 0, nil, fmt.Errorf("pki/signer/vault: signature in %q is empty", encoded)
	}
	return version, sig, nil
}

// parseTransitPublicKey extracts version's public key from a
// `GET transit/keys/<name>` response's Data. Vault's own JSON shape is
// {"latest_version": <number>, "keys": {"<version>": {"public_key": "<base64>", ...}}},
// with every version's entry kept in the keys map for as long as the key
// exists -- the pinned read below relies on that, and the lookup keys on
// the pinned version rather than the response's own "latest_version" for
// exactly the reason readPublicKey's doc comment gives: the version this
// package created the key at is the only one it ever exports or signs
// with, whatever the key's latest version has since become.
func parseTransitPublicKey(data map[string]interface{}, version int) (ed25519.PublicKey, error) {
	keys, ok := data["keys"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("response has no \"keys\" field")
	}

	versionKey := strconv.Itoa(version)
	versionRaw, ok := keys[versionKey]
	if !ok {
		return nil, fmt.Errorf("no key data for version %q", versionKey)
	}
	versionData, ok := versionRaw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected key version shape for %q", versionKey)
	}
	encoded, ok := versionData["public_key"].(string)
	if !ok {
		return nil, fmt.Errorf("no \"public_key\" field for version %q", versionKey)
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

// compile-time check that *signer satisfies pki.Signer.
var _ pki.Signer = (*signer)(nil)
