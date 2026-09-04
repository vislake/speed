package vault_test

// Runnable documentation for the Vault-backed Signer, compiled and executed
// by `go test` like every other package's examples in this codebase, so an
// API change that invalidates the documented usage fails the build instead
// of only rotting in prose.
//
// Neither Example below reaches a real Vault server: see doc.go's own note
// on why no offline-runnable Example against a live Transit engine exists
// in this package (Docker-backed testcontainers, unavailable in the plain
// unit-test tier godoc Examples run under). Both demonstrate construction
// and pki.SignerRegistry usage only -- exactly what the round asked these
// Examples to show.

import (
	"fmt"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/pki"
	"github.com/vislake/speed/go/pki/signer/vault"
)

// ExampleNewSigner shows constructing a direct-sign-mode Signer directly,
// the escape hatch for a caller that wants to wire it with pki.WithSigner
// rather than going through pki.SignerRegistry. Nothing is dialed here --
// the underlying Vault client, like every other built-in seam's client in
// this codebase, connects lazily on first use -- so this Example never
// needs a reachable Vault server to construct successfully.
func ExampleNewSigner() {
	signer, err := vault.NewSigner(vault.Config{
		Address: "https://vault.example.com:8200",
		Token:   "s.example-token",
		Mode:    vault.ModeDirectSign,
	})
	if err != nil {
		fmt.Println("new signer:", err)
		return
	}
	//nolint:staticcheck // QF1011: the assertion doubles as written doc that
	// NewSigner satisfies pki.Signer -- kept rather than inlined, which
	// would leave the value unused.
	var _ pki.Signer = signer

	fmt.Println("signer wired; the first Sign call contacts Vault")
	// Output:
	// signer wired; the first Sign call contacts Vault
}

// Example demonstrates the package's self-registration: importing it for
// side effect -- as a host does with a blank import when it wants
// pki.SignerRegistry to resolve "signer.vault" or "signer.vault-direct" --
// makes both names build through pki.SignerRegistry, the database/sql-style
// driver pattern this package follows.
func Example() {
	cfg := pkgcore.Config{
		"address":           "https://vault.example.com:8200",
		"token":             "s.example-token",
		"wrapping_key_name": "pki-wrapping-key",
	}
	envelopeSigner, caps, err := pki.SignerRegistry.Build("signer.vault", cfg)
	fmt.Println("signer.vault:", err, envelopeSigner != nil, caps)

	directSigner, caps, err := pki.SignerRegistry.Build("signer.vault-direct", cfg)
	fmt.Println("signer.vault-direct:", err, directSigner != nil, caps)

	// Output:
	// signer.vault: <nil> true none
	// signer.vault-direct: <nil> true KeyNeverLeavesBoundary
}
