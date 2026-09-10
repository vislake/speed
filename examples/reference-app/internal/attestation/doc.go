// Package attestation is the reference app's AI-output authenticity
// attestation layer -- the real consumer of go/pki's X.509 layer that
// closes the "no real consumer yet" exception docs/internal/22-pki.md and
// go/pki/AGENTS.md record (go/pki/AGENTS.md's "X.509 layer: real
// consumer, precise residuals" section has the full account).
//
// # The product problem
//
// A smile simulation's generated image is AI output over patient media
// (PHI-adjacent), and the clinic shares such outputs with patients and
// collaborating clinics through go/sharing's public access links. A
// shared output carries no proof of where it came from or whether the
// bytes are the bytes the platform generated: anyone who obtains the link
// (or the bytes) could serve altered images under the clinic's name. This
// package makes the platform vouch for the outputs it generates, in the
// one shape that is genuinely expressible on go/pki's API: the platform itself is the only legitimate holder of the private
// keys it issues (pki never delivers them), so the platform attests an
// output by signing a structured message -- object id, content SHA-256,
// tenant -- with a certificate it issued to that tenant
// (CAService.IssueCertificate + CAService.SignCertificate), records the
// attestation, and gates the public
// share of an attested output on chain verification of the certificate
// (CAService.VerifyCertificate), signature verification with the leaf's
// public key, and a live digest comparison of the bytes being served
// against the attested digest.
//
// # What is attested, and when
//
// An output is registered and attested when the app first observes its
// generating job as succeeded -- the completion definition
// internal/smilesim's Service establishes for a simulation: the job's
// terminal status, which the queue's terminal signal and the job-status
// route's poll alike report (see that package's doc comment). The
// observations that make an output visible to its own tenant -- the
// job-status poll, the per-photo enumeration and the simulation-content
// read, all in internal/app/smilesim.go -- each call EnsureAttested for a
// succeeded output, idempotently: an object with a row whose certificate
// is still active is left alone. Every reachable share of an output
// presupposes one of those observations (a share can only be minted
// against an output id a poll, enumeration or content read returned), so
// no output can be shared that was never registered.
//
// # The gate
//
// internal/app's sharing resolver (internal/app/sharing_resolver.go) calls
// CheckContent before serving any share whose resource it opened. An
// object with no
// attestation row -- an uploaded patient photo, any non-AI object -- is
// served exactly as before; an attested object is refused unless every
// check passes: the certificate verifies against its full chain (a
// revoked certificate or a revoked chain member refuses, forever, until
// the tenant re-attests under a new certificate), the signature over the
// stored message verifies with the leaf's public key, the message names
// this object and this tenant, and its digest equals the SHA-256 of the
// bytes about to be served. A refusal surfaces as the resolver error the
// sharing module answers as sharing.resource_unavailable (502): the
// module deliberately does not collapse a resource fault into the 404
// not-accessible shape, and this layer inherits that honest answer.
//
// # One certificate per tenant, re-issued on revocation
//
// Attestations are signed with a per-tenant certificate
// (CertificateParams.Purpose "simulation.attestation", one year of
// validity). The tenant's current certificate is the one its most recent
// attestation row names; EnsureAttested reuses it while it is active and,
// once revoked (RevokeCertificate through pki's HTTP surface -- the
// revocation drill every journey test drives), issues a fresh certificate
// and re-signs the object under it. An output whose attestation row names
// a revoked certificate stays refused at the gate until that output is
// observed again -- the clinic re-opening the case, which is exactly when
// a re-vouched output should become shareable again.
//
// # Boot responsibilities
//
// The app's CA chain (a root and one issuing intermediate authority, both
// platform rows in pki_authorities) is created once per database by
// EnsureAuthorityChain, called at every boot after Kernel.Bootstrap has
// applied pki's migrations; the chain is found by its fixed subject names
// on later boots, so a restart never mints a second chain. Concurrent
// first boots (two replicas of a distributed deployment sharing one
// database) can each mint a chain in the race window; every attestation
// row records the certificate that signed it and verification walks that
// certificate's own chain, so the duplication is cosmetic, never a
// correctness gap -- recorded in go/pki/AGENTS.md's consumer record.
package attestation
