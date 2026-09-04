package kmsaws

// Mode selects which of the two protection strategies
// docs/internal/22-pki.md's Signer section describes a KMS-backed Signer
// runs under. Both are correct implementations of pki.Signer; they differ
// only in where the private key material actually lives. See
// go/pki/signer/vault's identical Mode type for the same distinction
// against Vault Transit -- the two packages deliberately mirror each
// other's shape.
type Mode int

const (
	// ModeEnvelope: GenerateKey creates the private key material inside
	// THIS process (crypto/ed25519.GenerateKey), then immediately seals it
	// by calling KMS's Encrypt operation against WrappingKeyID (an
	// existing symmetric CMK). The resulting ciphertext blob, base64
	// encoded, IS the opaque keyRef pki.Signer.GenerateKey returns -- this
	// Signer holds no storage of its own. Sign decrypts it back into
	// memory for the duration of one signing call. Does NOT earn
	// pkgcore.KeyNeverLeavesBoundary.
	ModeEnvelope Mode = iota

	// ModeDirectSign: GenerateKey asks KMS to create an asymmetric CMK
	// with KeySpec ECC_NIST_EDWARDS25519 and KeyUsage SIGN_VERIFY; keyRef
	// is that CMK's KeyId. Sign calls KMS's Sign operation with
	// SigningAlgorithm ED25519_SHA_512 and MessageType RAW every time --
	// the private key never exists inside this process, and never has.
	// Earns pkgcore.KeyNeverLeavesBoundary (registered as
	// "signer.aws-kms-direct"; see register.go). See doc.go's own section
	// on why RAW/ED25519_SHA_512, never DIGEST/ED25519_PH_SHA_512, is
	// mandatory here.
	ModeDirectSign
)

// Config configures a Signer over one AWS account/region's KMS.
type Config struct {
	// Region is the AWS region KMS keys live in, e.g. "us-east-1". KMS
	// keys are region-scoped, so this is required.
	Region string

	// AccessKeyID, SecretAccessKey and (optional) SessionToken are static
	// credentials this Signer authenticates every request with.
	// Deliberately NOT resolved through the AWS SDK's default credential
	// chain (environment, shared config file, EC2/ECS instance metadata,
	// SSO) -- see doc.go's own note on the dependency cost that chain adds
	// (github.com/aws/aws-sdk-go-v2/config pulls in STS, SSO and SSO-OIDC,
	// none of which this package needs to make a signature). A host that
	// wants IAM-role-based credentials (the ordinary production shape on
	// EC2/ECS/Lambda) resolves them itself -- with
	// github.com/aws/aws-sdk-go-v2/config, which it already depends on for
	// its own AWS wiring -- and passes the resolved static values through,
	// or bypasses this package's Config entirely and builds a *kms.Client
	// its own way.
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string

	// Mode selects ModeEnvelope or ModeDirectSign. See the Mode constants'
	// own doc comments.
	Mode Mode

	// WrappingKeyID names the symmetric KMS key (key ID, key ARN, alias
	// name, or alias ARN -- any form KMS's own KeyId parameter accepts)
	// this Signer's ModeEnvelope calls Encrypt/Decrypt against. Required
	// when Mode is ModeEnvelope; unused (and need not be set) under
	// ModeDirectSign. The key must already exist -- provisioning it is
	// deployment/operations work this package does not perform on the
	// caller's behalf, the same "no implicit fallback path" discipline
	// docs/internal/22-pki.md's "no second path" section requires.
	WrappingKeyID string
}
