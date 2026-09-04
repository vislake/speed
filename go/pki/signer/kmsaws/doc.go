// Package kmsaws is a pki.Signer implementation backed by AWS Key
// Management Service, per docs/internal/22-pki.md's "three
// implementations, each its own subpackage" table. It is split out of
// go/pki's own root package for the same
// dependency-isolation reason go/pki/signer/vault is: a consumer which
// never wires a KMS-backed signer does not inherit the AWS SDK for Go v2's
// KMS client and its transitive dependencies (3 indirect modules, measured
// the same way -- a throwaway module, GOWORK=off go mod tidy, count "//
// indirect" entries; see go/pki/AGENTS.md's Known limitations for the
// recorded number). The measurement uses only
// github.com/aws/aws-sdk-go-v2/service/kms plus
// github.com/aws/aws-sdk-go-v2/credentials for explicit static
// credentials, deliberately NOT github.com/aws/aws-sdk-go-v2/config's
// default-credential-chain resolver (SSO/STS/OIDC and their own
// transitive closure) -- see config.go's doc comment for why.
//
// Importing this package registers two names on pki.SignerRegistry as a
// side effect (see register.go): "signer.aws-kms" (envelope mode) and
// "signer.aws-kms-direct" (direct-sign mode) -- the identical two-names-
// per-provider shape go/pki/signer/vault uses, and for the identical
// reason (see that package's own doc comment: pkgcore.SeamRegistry[T]
// pairs one fixed Capability with one name, so a capability that depends
// on a runtime Mode needs two names, not one that lies about it depending
// on what a given Config selected).
//
// # Ed25519 signing algorithm: RAW message type, not DIGEST -- get this
// # exactly right
//
// docs/internal/22-pki.md's "Ed25519 direct-signs on all three
// implementations" section (verified 2026-09-04) is explicit about a
// distinction that is easy to get subtly,
// silently wrong: AWS KMS's ECC_NIST_EDWARDS25519 key spec supports two
// signing algorithms, ED25519_SHA_512 (FIPS 186-5 §7.6, PureEdDSA -- RFC
// 8037's JWT EdDSA) with MessageType RAW, and ED25519_PH_SHA_512 (FIPS
// 186-5 §7.8, HashEdDSA/Ed25519ph) with MessageType DIGEST. These are NOT
// interchangeable: a signature made under the DIGEST/PH combination does
// not verify against a PureEdDSA verifier (crypto/ed25519.Verify)
// SILENTLY -- it signs successfully and fails only at verification time,
// against a signature that already went out the door. This package's
// direct-sign mode uses ED25519_SHA_512 with MessageType RAW exclusively
// (signDirect in signer.go), and TestSignDirect_ProducesAPureEdDSASignature
// pins a real crypto/ed25519.Verify call against a signature this
// package's own decode path produced, so a future edit that swaps the
// algorithm or MessageType constant fails a test rather than silently
// shipping a signature nothing can verify.
//
// # No integration leg -- by design, not by gap
//
// docs/internal/22-pki.md's testing-strategy section says this outright:
// AWS KMS gets no integration leg -- unit-test against a stubbed SDK
// interface, real verification is manual. LocalStack's KMS implementation
// is known
// to diverge from the real service, so a testcontainers-backed
// integration_test/ package here would prove conformance against
// LocalStack, not against AWS KMS -- worse than no such package, since it
// would look like coverage without being coverage. This package's
// signer_test.go stubs the six kms.Client methods this package calls
// (kmsClient, signer.go) and exercises every request/response shape
// against those stubs; nothing here has been run against a real AWS
// account, and go/pki/AGENTS.md's Known limitations records that plainly.
package kmsaws
