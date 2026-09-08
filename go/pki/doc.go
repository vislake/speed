// Package pki owns key material that needs a lifecycle: signing keys and
// X.509 certificates, from generation through rotation to revocation. It
// sits above dbkit and tenancy and below authn in the module dependency
// graph, and it implements pkgcore.Module like every other business module.
//
// # Two layers
//
// The module splits into two layers that consumers reach independently:
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
//     CAService's doc comment and AGENTS.md's "X.509 layer: real consumer,
//     precise residuals" section for what that consumer exercises and what
//     it deliberately still does not (the JWKS exports, the CRLDP
//     extension, CRL-regeneration scheduling).
//
// # Signer seam, not key extraction
//
// The Signer interface is this module's most important design decision:
// it exposes a signing OPERATION (GenerateKey / Sign / Public / Destroy),
// never a way to read a private key back out. That is what makes a
// key-management-service-backed implementation (Vault Transit, AWS KMS --
// both in their own subpackages) able to sign without the key ever
// entering this process's memory. LocalSigner, the zero-dependency
// implementation, stores an encrypted private key in pki_local_keys and
// decrypts it in memory for each Sign call -- it does NOT have that
// property, which is expected and documented, not a shortcut.
//
// # What ships on top of the two layers
//
// Revocation exists on both layers -- Service.RevokeSigningKey excludes a
// revoked key from every read path immediately, and CAService revocation
// is database-arbitrated through an append-only ledger that every chain
// walk consults -- alongside CRL generation, two JWKS exports, an HTTP
// surface at /api/v1/pki, and SignerRegistry (a pkgcore.SeamRegistry[Signer])
// carrying LocalSigner plus the vault and kmsaws provider implementations.
package pki
