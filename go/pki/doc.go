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
//     pki_signing_keys and pki_local_keys tables) is what authn consumes
//     through its own KeySource interface: the reference app wires
//     pki.NewModule(db).Service() as authn's KeySource, and the layer
//     holds the full pending/active/retiring/retired/revoked state
//     machine, the jobs-driven expiry scan, the propagation window and
//     the overlap period -- see Service's doc comment.
//   - The X.509 layer (CAService, pki_authorities, pki_certificates) issues
//     an internal CA chain and end-entity certificates on top of the
//     lifecycle layer. Its real consumer is the reference app's AI-output
//     attestation (internal/attestation: CA bootstrapping, per-tenant
//     certificate issuance, output signing via SignCertificate, and
//     chain-verified gating of public shares on VerifyCertificate) -- see
//     CAService's doc comment and AGENTS.md's consumer round entry for
//     what that consumer exercises and what it deliberately still does
//     not (the JWKS exports, the CRLDP extension, CRL-regeneration
//     scheduling).
//
// # Signer seam, not key extraction
//
// The Signer interface is this module's most important design decision:
// it exposes a signing OPERATION (GenerateKey / Sign / Public / Destroy),
// never a way to read a private key back out. That is what makes a
// key-management-service-backed implementation (Vault Transit, AWS KMS --
// both landed in round 4, in their own subpackages) able to sign without
// the key ever entering this process's memory. LocalSigner, the
// zero-dependency implementation, stores an encrypted private key in
// pki_local_keys and decrypts it in memory for each Sign call -- it does
// NOT have that property, which is expected and documented, not a
// shortcut.
//
// # Four delivered rounds
//
// This module was delivered across four rounds (docs/internal/22-pki.md,
// the "delivery rounds" section), all of which have landed: round 1 built
// the four tables and their dual-dialect migrations, the Signer seam and
// LocalSigner, internal CA and end-entity certificate issuance, tenant
// isolation proofs, and the key-lifecycle layer's public API shape; round
// 2 added the pending/active/retiring/retired/revoked state machine, the
// jobs-driven expiry scan, the propagation window and the overlap period;
// round 3 added revocation for both layers, CRL generation, chain
// verification, JWKS export and the module's first HTTP surface; round 4
// added SignerRegistry and the vault and kmsaws Signer implementations.
// Audit rounds since have hardened specific mechanisms -- the
// ledger-arbitrated certificate revocation, the guarded certificate-row
// transition, and the revoked-authority issuance refusal and CRL-number
// arbitration -- without changing the two-layer shape. See AGENTS.md's
// "Known limitations" for the precise boundary of what remains unbuilt.
package pki
