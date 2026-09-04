package kmsaws_test

// Runnable documentation for the AWS KMS-backed Signer, compiled and
// executed by `go test`. Neither Example below reaches a real AWS account
// -- see doc.go's own "no integration leg" section (docs/internal/22-pki.md's
// own testing-strategy note that AWS KMS gets no integration leg even in
// its target design, LocalStack's KMS implementation being known to
// diverge from the real service). Both demonstrate construction and
// pki.SignerRegistry usage only.

import (
	"fmt"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/pki"
	"github.com/vislake/speed/go/pki/signer/kmsaws"
)

// ExampleNewSigner shows constructing a direct-sign-mode Signer directly,
// the escape hatch for a caller that wants to wire it with pki.WithSigner
// rather than going through pki.SignerRegistry. Nothing is dialed here --
// the underlying KMS client issues no request until the first operation --
// so this Example never needs reachable AWS credentials to construct
// successfully.
func ExampleNewSigner() {
	signer, err := kmsaws.NewSigner(kmsaws.Config{
		Region:          "us-east-1",
		AccessKeyID:     "AKIAEXAMPLE",
		SecretAccessKey: "example-secret",
		Mode:            kmsaws.ModeDirectSign,
	})
	if err != nil {
		fmt.Println("new signer:", err)
		return
	}
	//nolint:staticcheck // QF1011: the assertion doubles as written doc that
	// NewSigner satisfies pki.Signer.
	var _ pki.Signer = signer

	fmt.Println("signer wired; the first Sign call contacts AWS KMS")
	// Output:
	// signer wired; the first Sign call contacts AWS KMS
}

// Example demonstrates the package's self-registration: importing it for
// side effect makes "signer.aws-kms" and "signer.aws-kms-direct" build
// through pki.SignerRegistry.
func Example() {
	cfg := pkgcore.Config{
		"region":            "us-east-1",
		"access_key_id":     "AKIAEXAMPLE",
		"secret_access_key": "example-secret",
		"wrapping_key_id":   "alias/pki-wrapping-key",
	}
	envelopeSigner, caps, err := pki.SignerRegistry.Build("signer.aws-kms", cfg)
	fmt.Println("signer.aws-kms:", err, envelopeSigner != nil, caps)

	directSigner, caps, err := pki.SignerRegistry.Build("signer.aws-kms-direct", cfg)
	fmt.Println("signer.aws-kms-direct:", err, directSigner != nil, caps)

	// Output:
	// signer.aws-kms: <nil> true none
	// signer.aws-kms-direct: <nil> true KeyNeverLeavesBoundary
}
