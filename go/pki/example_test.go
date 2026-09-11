package pki_test

// Runnable documentation for pki's public API, mirroring
// go/dbkit/example_test.go's convention: these examples are compiled AND
// executed by `go test`, so a change to pki's public API that breaks the
// documented usage fails the build rather than only rotting in prose.
//
// The examples together discharge the godoc `Example` obligation the X.509
// layer carried while it had no real consumer, and retain it -- with a
// narrowed narrative -- now that the layer does (the reference app's
// AI-output attestation; go/pki/AGENTS.md's "X.509 layer: real consumer,
// precise residuals" section records what the obligation covers today),
// kept in step with the layer's growth:
//
//   - Example covers the layer's full main path -- issue a root CA, an
//     intermediate signed by the root, and an end-entity certificate
//     signed by the intermediate, then verify the resulting chain with the
//     standard library's own crypto/x509.Verify -- the shape the reference
//     app's attestation consumer drives for real, kept additionally
//     compilable-and-runnable under an external caller's own import.
//   - ExampleCAService_GenerateCRL drives the revocation path and
//     CRL generation: revoke an issued certificate, regenerate the issuing
//     authority's CRL, and read the document back with the standard
//     library's own parser.
//   - ExampleCAService_SignCertificate drives the consumer-side signing
//     path: sign a message with an issued certificate's key
//     (CAService.SignCertificate), verify the signature with the standard
//     library against the leaf's public key, and watch the signing call
//     refuse with ErrCertificateRevoked once the certificate is revoked.
//   - ExampleCAService_ExportAuthorityChainJWKS exercises the X.509
//     layer's JWKS export.
//   - ExampleService_ExportJWKS and ExampleService_RevokeSigningKey cover
//     the key-lifecycle layer's JWKS export and revocation halves.
//   - ExampleSignerRegistry resolves a Signer by registered name through
//     pki.SignerRegistry and signs with the resolved signer.
//   - ExampleBuildSignerRequiring resolves a Signer under a required
//     capability (BuildSignerRequiring): signer.local refused under a
//     KeyNeverLeavesBoundary requirement, accepted under none, and used to
//     sign.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/go/pki"
)

// newExampleModule opens a private, in-memory SQLite database named name
// (each example uses its own name, so examples never share state), applies
// pki's migrations to it and returns a ready *pki.Module plus a keepAlive
// handle to the underlying database. A real host opens PostgreSQL in the
// distributed deployment mode (dbkit.DialectPostgres); SQLite keeps these
// examples self-contained under `go test`, with no external service
// required -- which is exactly what the standalone deployment mode does in
// production too.
//
// The keepAlive handle is returned because a shared-cache in-memory SQLite
// database disappears once its last connection closes, and one example
// below (ExampleSignerRegistry) opens a SECOND connection to the same
// database through SignerRegistry.Build. Callers defer keepAlive.Close();
// an example whose database only ever has one connection is unaffected by
// holding it.
func newExampleModule(name string) (*pki.Module, *sql.DB, error) {
	// LocalSigner's private key column is encrypted at rest; a host
	// registers the cipher once at bootstrap, before opening this
	// database in a real application (the ordering matters here too --
	// GORM parses a model's serializer tag at first use). The registration
	// is process-global and every example uses the same 32-byte key, so
	// each example re-registering it is behaviourally a no-op.
	cipher, err := dbkit.NewCipher([]byte("01234567890123456789012345678901"))
	if err != nil {
		return nil, nil, err
	}
	if regErr := pki.RegisterLocalKeySerializer(cipher); regErr != nil {
		return nil, nil, regErr
	}

	ctx := context.Background()
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:" + name + "?mode=memory&cache=shared",
	})
	if err != nil {
		return nil, nil, err
	}

	// Migrations are versioned SQL, applied through dbkit's registry. There
	// is no AutoMigrate anywhere in this codebase.
	module := pki.NewModule(db)
	registry := dbkit.NewMigrationRegistry()
	if regErr := registry.Register(module); regErr != nil {
		return nil, nil, regErr
	}
	if applyErr := registry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		return nil, nil, applyErr
	}
	keepAlive, err := db.DB()
	if err != nil {
		return nil, nil, err
	}
	return module, keepAlive, nil
}

// exampleChain issues the module's three-level chain -- a root CA, an
// intermediate signed by the root, and one end-entity certificate signed by
// the intermediate under the example tenant -- entirely through pki's
// exported API, returning the three resulting rows.
func exampleChain(ca *pki.CAService) (root, intermediate *pki.Authority, cert *pki.Certificate, err error) {
	ctx := context.Background()
	root, err = ca.CreateRootCA(ctx, pki.RootCAParams{
		Subject:  pkix.Name{CommonName: "speed Root CA"},
		NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour),
	})
	if err != nil {
		return nil, nil, nil, err
	}

	intermediate, err = ca.CreateIntermediateCA(ctx, root.ID, pki.IntermediateCAParams{
		Subject:  pkix.Name{CommonName: "speed Intermediate CA"},
		NotAfter: time.Now().Add(5 * 365 * 24 * time.Hour),
	})
	if err != nil {
		return nil, nil, nil, err
	}

	// Certificate is tenant data, so issuing one requires a tenant in ctx --
	// the same rule every tenant-scoped repository in this codebase
	// enforces.
	tenantCtx := pkgcore.WithTenant(ctx, pkgcore.TenantID("acme-dental"))
	cert, err = ca.IssueCertificate(tenantCtx, intermediate.ID, pki.CertificateParams{
		Purpose:  "tenant.jwt_signing",
		Subject:  pkix.Name{CommonName: "acme.speed.internal"},
		DNSNames: []string{"acme.speed.internal"},
		NotAfter: time.Now().Add(365 * 24 * time.Hour),
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return root, intermediate, cert, nil
}

// Example builds a three-level internal CA chain -- root, intermediate,
// end-entity -- entirely through pki's exported API, and verifies the
// resulting certificate chains correctly with the standard library's own
// crypto/x509.Verify.
func Example() {
	module, keepAlive, err := newExampleModule("pki_example")
	if err != nil {
		fmt.Println("setup:", err)
		return
	}
	defer keepAlive.Close()

	ca := module.CA()

	root, intermediate, cert, err := exampleChain(ca)
	if err != nil {
		fmt.Println("issue chain:", err)
		return
	}

	// Verifying the chain with the standard library's own crypto/x509,
	// exactly as an external consumer would, without any pki-specific
	// verification helper.
	rootCert, err := parsePEM(root.CertificatePEM)
	if err != nil {
		fmt.Println("parse root cert:", err)
		return
	}
	intermediateCert, err := parsePEM(intermediate.CertificatePEM)
	if err != nil {
		fmt.Println("parse intermediate cert:", err)
		return
	}
	endEntityCert, err := parsePEM(cert.CertificatePEM)
	if err != nil {
		fmt.Println("parse end-entity cert:", err)
		return
	}

	roots := x509.NewCertPool()
	roots.AddCert(rootCert)
	intermediates := x509.NewCertPool()
	intermediates.AddCert(intermediateCert)

	chains, err := endEntityCert.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates})
	if err != nil {
		fmt.Println("verify:", err)
		return
	}
	fmt.Printf("verified chains: %d\n", len(chains))
	fmt.Printf("chain length: %d\n", len(chains[0]))
	fmt.Printf("end-entity subject: %s\n", endEntityCert.Subject.CommonName)

	// Output:
	// verified chains: 1
	// chain length: 3
	// end-entity subject: acme.speed.internal
}

// ExampleCAService_GenerateCRL walks the revocation path an external
// caller drives: revoke an end-entity certificate, regenerate the issuing
// authority's CRL so the revocation is published, then read the generated
// document back with the standard library's own parser -- the way an
// independent verifier would -- and confirm the revoked certificate's
// serial is listed and the document carries the authority's signature. It
// ends by confirming the module's own chain verification refuses the
// revoked certificate with the coded ErrCertificateRevoked.
func ExampleCAService_GenerateCRL() {
	module, keepAlive, err := newExampleModule("pki_example_crl")
	if err != nil {
		fmt.Println("setup:", err)
		return
	}
	defer keepAlive.Close()

	ca := module.CA()
	ctx := context.Background()

	_, intermediate, cert, err := exampleChain(ca)
	if err != nil {
		fmt.Println("issue chain:", err)
		return
	}

	tenantCtx := pkgcore.WithTenant(ctx, pkgcore.TenantID("acme-dental"))
	revoked, err := ca.RevokeCertificate(tenantCtx, cert.ID, "compromised")
	if err != nil {
		fmt.Println("revoke certificate:", err)
		return
	}
	fmt.Println("revoked:", revoked)

	// A zero validity falls back to DefaultCRLValidity; the generated
	// document is persisted on the authority row the method returns.
	authority, err := ca.GenerateCRL(ctx, intermediate.ID, 0)
	if err != nil {
		fmt.Println("generate CRL:", err)
		return
	}
	fmt.Println("crl number:", authority.CRLNumber)

	entries, listsSerial, signatureOK, err := readCRL(authority.CRLPEM, cert, intermediate)
	if err != nil {
		fmt.Println("read CRL:", err)
		return
	}
	fmt.Println("crl revoked entries:", entries)
	fmt.Println("crl lists the revoked serial:", listsSerial)
	fmt.Println("crl signature verifies:", signatureOK)

	_, verifyErr := ca.VerifyCertificate(tenantCtx, cert.ID)
	refused := apperr.HasCode(verifyErr, pki.ErrCertificateRevoked.Code)
	fmt.Println("verify refuses the revoked certificate with ErrCertificateRevoked:", refused)

	// Output:
	// revoked: true
	// crl number: 1
	// crl revoked entries: 1
	// crl lists the revoked serial: true
	// crl signature verifies: true
	// verify refuses the revoked certificate with ErrCertificateRevoked: true
}

// readCRL decodes and parses a PEM-encoded X.509 CRL with the standard
// library, then checks it the way an independent verifier would against the
// certificate whose revocation the CRL should publish (cert) and the
// authority that issued both (issuer): how many revoked entries the
// document lists, whether the revoked certificate's own serial number is
// among them, and whether the document's signature checks out against the
// issuing authority's certificate.
func readCRL(crlPEM string, cert *pki.Certificate, issuer *pki.Authority) (entries int, listsSerial, signatureOK bool, err error) {
	block, _ := pem.Decode([]byte(crlPEM))
	if block == nil {
		return 0, false, false, errors.New("no CRL PEM block found")
	}
	rl, err := x509.ParseRevocationList(block.Bytes)
	if err != nil {
		return 0, false, false, err
	}
	certParsed, err := parsePEM(cert.CertificatePEM)
	if err != nil {
		return 0, false, false, err
	}
	issuerParsed, err := parsePEM(issuer.CertificatePEM)
	if err != nil {
		return 0, false, false, err
	}

	listed := false
	for _, entry := range rl.RevokedCertificateEntries {
		if entry.SerialNumber.Cmp(certParsed.SerialNumber) == 0 {
			listed = true
		}
	}
	return len(rl.RevokedCertificateEntries), listed, rl.CheckSignatureFrom(issuerParsed) == nil, nil
}

// ExampleCAService_SignCertificate drives the signing half of the
// issue -> sign -> verify loop the X.509 layer's real consumer (the
// reference app's AI-output attestation) runs: sign a message with an
// issued certificate's key through CAService.SignCertificate, then check
// the signature with the standard library against the leaf certificate's
// own public key -- the same check the app's sharing gate performs before
// serving an attested output -- and confirm that once the certificate is
// revoked, the same signing call refuses with the coded
// ErrCertificateRevoked.
func ExampleCAService_SignCertificate() {
	module, keepAlive, err := newExampleModule("pki_example_sign_certificate")
	if err != nil {
		fmt.Println("setup:", err)
		return
	}
	defer keepAlive.Close()

	ca := module.CA()
	ctx := context.Background()

	_, _, cert, err := exampleChain(ca)
	if err != nil {
		fmt.Println("issue chain:", err)
		return
	}

	tenantCtx := pkgcore.WithTenant(ctx, pkgcore.TenantID("acme-dental"))
	message := []byte(`{"object_id":"sim-1","content_sha256":"abc","tenant_id":"acme-dental"}`)
	signature, err := ca.SignCertificate(tenantCtx, cert.ID, message)
	if err != nil {
		fmt.Println("sign certificate:", err)
		return
	}
	fmt.Println("signature length:", len(signature))

	leaf, err := parsePEM(cert.CertificatePEM)
	if err != nil {
		fmt.Println("parse leaf certificate:", err)
		return
	}
	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		fmt.Println("leaf public key:", "not ed25519")
		return
	}
	fmt.Println("signature verifies against the certificate:", ed25519.Verify(pub, message, signature))

	if _, err := ca.RevokeCertificate(tenantCtx, cert.ID, "compromised"); err != nil {
		fmt.Println("revoke certificate:", err)
		return
	}
	_, signErr := ca.SignCertificate(tenantCtx, cert.ID, message)
	refused := apperr.HasCode(signErr, pki.ErrCertificateRevoked.Code)
	fmt.Println("sign refuses a revoked certificate with ErrCertificateRevoked:", refused)

	// Output:
	// signature length: 64
	// signature verifies against the certificate: true
	// sign refuses a revoked certificate with ErrCertificateRevoked: true
}

// ExampleCAService_ExportAuthorityChainJWKS exports an intermediate
// authority's certificate chain as an RFC 7517 JSON Web Key Set -- the
// document a data-plane cluster kid-matches against -- and checks the
// result with the standard library: two keys (the intermediate itself,
// then its root issuer), ordered leaf-first, whose public-key material
// matches the authorities' own certificates byte for byte.
//
// One corner of this method cannot be shown from an example: a chain
// containing a revoked authority (the intermediate itself or any ancestor)
// is refused wholesale with ErrCertificateRevoked, but no public method
// writes AuthorityStatusRevoked -- model.go's own AuthorityStatus doc
// comment records that -- so the refusal is driven only by this module's
// own unit suite, which seeds the row directly (revocation_test.go).
func ExampleCAService_ExportAuthorityChainJWKS() {
	module, keepAlive, err := newExampleModule("pki_example_chain_jwks")
	if err != nil {
		fmt.Println("setup:", err)
		return
	}
	defer keepAlive.Close()

	ca := module.CA()
	ctx := context.Background()

	root, intermediate, _, err := exampleChain(ca)
	if err != nil {
		fmt.Println("issue chain:", err)
		return
	}

	// The authority chain is platform public-key material; no tenant is
	// needed in ctx.
	jwks, err := ca.ExportAuthorityChainJWKS(ctx, intermediate.ID)
	if err != nil {
		fmt.Println("export authority chain JWKS:", err)
		return
	}
	fmt.Println("chain jwks keys:", len(jwks.Keys))

	leafFirst := len(jwks.Keys) == 2 &&
		jwks.Keys[0].KeyID == intermediate.ID &&
		jwks.Keys[1].KeyID == root.ID
	fmt.Println("chain jwks ordered leaf-first by authority id:", leafFirst)

	rootCert, err := parsePEM(root.CertificatePEM)
	if err != nil {
		fmt.Println("parse root certificate:", err)
		return
	}
	matches := false
	if len(jwks.Keys) == 2 {
		if jwkPub, ok := jwks.Keys[1].Key.(ed25519.PublicKey); ok {
			if certPub, ok := rootCert.PublicKey.(ed25519.PublicKey); ok {
				matches = bytes.Equal(jwkPub, certPub)
			}
		}
	}
	fmt.Println("chain jwks root key matches the root certificate:", matches)

	// Output:
	// chain jwks keys: 2
	// chain jwks ordered leaf-first by authority id: true
	// chain jwks root key matches the root certificate: true
}

// ExampleService_ExportJWKS exports the key-lifecycle layer's active and
// retiring public keys as an RFC 7517 JSON Web Key Set -- the document an
// EXTERNAL verifier of speed-issued tokens fetches (in-process verification
// uses KeySource instead, never this export). It shows the one-key answer
// for a provisioned purpose, proves the exported key genuinely verifies a
// signature the active key produced -- the external verifier's whole job --
// and shows the empty-set answer for a purpose that was never provisioned.
func ExampleService_ExportJWKS() {
	module, keepAlive, err := newExampleModule("pki_example_export_jwks")
	if err != nil {
		fmt.Println("setup:", err)
		return
	}
	defer keepAlive.Close()

	svc := module.Service()
	ctx := context.Background()

	const purpose = "authn.access_token"
	err = svc.EnsurePurpose(ctx, purpose, pki.AlgorithmEd25519, 15*time.Minute)
	if err != nil {
		fmt.Println("ensure purpose:", err)
		return
	}
	_, _, sign, err := svc.ActiveSigner(ctx, purpose)
	if err != nil {
		fmt.Println("active signer:", err)
		return
	}

	jwks, err := svc.ExportJWKS(ctx, purpose)
	if err != nil {
		fmt.Println("export JWKS:", err)
		return
	}
	fmt.Println("export for a provisioned purpose:", len(jwks.Keys), "key")

	// The exported key is public-key material only; verify a live
	// signature with it exactly as an external verifier would.
	verified := false
	if len(jwks.Keys) == 1 {
		if pub, ok := jwks.Keys[0].Key.(ed25519.PublicKey); ok {
			message := []byte("speed export example")
			sig, signErr := sign(ctx, message)
			if signErr == nil {
				verified = ed25519.Verify(pub, message, sig)
			}
		}
	}
	fmt.Println("exported key verifies a live signature:", verified)

	unprovisioned, err := svc.ExportJWKS(ctx, "tenant.jwt_signing")
	if err != nil {
		fmt.Println("export unprovisioned JWKS:", err)
		return
	}
	fmt.Println("export for an unprovisioned purpose:", len(unprovisioned.Keys), "keys")

	// Output:
	// export for a provisioned purpose: 1 key
	// exported key verifies a live signature: true
	// export for an unprovisioned purpose: 0 keys
}

// ExampleService_RevokeSigningKey shows emergency revocation on the
// key-lifecycle layer: revoke the purpose's active key and watch the very
// next ExportJWKS answer drop from one key to an empty set -- the revoked
// key leaves the published set immediately, because the revocation
// invalidates the key-set cache through the same event mechanism a remote
// replica's revocation would use.
func ExampleService_RevokeSigningKey() {
	module, keepAlive, err := newExampleModule("pki_example_revoke_signing_key")
	if err != nil {
		fmt.Println("setup:", err)
		return
	}
	defer keepAlive.Close()

	svc := module.Service()
	ctx := context.Background()

	const purpose = "authn.access_token"
	err = svc.EnsurePurpose(ctx, purpose, pki.AlgorithmEd25519, 15*time.Minute)
	if err != nil {
		fmt.Println("ensure purpose:", err)
		return
	}

	before, err := svc.ExportJWKS(ctx, purpose)
	if err != nil {
		fmt.Println("export before revoke:", err)
		return
	}
	fmt.Println("keys before revoke:", len(before.Keys))

	kid, _, _, err := svc.ActiveSigner(ctx, purpose)
	if err != nil {
		fmt.Println("active signer:", err)
		return
	}
	revoked, err := svc.RevokeSigningKey(ctx, kid, "compromised")
	if err != nil {
		fmt.Println("revoke signing key:", err)
		return
	}
	fmt.Println("revoked:", revoked)

	after, err := svc.ExportJWKS(ctx, purpose)
	if err != nil {
		fmt.Println("export after revoke:", err)
		return
	}
	fmt.Println("keys after revoke:", len(after.Keys))

	// Output:
	// keys before revoke: 1
	// revoked: true
	// keys after revoke: 0
}

// ExampleSignerRegistry shows resolving a Signer by registered name through
// pki.SignerRegistry -- the database/sql-style driver pattern -- with the
// module's own zero-external-dependency "signer.local" registration, which
// builds an offline-runnable LocalSigner from a flat pkgcore.Config naming
// a database. The resolved signer then generates a key and signs with it
// for real; an unknown name is refused with
// pkgcore.ErrUnknownImplementation.
//
// The vault and kmsaws provider names ("signer.vault",
// "signer.vault-direct", "signer.aws-kms", "signer.aws-kms-direct")
// execute the same Build path in their own packages' example tests
// (go/pki/signer/vault and go/pki/signer/kmsaws), which run under the same
// `go test`. Only client construction can run there: a real Vault server
// or AWS account is not reachable from the unit-test tier, so no example
// in either package performs a Sign against a live backend -- each
// package's doc.go records that boundary.
func ExampleSignerRegistry() {
	module, keepAlive, err := newExampleModule("pki_example_registry")
	if err != nil {
		fmt.Println("setup:", err)
		return
	}
	defer keepAlive.Close()

	ctx := context.Background()

	// Build opens its OWN connection to the same in-memory database the
	// module above migrated; keepAlive (deferred above) is what keeps the
	// database alive for it -- signer_registry.go's localSignerFromConfig
	// doc comment explains why this registry entry cannot share the
	// module's *gorm.DB.
	signer, caps, err := pki.SignerRegistry.Build("signer.local", pkgcore.Config{
		"dialect": string(dbkit.DialectSQLite),
		"dsn":     "file:pki_example_registry?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("build signer.local:", err)
		return
	}
	fmt.Println("signer.local:", err, signer != nil, caps)

	if _, _, lookupErr := pki.SignerRegistry.Build("signer.no-such-provider", nil); lookupErr == nil {
		fmt.Println("unknown registered name refused: false")
		return
	}
	fmt.Println("unknown registered name refused: true")
	_ = module

	keyRef, pub, err := signer.GenerateKey(ctx, pki.AlgorithmEd25519)
	if err != nil {
		fmt.Println("generate key:", err)
		return
	}
	message := []byte("speed registry example")
	sig, err := signer.Sign(ctx, keyRef, message)
	if err != nil {
		fmt.Println("sign:", err)
		return
	}
	gotPub, err := signer.Public(ctx, keyRef)
	if err != nil {
		fmt.Println("public:", err)
		return
	}
	working := bytes.Equal(gotPub.(ed25519.PublicKey), pub.(ed25519.PublicKey)) &&
		ed25519.Verify(pub.(ed25519.PublicKey), message, sig)
	fmt.Println("registry-resolved signer signs and verifies:", working)

	// Output:
	// signer.local: <nil> true none
	// unknown registered name refused: true
	// registry-resolved signer signs and verifies: true
}

// ExampleBuildSignerRequiring demonstrates the pki-local capability check a
// host that intends to require pkgcore.KeyNeverLeavesBoundary of the signer
// it wires resolves its signer through: BuildSignerRequiring behaves exactly
// like pki.SignerRegistry.Build, and additionally refuses a resolution whose
// registration's declared capability cannot satisfy the requirement -- the
// comparison pkgcore.Kernel.Bootstrap performs for its own four built-in
// seams but has no knowledge of for pki.Signer (signer_registry.go's
// BuildSignerRequiring doc comment, and go/pki/AGENTS.md's Known
// limitations, have the full account). "signer.local" does not declare the
// capability -- LocalSigner decrypts key material into process memory to
// sign -- so resolving it under a KeyNeverLeavesBoundary requirement is
// refused with an error naming the signer and the missing capability. The
// registered names that DO carry the capability are the direct-sign
// provider names a host blank-imports (go/pki/signer/vault's
// "signer.vault-direct", go/pki/signer/kmsaws's "signer.aws-kms-direct");
// resolving one of those under the same requirement succeeds. A zero
// requirement (the shape a host with no boundary intent passes) accepts any
// signer, "signer.local" included, and the resolution then signs normally.
func ExampleBuildSignerRequiring() {
	module, keepAlive, err := newExampleModule("pki_example_requiring")
	if err != nil {
		fmt.Println("setup:", err)
		return
	}
	defer keepAlive.Close()

	ctx := context.Background()
	cfg := pkgcore.Config{
		"dialect": string(dbkit.DialectSQLite),
		"dsn":     "file:pki_example_requiring?mode=memory&cache=shared",
	}

	signer, err := pki.BuildSignerRequiring("signer.local", cfg, pkgcore.KeyNeverLeavesBoundary)
	if err == nil {
		fmt.Println("signer.local under KeyNeverLeavesBoundary refused: false")
		return
	}
	fmt.Println("signer.local under KeyNeverLeavesBoundary refused: true")

	signer, err = pki.BuildSignerRequiring("signer.local", cfg, 0)
	if err != nil {
		fmt.Println("signer.local with no requirement:", err)
		return
	}

	keyRef, pub, err := signer.GenerateKey(ctx, pki.AlgorithmEd25519)
	if err != nil {
		fmt.Println("generate key:", err)
		return
	}
	message := []byte("speed requirement example")
	sig, err := signer.Sign(ctx, keyRef, message)
	if err != nil {
		fmt.Println("sign:", err)
		return
	}
	fmt.Println("requirement-free signer signs and verifies:",
		ed25519.Verify(pub.(ed25519.PublicKey), message, sig))
	_ = module

	// Output:
	// signer.local under KeyNeverLeavesBoundary refused: true
	// requirement-free signer signs and verifies: true
}

// parsePEM decodes a single PEM-encoded certificate, the form every
// CertificatePEM field in this module returns.
func parsePEM(certPEM string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	return x509.ParseCertificate(block.Bytes)
}
