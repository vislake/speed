// Package vault is a pki.Signer implementation backed by HashiCorp Vault's
// Transit secrets engine, per docs/internal/22-pki.md's "three
// implementations, each its own subpackage" table. It is split out of
// go/pki's own root package -- rather than
// living beside the Signer interface the way go/pkgcore's Redis and S3
// implementations once did before issue #1's split -- so that a consumer
// which never wires a Vault-backed signer does not inherit
// github.com/hashicorp/vault/api and its transitive dependencies (18
// indirect modules, measured the same way every other seam split in this
// codebase is measured: a throwaway module, GOWORK=off go mod tidy, count
// "// indirect" entries -- see go/pki/AGENTS.md's Known limitations for the
// recorded number).
//
// Importing this package registers two names on pki.SignerRegistry as a
// side effect (see register.go): "signer.vault" (envelope mode) and
// "signer.vault-direct" (direct-sign mode). A host that wants either either
// blank-imports this package (`import _ ".../pki/signer/vault"`) so
// pki.SignerRegistry.Build resolves the name, or calls NewSigner directly
// and wires the result with pki.WithSigner -- the same two paths
// go/pkgcore/objectstore/s3's own doc comment describes for its own split.
//
// # Two names, not one, for one provider
//
// docs/internal/22-pki.md's Signer section requires this package to support
// BOTH envelope mode (the real private key, encrypted by Vault, held
// locally -- see Config's Mode field) and direct-sign mode (the private key
// generated inside, and never exported by, Vault -- earning
// pkgcore.KeyNeverLeavesBoundary). pkgcore.SeamRegistry[T]'s own contract
// pairs one FIXED Capability value with one registered name -- Build
// returns the Capability recorded on the Registration at init() time, never
// something read back off the constructed value -- so a single
// "signer.vault" name cannot correctly answer "does this have
// KeyNeverLeavesBoundary" when that answer depends on which Mode a
// particular Config selects. Rather than have Build lie about the
// capability of whichever mode a given cfg happened to choose, this package
// registers the two modes as two separate, honestly-labelled names, exactly
// mirroring how pkgcore's own MailerRegistry carries "mailer.console"
// (Stateless) and "mailer.smtp" (MultiReplicaSafe|SurvivesRestart) as two
// names rather than one name with a capability that depends on
// configuration.
//
// # In-place Transit key rotation is not governed by this module's
// # lifecycle state machine -- read this before choosing a direct-sign name
//
// The pki module's key-lifecycle state machine (pending -> active ->
// retiring -> retired) exists precisely so rotation is coordinated -- a
// purpose's successor key is staged as pending, promoted only after the
// propagation window, and the retiring key stays verifiable during the
// overlap -- but it rotates by creating NEW Transit key names (each
// generateKeyDirect call makes a fresh "pki-<uuid>" key); it never rotates
// a Transit key in place. Vault's own rotate endpoint is the one way a
// managed name can acquire a version other than the one this package
// created it at, and an operator rotating through it bypasses the module's
// state machine entirely. What this package does about that is the
// key_version pin (landed with the sign-request pin; see
// directSignPinnedKeyVersion in signer.go):
//
//   - Every direct-sign request pins the key to
//     directSignPinnedKeyVersion -- the version a freshly created Transit
//     key starts at, and therefore the version every keyRef this
//     package's ModeDirectSign GenerateKey issues was created at: the
//     version "the module issued and exported with". Vault's sign
//     endpoint accepts the key_version parameter and defaults to the
//     latest version without it, so an unpinned request would sign with
//     whatever version an in-place rotation had made latest.
//   - Public-key reads (readPublicKey, and with them every answer this
//     package gives about a keyRef's public key) serve the same pinned
//     version, never the key's latest_version.
//   - A sign answer whose "vault:v<N>:" envelope names any version other
//     than the pin is refused outright (signDirect's comparison after
//     parseVaultSignatureEnvelope), so a rotation that actually changed
//     which version signed -- or a Vault build that ignored the request's
//     key_version parameter -- surfaces as a loud error, never as a
//     signature no exported key verifies.
//
// The pin makes an in-place rotation unable to silently move either half
// of the sign/verify pair: signatures stay on the created version, and so
// does the advertised key, so they can never diverge across the flip. What
// the pin deliberately does NOT do is adopt the rotated version: the new
// version is inert until the host rotates through the module's own
// lifecycle, which creates a new name per stage (and a new keyRef) the
// ordinary way. The pin's behaviour is proven against stubbed clients only
// -- this package has no real-Vault integration leg (go/pki/AGENTS.md's
// Known limitations records why) -- so the interaction details of a real
// Vault's rotation policy (min_decryption_version handling above all, the
// one Vault-side setting that can make a pinned sign request itself be
// refused) remain to be verified against a real server by a future
// integration tier. The envelope-mode names are not exposed to this hazard
// at all: an envelope keyRef is a ciphertext the caller holds, and
// rotating WrappingKeyName merely makes OLD ciphertexts undecryptable (an
// encrypt/decrypt failure, loud and visible), never a silent
// signature/verification divergence.
//
// # No offline-runnable Example against a real Transit engine
//
// docs/internal/22-pki.md's testing-strategy section names a Vault
// integration leg that starts a real Vault server (dev mode) via
// testcontainers -- Docker-backed, and this codebase's plain unit-test tier
// (the one godoc Examples run under, per the repository's documentation
// rule) never has Docker available; Docker-backed tiers are a separate,
// explicitly-tagged integration_test/ package this round does not add (see
// go/pki/AGENTS.md's Known limitations). Consequently this package's
// Example functions (example_test.go) demonstrate construction and
// self-registration only -- building a Config, calling NewSigner (which
// dials nothing: the underlying Vault client, like every other built-in
// seam's client in this codebase, connects lazily on first use), and
// resolving "signer.vault"/"signer.vault-direct" through
// pki.SignerRegistry.Build -- never an actual GenerateKey/Sign round trip
// against a live Transit engine. The Sign/GenerateKey/Public/Destroy logic
// itself is proven instead by signer_test.go against a stubbed
// transitClient (docs/internal/22-pki.md's own testing-strategy note that
// AWS KMS gets its SDK interface stubbed for unit tests, with real
// verification left to manual testing, applies equally well here, for
// exactly the parts that do not need a real Transit engine's network
// behaviour to prove).
package vault
