---
title: pki
description: "Key material with a lifecycle: Ed25519 signing keys and an internal X.509 CA — generation, rotation, revocation and CRLs behind a signer seam that never exposes a private key."
weight: 3
---

# pki

pki is speed's key-material module: signing keys and X.509
certificates that need a lifecycle — generation, rotation, revocation
— owned by the platform behind a `Signer` seam that never exposes a
private key.

## What it is for

Two layers. The **key-lifecycle layer** manages signing keys per
*purpose* through a state machine the module drives:
`pending → active → retiring →
retired` (plus `revoked`), with at most one active key per purpose
enforced by the database, a propagation window and renewal lead time
honored between transitions, and an event-invalidated key-set cache so
a rotation or revoke on one replica converges the others through the
bus. `Service.EnsurePurpose` is the bootstrap path ("this purpose
needs a key, in validity, or make one"); `ActiveSigner` and
`VerificationKeys` serve the signing hot paths; the expiry scan
(`Service.EnqueueExpiryScan`, a jobs task) drives the transitions on a
cadence your host schedules — never its own. `Signer` exposes
operations (`GenerateKey`, `Sign`, `Public`, `Destroy`), never a way
to read a private key back out; `LocalSigner` is the zero-dependency
implementation, and `go/pki/signer/vault` and `go/pki/signer/kmsaws`
implement the same seam against Vault Transit and AWS KMS, each
registering two names — envelope mode (key encrypted externally,
decrypted locally to sign) and direct-sign mode (key never leaves the
boundary, declared through `pkgcore.KeyNeverLeavesBoundary`) — so a
host swaps providers by changing a registered name.

The **X.509 layer** is an internal CA: `CAService.CreateRootCA`,
`CreateIntermediateCA` and `IssueCertificate` (16-byte `crypto/rand`
serials), `VerifyCertificate` (real chain verification that refuses a
revoked certificate or a revoked authority anywhere in the chain),
certificate revocation arbitrated between the row and an append-only
revocation ledger, CRL generation with RFC 5280 numbering, and two
public-keys-only JWKS exports. A small HTTP surface under
`/api/v1/pki` mounts the five operations that belong on the wire:
`pki_revokeSigningKey`, `pki_revokeCertificate`, the two JWKS reads
and `pki_getAuthorityCrl`. The reference app's AI-output attestation
layer is the X.509 layer's real consumer — it mints a per-database
chain, issues per-tenant certificates, signs each simulation output,
and refuses to share an attested object whose certificate, signature
or digest fails verification.

What it is **not**: not TLS certificates for transport — no ACME, no
OCSP responder (CRL only), no Certificate Transparency, no
cross-deployment CA federation, no generic "export private key" API.

## When to choose it

You sign something with keys that must rotate and be revocable —
authn consumes the key-lifecycle layer through its structurally
satisfied `KeySource` seam, and any other signing need (content
attestation, document signing) fits the same shape. You need an
internal CA to issue certificates whose revocation is provable to
verifiers. Your threat model demands key material that never enters
your process (`vault`/`kmsaws` direct-sign modes). If you need
*transport* security or public-facing certificates, this is not the
module.

## Wiring it in

```go
pkiModule := pki.NewModule(db, pki.WithQueue(queue)) // queue enables the expiry-scan handler
keySource := pkiModule.Service()  // satisfies authn's KeySource structurally
authnModule := authn.NewModule(db, authn.WithKeySource(keySource), /* ... */)

// X.509 layer, when you issue certificates:
ca := pkiModule.CA()
err := ca.CreateRootCA(ctx, pki.RootCAParams{ /* subject, validity */ })
```

Lifecycle plumbing: after `Bootstrap`, call `EnsurePurpose` for each
purpose your product signs for, and schedule `EnqueueExpiryScan` from
your periodic-task loop; the scan stages and promotes successors. Host
options: `WithSigner(name, signer)`, `WithPropagationWindow`,
`WithRenewalLeadTime`, `WithCacheTTL`, `WithExpiryScanWindow`.
Resolve a signer by registered name with a capability requirement
through `SignerRegistry`'s `BuildSignerRequiring` (blank-import the
provider subpackage first).

## Core concepts and API surface

- **Keys are `keyRef`s, never material.** Rows carry
  `signer_name` + `key_ref` — an opaque pointer to whichever `Signer`
  owns the key; envelope-mode key refs *are* the ciphertext (a Vault
  or KMS wrapping).
- **Revocation is immediate and idempotent.**
  `Service.RevokeSigningKey` excludes the key from the cache-backed
  signing paths at once; `CAService.RevokeCertificate` is
  ledger-arbitrated (exactly one concurrent caller wins, the ledger
  and the row can never disagree); `VerifyCertificate` and CRLs refuse
  revoked material.
- **The tenant split.** `pki_signing_keys` and `pki_authorities` are
  platform data; `pki_certificates` is tenant data. Only
  `pki_revokeCertificate` reads the request tenant; the signing-key
  revoke's permission is evaluated in the platform domain even over
  HTTP.
- **Configuration** arrives as host options and declared config items
  (validity periods, propagation window, renewal lead time).
  `Service.ReclaimRetired` (host-invoked) destroys retired keys'
  underlying material; `PromoteNow` is the manual,
  propagation-honoring promotion companion to revocation.
- **Coded errors** — `pki.key_not_found`, `pki.certificate_revoked`,
  `pki.authority_revoked` and the rest — are indexed in the [error
  code index](../../error-codes/#pki).

## Limitations and links

- The X.509 layer has no expiry-driven lifecycle: nothing renews an
  authority or certificate automatically — a host tracks validity and
  calls `IssueCertificate` again. The residual unconsumed surface
  (JWKS exports, CRLDP embedding, periodic CRL regeneration,
  authority-revocation writers) is listed precisely in the `AGENTS.md`.
- No Vault or AWS-KMS integration leg exists: both provider packages
  are proven against stubbed clients (LocalStack's KMS diverges from
  the real service, which rules out an AWS leg by design).
- `EnsurePurpose` self-heals an expired key by revoking it and
  creating a replacement in the same call; it does not detect an
  algorithm mismatch on repeated calls (safe today, recorded).
- `LocalSigner` decrypts its key into process memory per `Sign` — the
  documented cost of the zero-dependency implementation.

### Source

- [go/pki/AGENTS.md](https://github.com/vislake/speed/blob/main/go/pki/AGENTS.md) — the authoritative document (signer seam, lifecycle, X.509 layer, residuals, limitations)
- Design rationale: [docs/internal/22-pki.md](https://github.com/vislake/speed/blob/main/docs/internal/22-pki.md)
- Related pages: [Platform services](../), [storage](storage/)
