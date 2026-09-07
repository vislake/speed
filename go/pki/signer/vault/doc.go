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
// WARNING, standing until the fix below lands: Vault-side rotation of a
// Transit key is NOT governed by the pki module's key-lifecycle state
// machine (pending -> active -> retiring -> retired). That state machine
// exists precisely so rotation is coordinated -- a purpose's successor key
// is staged as pending, promoted only after the propagation window, and
// the retiring key stays verifiable during the overlap -- but it rotates
// by creating NEW Transit key names (each generateKeyDirect call makes a
// fresh "pki-<uuid>" key); it never rotates a Transit key in place. If an
// operator rotates the Transit key behind a name this package manages
// through Vault's own rotate endpoint instead (the one way the same name
// can acquire a new version), that rotation enters and leaves the pki
// module's protocol with nothing noticing: this package's signDirect pins
// no key_version, so Vault signs with the key's NEW latest version the
// moment the flip lands, while the public-key read this package performs
// and the module's JWKS export serve the LATEST version too -- the two
// agree at one moment and diverge across time. A verifier holding a JWKS
// exported before the flip can no longer verify signatures made after it,
// and a JWKS exported after it cannot verify signatures made before it;
// there is no pending/retiring overlap window, no JWKS rotation
// coordination, and no error anywhere. The envelope-mode names are not
// exposed to this hazard: an envelope keyRef is a ciphertext the caller
// holds, and rotating WrappingKeyName merely makes OLD ciphertexts
// undecryptable (an encrypt/decrypt failure, loud and visible), never a
// silent signature/verification divergence.
//
// The closure is planned in two tiers. Tier 1, the real fix: pin the
// version in the signing request -- Vault Transit's sign endpoint accepts
// key_version -- to the version the module issued and exported with, and
// reconcile it with the lifecycle state; that is how an external in-place
// rotation would enter this module's protocol instead of bypassing it.
// Tier 2, what this round ships until tier 1 lands: the standing warning
// on the signer.vault-direct registration (register.go), the
// needed-but-not-connected accounting on decodeVaultSignature
// (signer.go) -- which now parses and validates the version Vault's
// "vault:v<N>:" envelope carries even though nothing can act on it yet,
// so the field never reads like a decided irrelevance -- and this section.
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
