package pki_test

// Runnable documentation for pki's public API, mirroring
// go/dbkit/example_test.go's convention: this example is compiled AND
// executed by `go test`, so a change to pki's public API that breaks the
// documented usage fails the build rather than only rotting in prose.
//
// This example is one of the three compensating obligations
// docs/internal/22-pki.md places on the X.509 layer for having no real
// consumer yet: it covers the layer's full main path -- issue a root CA, an
// intermediate signed by the root, and an end-entity certificate signed by
// the intermediate -- so at least this shape is known to compile and run
// under an external caller's own import, even without a real business
// module driving it.

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/pki"
)

// Example builds a three-level internal CA chain -- root, intermediate,
// end-entity -- entirely through pki's exported API, and verifies the
// resulting certificate chains correctly with the standard library's own
// crypto/x509.Verify.
func Example() {
	ctx := context.Background()

	// A real host opens PostgreSQL in the distributed deployment mode
	// (dbkit.DialectPostgres). SQLite keeps this example self-contained
	// under `go test`, with no external service required -- which is
	// exactly what the standalone deployment mode does in production too.
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:pki_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	// LocalSigner's private key column is encrypted at rest; a host
	// registers the cipher once at bootstrap, before opening this
	// database in a real application (the ordering matters here too --
	// GORM parses a model's serializer tag at first use).
	cipher, err := dbkit.NewCipher([]byte("01234567890123456789012345678901"))
	if err != nil {
		fmt.Println("new cipher:", err)
		return
	}
	if regErr := pki.RegisterLocalKeySerializer(cipher); regErr != nil {
		fmt.Println("register local key serializer:", regErr)
		return
	}

	// Migrations are versioned SQL, applied through dbkit's registry. There
	// is no AutoMigrate anywhere in this codebase.
	module := pki.NewModule(db)
	registry := dbkit.NewMigrationRegistry()
	if regErr := registry.Register(module); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := registry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	ca := module.CA()

	root, err := ca.CreateRootCA(ctx, pki.RootCAParams{
		Subject:  pkix.Name{CommonName: "speed Root CA"},
		NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour),
	})
	if err != nil {
		fmt.Println("create root CA:", err)
		return
	}

	intermediate, err := ca.CreateIntermediateCA(ctx, root.ID, pki.IntermediateCAParams{
		Subject:  pkix.Name{CommonName: "speed Intermediate CA"},
		NotAfter: time.Now().Add(5 * 365 * 24 * time.Hour),
	})
	if err != nil {
		fmt.Println("create intermediate CA:", err)
		return
	}

	// Certificate is tenant data, so issuing one requires a tenant in ctx --
	// the same rule every tenant-scoped repository in this codebase
	// enforces.
	tenantCtx := pkgcore.WithTenant(ctx, pkgcore.TenantID("acme-dental"))
	cert, err := ca.IssueCertificate(tenantCtx, intermediate.ID, pki.CertificateParams{
		Purpose:  "tenant.jwt_signing",
		Subject:  pkix.Name{CommonName: "acme.speed.internal"},
		DNSNames: []string{"acme.speed.internal"},
		NotAfter: time.Now().Add(365 * 24 * time.Hour),
	})
	if err != nil {
		fmt.Println("issue certificate:", err)
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

// parsePEM decodes a single PEM-encoded certificate, the form every
// CertificatePEM field in this module returns.
func parsePEM(certPEM string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	return x509.ParseCertificate(block.Bytes)
}
