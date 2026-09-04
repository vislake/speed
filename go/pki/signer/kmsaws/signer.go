package kmsaws

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"

	"github.com/vislake/speed/go/pki"
)

// scheduleDeletionPendingWindowDays is the pending window Destroy schedules
// a ModeDirectSign key's deletion under. AWS KMS never deletes a key
// immediately -- ScheduleKeyDeletion's PendingWindowInDays must be between
// 7 and 30 (KMS's own enforced range), and this package uses the minimum.
// Destroy's own doc comment records what this means for callers: the key
// remains usable (and, more importantly, remains billed and remains a live
// attack surface) for this many days after Destroy returns nil.
const scheduleDeletionPendingWindowDays = 7

// kmsClient is the subset of *kms.Client this package calls, declared as
// its own interface so unit tests can inject a scripted fake without a
// real AWS account -- exactly the stub-the-SDK-interface-for-unit-tests
// approach docs/internal/22-pki.md's testing-strategy section prescribes
// for this package specifically, since no integration leg exists (see
// doc.go). *kms.Client already has every one of these
// methods with this exact signature, so it satisfies kmsClient
// structurally -- no adapter type is needed anywhere in this package.
type kmsClient interface {
	CreateKey(ctx context.Context, params *kms.CreateKeyInput, optFns ...func(*kms.Options)) (*kms.CreateKeyOutput, error)
	GetPublicKey(ctx context.Context, params *kms.GetPublicKeyInput, optFns ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error)
	Sign(ctx context.Context, params *kms.SignInput, optFns ...func(*kms.Options)) (*kms.SignOutput, error)
	ScheduleKeyDeletion(ctx context.Context, params *kms.ScheduleKeyDeletionInput, optFns ...func(*kms.Options)) (*kms.ScheduleKeyDeletionOutput, error)
	Encrypt(ctx context.Context, params *kms.EncryptInput, optFns ...func(*kms.Options)) (*kms.EncryptOutput, error)
	Decrypt(ctx context.Context, params *kms.DecryptInput, optFns ...func(*kms.Options)) (*kms.DecryptOutput, error)
}

// signer is this package's pki.Signer implementation, covering both Mode
// values.
type signer struct {
	client        kmsClient
	mode          Mode
	wrappingKeyID string
}

// NewSigner returns a pki.Signer backed by cfg's AWS account and region.
// Nothing is dialed here: the underlying KMS client, like every other
// built-in seam's client in this codebase, issues no request until the
// first operation. An unusable configuration -- an empty Region or
// AccessKeyID, or ModeEnvelope without WrappingKeyID -- returns an error
// rather than panicking, the same registry-reachable-constructor
// convention go/pki/signer/vault's own NewSigner documents.
func NewSigner(cfg Config) (pki.Signer, error) {
	if cfg.Region == "" {
		return nil, fmt.Errorf("pki/signer/kmsaws: NewSigner requires a non-empty Config.Region")
	}
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, fmt.Errorf("pki/signer/kmsaws: NewSigner requires a non-empty Config.AccessKeyID and Config.SecretAccessKey")
	}
	if cfg.Mode == ModeEnvelope && cfg.WrappingKeyID == "" {
		return nil, fmt.Errorf("pki/signer/kmsaws: NewSigner requires a non-empty Config.WrappingKeyID in ModeEnvelope")
	}

	awsCfg := aws.Config{
		Region:      cfg.Region,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, cfg.SessionToken),
	}

	return &signer{
		client:        kms.NewFromConfig(awsCfg),
		mode:          cfg.Mode,
		wrappingKeyID: cfg.WrappingKeyID,
	}, nil
}

// GenerateKey implements pki.Signer. Only pki.AlgorithmEd25519 is
// supported.
func (s *signer) GenerateKey(ctx context.Context, algorithm string) (string, crypto.PublicKey, error) {
	if algorithm != pki.AlgorithmEd25519 {
		return "", nil, pki.ErrAlgorithmUnsupportedBySigner.WithParam("algorithm", algorithm)
	}
	if s.mode == ModeDirectSign {
		return s.generateKeyDirect(ctx)
	}
	return s.generateKeyEnvelope(ctx)
}

// generateKeyDirect creates a new asymmetric CMK with KeySpec
// ECC_NIST_EDWARDS25519 and returns its KeyId as keyRef.
func (s *signer) generateKeyDirect(ctx context.Context) (string, crypto.PublicKey, error) {
	created, err := s.client.CreateKey(ctx, &kms.CreateKeyInput{
		KeySpec:  types.KeySpecEccNistEdwards25519,
		KeyUsage: types.KeyUsageTypeSignVerify,
	})
	if err != nil {
		return "", nil, fmt.Errorf("pki/signer/kmsaws: create key: %w", err)
	}
	if created.KeyMetadata == nil || created.KeyMetadata.KeyId == nil {
		return "", nil, fmt.Errorf("pki/signer/kmsaws: create key: response has no KeyMetadata.KeyId")
	}
	keyID := *created.KeyMetadata.KeyId

	pub, err := s.readPublicKey(ctx, keyID)
	if err != nil {
		return "", nil, err
	}
	return keyID, pub, nil
}

// generateKeyEnvelope generates a real ed25519 key pair in this process,
// then immediately wraps the private half with KMS's Encrypt operation
// against WrappingKeyID -- see Mode's own doc comment for why the
// resulting ciphertext is the keyRef this returns.
func (s *signer) generateKeyEnvelope(ctx context.Context) (string, crypto.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, fmt.Errorf("pki/signer/kmsaws: generate ed25519 key: %w", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", nil, fmt.Errorf("pki/signer/kmsaws: marshal private key: %w", err)
	}

	out, err := s.client.Encrypt(ctx, &kms.EncryptInput{
		KeyId:     aws.String(s.wrappingKeyID),
		Plaintext: pkcs8,
	})
	if err != nil {
		return "", nil, fmt.Errorf("pki/signer/kmsaws: encrypt: %w", err)
	}
	keyRef := base64.StdEncoding.EncodeToString(out.CiphertextBlob)
	return keyRef, pub, nil
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

// signDirect asks KMS to sign input with the CMK named keyRef, using
// ED25519_SHA_512 (PureEdDSA) with MessageType RAW -- see doc.go's own
// section on why this exact combination is mandatory, never
// ED25519_PH_SHA_512/DIGEST.
func (s *signer) signDirect(ctx context.Context, keyRef string, input []byte) ([]byte, error) {
	out, err := s.client.Sign(ctx, &kms.SignInput{
		KeyId:            aws.String(keyRef),
		Message:          input,
		MessageType:      types.MessageTypeRaw,
		SigningAlgorithm: types.SigningAlgorithmSpecEd25519Sha512,
	})
	if err != nil {
		return nil, fmt.Errorf("pki/signer/kmsaws: sign with %q: %w", keyRef, err)
	}
	return out.Signature, nil
}

// signEnvelope decrypts keyRef back into a private key for the duration of
// this call only, signs locally, and lets the decrypted key go out of
// scope.
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

// readPublicKey fetches keyID's DER-encoded SubjectPublicKeyInfo through
// KMS's GetPublicKey and parses it into an ed25519.PublicKey.
func (s *signer) readPublicKey(ctx context.Context, keyID string) (ed25519.PublicKey, error) {
	out, err := s.client.GetPublicKey(ctx, &kms.GetPublicKeyInput{KeyId: aws.String(keyID)})
	if err != nil {
		return nil, fmt.Errorf("pki/signer/kmsaws: get public key for %q: %w", keyID, err)
	}
	pub, err := x509.ParsePKIXPublicKey(out.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("pki/signer/kmsaws: parse public key for %q: %w", keyID, err)
	}
	edPub, ok := pub.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("pki/signer/kmsaws: public key for %q is %T, want ed25519.PublicKey", keyID, pub)
	}
	return edPub, nil
}

// decryptPrivateKey unwraps keyRef (a base64-encoded KMS ciphertext blob)
// against WrappingKeyID and parses the result as a PKCS8-encoded ed25519
// private key.
func (s *signer) decryptPrivateKey(ctx context.Context, keyRef string) (ed25519.PrivateKey, error) {
	blob, err := base64.StdEncoding.DecodeString(keyRef)
	if err != nil {
		return nil, pki.ErrKeyNotFound.WithParam("reason", "keyRef is not valid base64")
	}

	out, err := s.client.Decrypt(ctx, &kms.DecryptInput{
		KeyId:          aws.String(s.wrappingKeyID),
		CiphertextBlob: blob,
	})
	if err != nil {
		return nil, fmt.Errorf("pki/signer/kmsaws: decrypt: %w", err)
	}
	priv, err := x509.ParsePKCS8PrivateKey(out.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("pki/signer/kmsaws: parse decrypted private key: %w", err)
	}
	edPriv, ok := priv.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("pki/signer/kmsaws: decrypted key is %T, want ed25519.PrivateKey", priv)
	}
	return edPriv, nil
}

// Destroy implements pki.Signer.
//
// # ModeDirectSign schedules, it does not immediately delete
//
// AWS KMS has no "delete now" operation for a key with deletion protection
// meaningfully enforced: ScheduleKeyDeletion's PendingWindowInDays must be
// at least 7 (KMS's own enforced minimum), so the CMK -- and its private
// key -- remains usable, billed, and a live attack surface for
// scheduleDeletionPendingWindowDays days after this method returns nil.
// This is an honest KMS limitation, not a bug in this package: a caller
// that needs "gone right now" semantics does not get them from KMS-backed
// direct-sign key destruction, only from the pki module's own revocation
// mechanism (docs/internal/22-pki.md's "revocation" section, round 3), which
// stops the key from being trusted immediately regardless of when KMS
// physically deletes it.
func (s *signer) Destroy(ctx context.Context, keyRef string) error {
	if s.mode == ModeDirectSign {
		if _, err := s.client.ScheduleKeyDeletion(ctx, &kms.ScheduleKeyDeletionInput{
			KeyId:               aws.String(keyRef),
			PendingWindowInDays: aws.Int32(scheduleDeletionPendingWindowDays),
		}); err != nil {
			return fmt.Errorf("pki/signer/kmsaws: schedule key deletion for %q: %w", keyRef, err)
		}
		return nil
	}

	// ModeEnvelope: keyRef IS the ciphertext, held by the CALLER, not by
	// this Signer and not by KMS -- see
	// go/pki/signer/vault's identical reasoning for its own envelope
	// Destroy. Validate keyRef still decrypts, then no-op.
	if _, err := s.decryptPrivateKey(ctx, keyRef); err != nil {
		return err
	}
	return nil
}

// compile-time check that *signer satisfies pki.Signer.
var _ pki.Signer = (*signer)(nil)
