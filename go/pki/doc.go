// Package pki owns key material that needs a lifecycle: signing keys and
// X.509 certificates, from generation through rotation to revocation. It
// sits above dbkit and tenancy and below authn in the module dependency
// graph (docs/internal/01-architecture.md), and it implements
// pkgcore.Module like every other business module.
//
// # Two layers
//
// The module splits into two layers that consumers reach independently
// (docs/internal/22-pki.md, the "two-layer structure" section):
//
//   - The key-lifecycle layer (Service, the Signer seam, LocalSigner, the
//     pki_signing_keys and pki_local_keys tables) is what authn will consume
//     through its own KeySource interface once its round-2 switch lands.
//     This round ships the interface shape and a synchronous, simplified
//     implementation -- see Service's doc comment for exactly what is and
//     is not implemented yet.
//   - The X.509 layer (CAService, pki_authorities, pki_certificates) issues
//     an internal CA chain and end-entity certificates on top of the
//     lifecycle layer. It has no real consumer in this repository yet; see
//     CAService's doc comment and this module's AGENTS.md for the
//     compensating obligations that come with shipping it anyway.
//
// # Signer seam, not key extraction
//
// The Signer interface is this module's most important design decision:
// it exposes a signing OPERATION (GenerateKey / Sign / Public / Destroy),
// never a way to read a private key back out. That is what makes a
// key-management-service-backed implementation (Vault Transit, AWS KMS --
// both round 4, in their own subpackages) able to sign without the key
// ever entering this process's memory. LocalSigner, the only implementation
// this round ships, stores an encrypted private key in pki_local_keys and
// decrypts it in memory for each Sign call -- it does NOT have that
// property, which is expected and documented, not a shortcut.
//
// # Round 1 of 4
//
// This module is delivered across four rounds (docs/internal/22-pki.md,
// the "delivery rounds" section); this package is round 1's output: the four tables and their
// dual-dialect migrations, the Signer seam and LocalSigner, internal CA and
// end-entity certificate issuance, tenant isolation proofs, and the
// key-lifecycle layer's PUBLIC API SHAPE with a simplified synchronous
// implementation. The pending/active/retiring/retired/revoked state machine,
// the jobs-driven expiry scan, the propagation window and the overlap
// period are round 2. Revocation and CRL generation are round 3. The
// vault and kmsaws Signer implementations are round 4. See AGENTS.md's
// "Known limitations" for the precise boundary.
package pki
