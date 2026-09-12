package kmsaws_test

// Runnable documentation for the AWS KMS-backed Signer, compiled and
// executed by `go test`. Neither Example below reaches a real AWS account
// -- AWS KMS has no integration leg, by design (see doc.go: LocalStack's
// KMS implementation is known to diverge from the real service). Both
// demonstrate construction and component-face usage only.

import (
	"context"
	"fmt"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/pki"
	"github.com/vislake/speed/go/pki/signer/kmsaws"
)

// ExampleNewSigner shows constructing a direct-sign-mode Signer directly,
// the escape hatch for a caller that wants to wire it with pki.WithSigner
// rather than selecting the component in a composition. Nothing is dialed here --
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

// Example demonstrates the package's component registration: importing it
// for side effect makes "signer.aws-kms" and "signer.aws-kms-direct"
// selectable in a composition, each constructing the package's own signer
// from its settings under the capability its descriptor declares.
func Example() {
	ctx := context.Background()
	for _, name := range []string{"signer.aws-kms", "signer.aws-kms-direct"} {
		reg := pkgcore.NewComponentRegistry()
		reg.Put(pkgcore.NewComponentConfig(map[string]any{
			"components": map[string]any{
				name: map[string]any{
					"region":            "us-east-1",
					"access_key_id":     "AKIAEXAMPLE",
					"secret_access_key": "example-secret",
					"wrapping_key_id":   "alias/pki-wrapping-key",
				},
			},
		}))
		err := reg.Prepare(ctx)
		if err == nil {
			err = reg.Construct(ctx)
		}
		var signer pki.Signer
		if err == nil {
			signer, err = pkgcore.Get[pki.Signer](reg)
		}
		var caps pkgcore.Capability
		if err == nil {
			caps, err = pkgcore.ComponentCapabilities(reg, name)
		}
		fmt.Println(name+":", err, signer != nil, caps)
		_ = reg.Close(ctx)
	}

	// Output:
	// signer.aws-kms: <nil> true none
	// signer.aws-kms-direct: <nil> true KeyNeverLeavesBoundary
}
