package vault

// Mode selects which of the two protection strategies
// docs/internal/22-pki.md's Signer section describes a Vault-backed Signer
// runs under. Both are correct implementations of pki.Signer; they differ
// only in where the private key material actually lives.
type Mode int

const (
	// ModeEnvelope: GenerateKey creates the private key material inside
	// THIS process (crypto/ed25519.GenerateKey, exactly like
	// pki.LocalSigner), then immediately seals it by calling Vault
	// Transit's encrypt operation against WrappingKeyName. The resulting
	// ciphertext IS the opaque keyRef pki.Signer.GenerateKey returns --
	// this Signer holds no storage of its own, and the caller's own table
	// (pki_signing_keys.key_ref and friends) is where that ciphertext
	// actually lives. Sign decrypts it back into memory for the duration
	// of one signing call, the same cost pki.LocalSigner's Sign pays for
	// its own dbkit-encrypted column. Does NOT earn
	// pkgcore.KeyNeverLeavesBoundary.
	ModeEnvelope Mode = iota

	// ModeDirectSign: GenerateKey asks Vault Transit to generate the key
	// AND hold it (created non-exportable); keyRef is the Transit key's
	// name. Sign is a Vault API call every time -- the private key never
	// exists inside this process, and never has. Earns
	// pkgcore.KeyNeverLeavesBoundary (registered as "signer.vault-direct";
	// see register.go).
	ModeDirectSign
)

// Config configures a Signer over one Vault Transit secrets engine mount.
type Config struct {
	// Address is the Vault server's base URL, e.g.
	// "https://vault.example.com:8200".
	Address string

	// Token authenticates every request this Signer makes. Acquiring one
	// (AppRole, Kubernetes auth, a human operator's `vault login`, ...) is
	// the host's concern -- this package accepts only an already-issued
	// token, the same boundary go/authn draws around session tokens it
	// verifies but does not mint through a third-party IdP itself.
	Token string

	// Namespace is the Vault Enterprise namespace this Signer operates in.
	// Empty for open-source Vault or the root namespace -- the overwhelming
	// majority of deployments.
	Namespace string

	// MountPath is the Transit secrets engine's mount point. Defaults to
	// "transit" -- Vault's own default mount name -- when empty.
	MountPath string

	// Mode selects ModeEnvelope or ModeDirectSign. See the Mode constants'
	// own doc comments.
	Mode Mode

	// WrappingKeyName names the Transit key this Signer's ModeEnvelope
	// calls encrypt/decrypt against. Required when Mode is ModeEnvelope;
	// unused (and need not be set) under ModeDirectSign. The key must
	// already exist in Vault -- provisioning it (`vault write
	// transit/keys/<name>`, Transit's own default AES256-GCM96 type is
	// exactly right for wrapping arbitrary bytes) is deployment/operations
	// work this package deliberately does not perform on the caller's
	// behalf, the same "no implicit fallback path" discipline
	// docs/internal/22-pki.md's "no second path" section requires of every
	// key-management seam in this module.
	WrappingKeyName string
}
