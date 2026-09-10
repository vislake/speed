package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/vislake/speed/go/dbkit"
)

// derivation_test.go pins the bootstrap-key derivation contract: the six
// declared key paths' frozen purpose strings, the path-shape refusal, and
// the derived material's exact reproduction from (rootKey, literal
// purpose) -- BootstrapKeyPurpose's stability contract in executable form.
//
// The purpose strings below are deliberately literals rather than calls
// back into BootstrapKeyPurpose: a test whose expectation came from the
// function under test could not catch a purpose string being edited, which
// is exactly the change that would silently rotate every deployed key.

// bootstrapKeyDerivationCases lists the six declared bootstrap key paths
// this repository's modules register and the purpose string each freezes.
var bootstrapKeyDerivationCases = []struct{ keyPath, purpose string }{
	{"authn.blind_index_key", "speed.authn.blind_index_key.v1"},
	{"authn.pii_cipher_key", "speed.authn.pii_cipher_key.v1"},
	{"config.master_key", "speed.config.master_key.v1"},
	{"notification.contact_index_key", "speed.notification.contact_index_key.v1"},
	{"org.invitation_email_index_key", "speed.org.invitation_email_index_key.v1"},
	{"pki.local_key_cipher_key", "speed.pki.local_key_cipher_key.v1"},
}

// TestBootstrapKeyPurpose_FrozenDeclaredPaths pins every frozen purpose
// string: renaming a declared key path (or editing a purpose) re-derives
// different material for whatever the old string named, so a change here is
// a key rotation that must ship as one, never a plain edit.
func TestBootstrapKeyPurpose_FrozenDeclaredPaths(t *testing.T) {
	seen := make(map[string]string, len(bootstrapKeyDerivationCases))
	for _, tt := range bootstrapKeyDerivationCases {
		got, err := BootstrapKeyPurpose(tt.keyPath)
		if err != nil {
			t.Fatalf("BootstrapKeyPurpose(%q): %v", tt.keyPath, err)
		}
		if got != tt.purpose {
			t.Fatalf("BootstrapKeyPurpose(%q) = %q, want the frozen %q; changing it rotates the key that path names", tt.keyPath, got, tt.purpose)
		}
		if other, dup := seen[got]; dup {
			t.Fatalf("%q and %q share the purpose %q; two declared keys must never derive the same material", tt.keyPath, other, got)
		}
		seen[got] = tt.keyPath
	}
}

// TestBootstrapKeyPurpose_RefusesMalformedPaths pins the shape rule: an
// empty path or an empty segment is refused with the offending path under
// "key", so a doubled or dangling dot fails at the call site instead of
// deriving material under a purpose nobody meant to freeze.
func TestBootstrapKeyPurpose_RefusesMalformedPaths(t *testing.T) {
	for _, tt := range []struct{ name, keyPath string }{
		{"empty", ""},
		{"only-dot", "."},
		{"leading-dot", ".config.master_key"},
		{"trailing-dot", "config.master_key."},
		{"doubled-dot", "config..master_key"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := BootstrapKeyPurpose(tt.keyPath); err == nil {
				t.Fatalf("BootstrapKeyPurpose(%q) accepted a malformed path", tt.keyPath)
			} else {
				assertCode(t, err, ErrInvalidBootstrapKeyPath)
				assertParam(t, err, "key", tt.keyPath)
			}
		})
	}
}

// TestDeriveBootstrapKeyMaterial_ReproducesFrozenPurposes pins the derived
// material itself: for each declared path, the result is exactly
// dbkit.DeriveKey over the frozen purpose literal -- the bytes a caller who
// knew only the root key and the frozen string would reproduce
// independently -- 32 bytes long, deterministic across calls, and pairwise
// distinct, so no wiring mistake can collapse two declared keys onto one
// material.
func TestDeriveBootstrapKeyMaterial_ReproducesFrozenPurposes(t *testing.T) {
	rootKey := sha256.Sum256([]byte("TestDeriveBootstrapKeyMaterial root key"))

	seen := make(map[string]string, len(bootstrapKeyDerivationCases))
	for _, tt := range bootstrapKeyDerivationCases {
		got, err := DeriveBootstrapKeyMaterial(rootKey[:], tt.keyPath)
		if err != nil {
			t.Fatalf("DeriveBootstrapKeyMaterial(%q): %v", tt.keyPath, err)
		}
		if len(got) != 32 {
			t.Fatalf("DeriveBootstrapKeyMaterial(%q) returned %d bytes, want 32", tt.keyPath, len(got))
		}

		want, deriveErr := dbkit.DeriveKey(rootKey[:], tt.purpose)
		if deriveErr != nil {
			t.Fatalf("dbkit.DeriveKey(rootKey, %q): %v", tt.purpose, deriveErr)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("DeriveBootstrapKeyMaterial(%q) = %x, want dbkit.DeriveKey over the frozen %q = %x", tt.keyPath, got, tt.purpose, want)
		}

		again, err := DeriveBootstrapKeyMaterial(rootKey[:], tt.keyPath)
		if err != nil {
			t.Fatalf("DeriveBootstrapKeyMaterial(%q) second call: %v", tt.keyPath, err)
		}
		if !bytes.Equal(got, again) {
			t.Fatalf("DeriveBootstrapKeyMaterial(%q) is not deterministic: %x then %x", tt.keyPath, got, again)
		}

		digest := hex.EncodeToString(got)
		if other, dup := seen[digest]; dup {
			t.Fatalf("%q and %q derived the identical material %s", tt.keyPath, other, digest)
		}
		seen[digest] = tt.keyPath
	}
}

// TestDeriveBootstrapKeyMaterial_Refusals pins both refusals and their
// order: a root key that is not 32 bytes fails with ErrInvalidRootKey
// chaining to dbkit.ErrInvalidKeySize, a malformed path fails with
// ErrInvalidBootstrapKeyPath, and with both inputs bad the path-shape check
// runs first so the refusal names the path, never touching the root key.
func TestDeriveBootstrapKeyMaterial_Refusals(t *testing.T) {
	validRootKey := sha256.Sum256([]byte("TestDeriveBootstrapKeyMaterial refusals"))

	if _, err := DeriveBootstrapKeyMaterial([]byte("short"), "config.master_key"); err == nil {
		t.Fatal("DeriveBootstrapKeyMaterial accepted a root key that is not 32 bytes")
	} else {
		assertCode(t, err, ErrInvalidRootKey)
		if !errors.Is(err, dbkit.ErrInvalidKeySize) {
			t.Errorf("root-key refusal does not chain to dbkit.ErrInvalidKeySize: %v", err)
		}
	}

	if _, err := DeriveBootstrapKeyMaterial(validRootKey[:], ""); err == nil {
		t.Fatal("DeriveBootstrapKeyMaterial accepted an empty key path")
	} else {
		assertCode(t, err, ErrInvalidBootstrapKeyPath)
	}

	if _, err := DeriveBootstrapKeyMaterial(nil, "config..master_key"); err == nil {
		t.Fatal("DeriveBootstrapKeyMaterial accepted a doubled-dot path")
	} else {
		assertCode(t, err, ErrInvalidBootstrapKeyPath)
	}
}
