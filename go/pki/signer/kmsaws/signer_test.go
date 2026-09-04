package kmsaws

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"

	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pki"
)

// fakeKMSClient is a scripted kmsClient double, letting this file pin the
// exact KMS request/response shapes this package's
// GenerateKey/Sign/Public/Destroy build and parse, without a real AWS
// account -- see doc.go's own "no integration leg" section.
type fakeKMSClient struct {
	createKey           func(context.Context, *kms.CreateKeyInput, ...func(*kms.Options)) (*kms.CreateKeyOutput, error)
	getPublicKey        func(context.Context, *kms.GetPublicKeyInput, ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error)
	sign                func(context.Context, *kms.SignInput, ...func(*kms.Options)) (*kms.SignOutput, error)
	scheduleKeyDeletion func(context.Context, *kms.ScheduleKeyDeletionInput, ...func(*kms.Options)) (*kms.ScheduleKeyDeletionOutput, error)
	encrypt             func(context.Context, *kms.EncryptInput, ...func(*kms.Options)) (*kms.EncryptOutput, error)
	decrypt             func(context.Context, *kms.DecryptInput, ...func(*kms.Options)) (*kms.DecryptOutput, error)
}

func (f *fakeKMSClient) CreateKey(ctx context.Context, in *kms.CreateKeyInput, opts ...func(*kms.Options)) (*kms.CreateKeyOutput, error) {
	if f.createKey == nil {
		return nil, fmt.Errorf("unexpected CreateKey call")
	}
	return f.createKey(ctx, in, opts...)
}

func (f *fakeKMSClient) GetPublicKey(ctx context.Context, in *kms.GetPublicKeyInput, opts ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error) {
	if f.getPublicKey == nil {
		return nil, fmt.Errorf("unexpected GetPublicKey call")
	}
	return f.getPublicKey(ctx, in, opts...)
}

func (f *fakeKMSClient) Sign(ctx context.Context, in *kms.SignInput, opts ...func(*kms.Options)) (*kms.SignOutput, error) {
	if f.sign == nil {
		return nil, fmt.Errorf("unexpected Sign call")
	}
	return f.sign(ctx, in, opts...)
}

func (f *fakeKMSClient) ScheduleKeyDeletion(ctx context.Context, in *kms.ScheduleKeyDeletionInput, opts ...func(*kms.Options)) (*kms.ScheduleKeyDeletionOutput, error) {
	if f.scheduleKeyDeletion == nil {
		return nil, fmt.Errorf("unexpected ScheduleKeyDeletion call")
	}
	return f.scheduleKeyDeletion(ctx, in, opts...)
}

func (f *fakeKMSClient) Encrypt(ctx context.Context, in *kms.EncryptInput, opts ...func(*kms.Options)) (*kms.EncryptOutput, error) {
	if f.encrypt == nil {
		return nil, fmt.Errorf("unexpected Encrypt call")
	}
	return f.encrypt(ctx, in, opts...)
}

func (f *fakeKMSClient) Decrypt(ctx context.Context, in *kms.DecryptInput, opts ...func(*kms.Options)) (*kms.DecryptOutput, error) {
	if f.decrypt == nil {
		return nil, fmt.Errorf("unexpected Decrypt call")
	}
	return f.decrypt(ctx, in, opts...)
}

// compile-time check that *fakeKMSClient satisfies kmsClient.
var _ kmsClient = (*fakeKMSClient)(nil)

func TestSigner_GenerateKey_RejectsUnsupportedAlgorithm(t *testing.T) {
	s := &signer{client: &fakeKMSClient{}, mode: ModeDirectSign}
	_, _, err := s.GenerateKey(context.Background(), "ecdsa-p256")
	found, ok := apperr.As(err)
	if !ok || found.Code != pki.ErrAlgorithmUnsupportedBySigner.Code {
		t.Fatalf("GenerateKey(unsupported) error = %v, want ErrAlgorithmUnsupportedBySigner", err)
	}
}

func TestSigner_DirectMode_GenerateKey_CreatesAsymmetricKeyAndReadsPublicKey(t *testing.T) {
	realPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(realPub)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}

	var gotKeySpec types.KeySpec
	var gotKeyUsage types.KeyUsageType
	var gotPublicKeyKeyID string
	fake := &fakeKMSClient{
		createKey: func(_ context.Context, in *kms.CreateKeyInput, _ ...func(*kms.Options)) (*kms.CreateKeyOutput, error) {
			gotKeySpec = in.KeySpec
			gotKeyUsage = in.KeyUsage
			return &kms.CreateKeyOutput{KeyMetadata: &types.KeyMetadata{KeyId: aws.String("key-1234")}}, nil
		},
		getPublicKey: func(_ context.Context, in *kms.GetPublicKeyInput, _ ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error) {
			gotPublicKeyKeyID = *in.KeyId
			return &kms.GetPublicKeyOutput{PublicKey: spki}, nil
		},
	}

	s := &signer{client: fake, mode: ModeDirectSign}
	keyRef, pub, err := s.GenerateKey(context.Background(), pki.AlgorithmEd25519)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if keyRef != "key-1234" {
		t.Errorf("keyRef = %q, want %q", keyRef, "key-1234")
	}
	if !pub.(ed25519.PublicKey).Equal(realPub) {
		t.Errorf("GenerateKey public key = %x, want %x", pub, realPub)
	}
	if gotKeySpec != types.KeySpecEccNistEdwards25519 {
		t.Errorf("CreateKey KeySpec = %v, want %v", gotKeySpec, types.KeySpecEccNistEdwards25519)
	}
	if gotKeyUsage != types.KeyUsageTypeSignVerify {
		t.Errorf("CreateKey KeyUsage = %v, want %v", gotKeyUsage, types.KeyUsageTypeSignVerify)
	}
	if gotPublicKeyKeyID != "key-1234" {
		t.Errorf("GetPublicKey KeyId = %q, want %q", gotPublicKeyKeyID, "key-1234")
	}
}

// TestSignDirect_ProducesAPureEdDSASignature is the pin doc.go's own
// section promises: it proves signDirect requests ED25519_SHA_512 with
// MessageType RAW -- the PureEdDSA combination a plain crypto/ed25519.Verify
// call accepts -- and that this package's own decode path (a direct pass-
// through of SignOutput.Signature, no re-encoding) hands back bytes that
// verify. A future edit that swaps in ED25519_PH_SHA_512 or MessageType
// DIGEST would still make this test's fake return SOMETHING, but the
// asserted request fields below catch the swap directly, and a real KMS
// server would additionally refuse to produce a PureEdDSA-verifiable
// signature under that combination -- see doc.go for why that failure mode
// is silent-until-verification and worth pinning explicitly.
func TestSignDirect_ProducesAPureEdDSASignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	message := []byte("pki kmsaws direct-sign round trip")
	realSig := ed25519.Sign(priv, message)

	var gotMessageType types.MessageType
	var gotAlgorithm types.SigningAlgorithmSpec
	var gotMessage []byte
	fake := &fakeKMSClient{
		sign: func(_ context.Context, in *kms.SignInput, _ ...func(*kms.Options)) (*kms.SignOutput, error) {
			gotMessageType = in.MessageType
			gotAlgorithm = in.SigningAlgorithm
			gotMessage = in.Message
			return &kms.SignOutput{Signature: realSig}, nil
		},
	}

	s := &signer{client: fake, mode: ModeDirectSign}
	sig, err := s.Sign(context.Background(), "key-1234", message)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !ed25519.Verify(pub, message, sig) {
		t.Error("ed25519.Verify failed for the signature this package produced")
	}
	if gotMessageType != types.MessageTypeRaw {
		t.Errorf("MessageType = %v, want %v (RAW, not DIGEST -- see doc.go)", gotMessageType, types.MessageTypeRaw)
	}
	if gotAlgorithm != types.SigningAlgorithmSpecEd25519Sha512 {
		t.Errorf("SigningAlgorithm = %v, want %v (not ED25519_PH_SHA_512 -- see doc.go)", gotAlgorithm, types.SigningAlgorithmSpecEd25519Sha512)
	}
	if string(gotMessage) != string(message) {
		t.Errorf("Message = %q, want the COMPLETE message %q (never a pre-hashed digest)", gotMessage, message)
	}
}

func TestSigner_DirectMode_Destroy_SchedulesDeletionWithTheMinimumWindow(t *testing.T) {
	var gotKeyID string
	var gotWindow *int32
	fake := &fakeKMSClient{
		scheduleKeyDeletion: func(_ context.Context, in *kms.ScheduleKeyDeletionInput, _ ...func(*kms.Options)) (*kms.ScheduleKeyDeletionOutput, error) {
			gotKeyID = *in.KeyId
			gotWindow = in.PendingWindowInDays
			return &kms.ScheduleKeyDeletionOutput{}, nil
		},
	}
	s := &signer{client: fake, mode: ModeDirectSign}
	if err := s.Destroy(context.Background(), "key-1234"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if gotKeyID != "key-1234" {
		t.Errorf("ScheduleKeyDeletion KeyId = %q, want %q", gotKeyID, "key-1234")
	}
	if gotWindow == nil || *gotWindow != scheduleDeletionPendingWindowDays {
		t.Errorf("ScheduleKeyDeletion PendingWindowInDays = %v, want %d", gotWindow, scheduleDeletionPendingWindowDays)
	}
}

func TestSigner_EnvelopeMode_GenerateKeyThenSignAndPublic_RoundTrip(t *testing.T) {
	wrap := newFakeWrap(t)
	fake := &fakeKMSClient{
		encrypt: func(_ context.Context, in *kms.EncryptInput, _ ...func(*kms.Options)) (*kms.EncryptOutput, error) {
			if *in.KeyId != "wrap-key" {
				t.Errorf("Encrypt KeyId = %q, want %q", *in.KeyId, "wrap-key")
			}
			return &kms.EncryptOutput{CiphertextBlob: wrap.encrypt(in.Plaintext)}, nil
		},
		decrypt: func(_ context.Context, in *kms.DecryptInput, _ ...func(*kms.Options)) (*kms.DecryptOutput, error) {
			if *in.KeyId != "wrap-key" {
				t.Errorf("Decrypt KeyId = %q, want %q", *in.KeyId, "wrap-key")
			}
			plaintext, err := wrap.decrypt(in.CiphertextBlob)
			if err != nil {
				return nil, err
			}
			return &kms.DecryptOutput{Plaintext: plaintext}, nil
		},
	}

	s := &signer{client: fake, mode: ModeEnvelope, wrappingKeyID: "wrap-key"}
	ctx := context.Background()

	keyRef, pub, err := s.GenerateKey(ctx, pki.AlgorithmEd25519)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if keyRef == "" {
		t.Fatal("GenerateKey returned an empty keyRef")
	}
	if _, decodeErr := base64.StdEncoding.DecodeString(keyRef); decodeErr != nil {
		t.Errorf("keyRef = %q is not valid base64: %v", keyRef, decodeErr)
	}

	gotPub, err := s.Public(ctx, keyRef)
	if err != nil {
		t.Fatalf("Public: %v", err)
	}
	if !gotPub.(ed25519.PublicKey).Equal(pub.(ed25519.PublicKey)) {
		t.Errorf("Public() = %x, want %x", gotPub, pub)
	}

	message := []byte("pki kmsaws envelope round trip")
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
	fake := &fakeKMSClient{
		encrypt: func(_ context.Context, in *kms.EncryptInput, _ ...func(*kms.Options)) (*kms.EncryptOutput, error) {
			return &kms.EncryptOutput{CiphertextBlob: wrap.encrypt(in.Plaintext)}, nil
		},
		decrypt: func(_ context.Context, in *kms.DecryptInput, _ ...func(*kms.Options)) (*kms.DecryptOutput, error) {
			decryptCalls++
			plaintext, err := wrap.decrypt(in.CiphertextBlob)
			if err != nil {
				return nil, err
			}
			return &kms.DecryptOutput{Plaintext: plaintext}, nil
		},
	}
	s := &signer{client: fake, mode: ModeEnvelope, wrappingKeyID: "wrap-key"}
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
	s := &signer{client: &fakeKMSClient{}, mode: ModeEnvelope, wrappingKeyID: "wrap-key"}
	_, err := s.Sign(context.Background(), "not-valid-base64!!", []byte("x"))
	found, ok := apperr.As(err)
	if !ok || found.Code != pki.ErrKeyNotFound.Code {
		t.Errorf("Sign(invalid keyRef) error = %v, want ErrKeyNotFound", err)
	}
}

func TestNewSigner_Validation(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "missing region", cfg: Config{AccessKeyID: "ak", SecretAccessKey: "sk"}},
		{name: "missing access key", cfg: Config{Region: "us-east-1", SecretAccessKey: "sk"}},
		{name: "missing secret key", cfg: Config{Region: "us-east-1", AccessKeyID: "ak"}},
		{
			name: "envelope mode missing wrapping key",
			cfg:  Config{Region: "us-east-1", AccessKeyID: "ak", SecretAccessKey: "sk", Mode: ModeEnvelope},
		},
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
	if _, err := NewSigner(Config{
		Region:          "us-east-1",
		AccessKeyID:     "ak",
		SecretAccessKey: "sk",
		Mode:            ModeDirectSign,
	}); err != nil {
		t.Fatalf("NewSigner() error = %v, want nil", err)
	}
}

// fakeWrap is a tiny, test-only stand-in for a KMS symmetric wrapping key:
// it "encrypts" by prefixing plaintext with a marker byte sequence and
// "decrypts" by stripping it, letting the envelope-mode tests exercise the
// real request/response shaping code without needing real AES-GCM.
type fakeWrap struct{ t *testing.T }

func newFakeWrap(t *testing.T) *fakeWrap { return &fakeWrap{t: t} }

var fakeWrapPrefix = []byte("kms-fake-wrap:")

func (w *fakeWrap) encrypt(plaintext []byte) []byte {
	return append(append([]byte{}, fakeWrapPrefix...), plaintext...)
}

func (w *fakeWrap) decrypt(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < len(fakeWrapPrefix) || string(ciphertext[:len(fakeWrapPrefix)]) != string(fakeWrapPrefix) {
		return nil, fmt.Errorf("fakeWrap: not a recognized ciphertext")
	}
	return ciphertext[len(fakeWrapPrefix):], nil
}
