package pkgcore

import (
	"errors"
	"strings"
	"testing"
)

// bootstrapKeyPurposeCases lists the six declared bootstrap key paths this
// repository's modules register and the purpose string each freezes.
//
// The purpose strings below are deliberately literals rather than calls
// back into BootstrapKeyPurpose: a test whose expectation came from the
// function under test could not catch a purpose string being edited, which
// is exactly the change that would silently rotate every deployed key.
var bootstrapKeyPurposeCases = []struct{ keyPath, purpose string }{
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
// a key rotation that must ship as one, never a plain edit. The shape and
// length assertions make the rule executable too: each purpose is exactly
// "speed." + keyPath + ".v1", nothing longer, nothing shorter, and no two
// declared paths share a purpose -- two declared keys must never derive the
// same material.
func TestBootstrapKeyPurpose_FrozenDeclaredPaths(t *testing.T) {
	t.Parallel()

	seen := make(map[string]string, len(bootstrapKeyPurposeCases))
	for _, tt := range bootstrapKeyPurposeCases {
		got, err := BootstrapKeyPurpose(tt.keyPath)
		if err != nil {
			t.Fatalf("BootstrapKeyPurpose(%q): %v", tt.keyPath, err)
		}
		if got != tt.purpose {
			t.Fatalf("BootstrapKeyPurpose(%q) = %q, want the frozen %q; changing it rotates the key that path names", tt.keyPath, got, tt.purpose)
		}
		if want := "speed." + tt.keyPath + ".v1"; got != want {
			t.Fatalf("BootstrapKeyPurpose(%q) = %q, want the literal concatenation %q", tt.keyPath, got, want)
		}
		if want := len(tt.keyPath) + len("speed.") + len(".v1"); len(got) != want {
			t.Fatalf("BootstrapKeyPurpose(%q) = %q (%d bytes), want the %d-byte literal shape", tt.keyPath, got, len(got), want)
		}
		if other, dup := seen[got]; dup {
			t.Fatalf("%q and %q share the purpose %q; two declared keys must never derive the same material", tt.keyPath, other, got)
		}
		seen[got] = tt.keyPath
	}
}

// TestBootstrapKeyPurpose_RefusesMalformedPaths pins the shape rule: an
// empty path or an empty segment (a leading, trailing or doubled dot) is
// refused with ErrInvalidBootstrapKeyPath, naming the offending path, so a
// malformed declaration fails at the call site instead of deriving material
// under a purpose nobody meant to freeze.
func TestBootstrapKeyPurpose_RefusesMalformedPaths(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ name, keyPath string }{
		{"empty", ""},
		{"only-dot", "."},
		{"leading-dot", ".config.master_key"},
		{"trailing-dot", "config.master_key."},
		{"doubled-dot", "config..master_key"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			purpose, err := BootstrapKeyPurpose(tt.keyPath)
			if err == nil {
				t.Fatalf("BootstrapKeyPurpose(%q) = %q, want a refusal", tt.keyPath, purpose)
			}
			if !errors.Is(err, ErrInvalidBootstrapKeyPath) {
				t.Fatalf("BootstrapKeyPurpose(%q) error = %v, want it to wrap ErrInvalidBootstrapKeyPath", tt.keyPath, err)
			}
			if purpose != "" {
				t.Errorf("BootstrapKeyPurpose(%q) returned %q alongside the error, want the empty string", tt.keyPath, purpose)
			}
			if !strings.Contains(err.Error(), tt.keyPath) {
				t.Errorf("refusal %q does not name the offending path %q", err, tt.keyPath)
			}
		})
	}
}

// TestBootstrapKeyPurpose_EmbedsShapedPathsVerbatim pins the "literal
// concatenation, no string surgery" rule: any non-empty path without an
// empty segment gets its purpose with the path copied in exactly -- no case
// folding, trimming or escaping -- because the purpose embeds the
// declaration's own spelling, and that spelling is what the derivation
// freezes.
func TestBootstrapKeyPurpose_EmbedsShapedPathsVerbatim(t *testing.T) {
	t.Parallel()

	for _, keyPath := range []string{"a.b.c", "UPPER.case", "with-dash.and_underscore", "single"} {
		got, err := BootstrapKeyPurpose(keyPath)
		if err != nil {
			t.Fatalf("BootstrapKeyPurpose(%q): %v", keyPath, err)
		}
		if want := "speed." + keyPath + ".v1"; got != want {
			t.Fatalf("BootstrapKeyPurpose(%q) = %q, want the verbatim concatenation %q", keyPath, got, want)
		}
	}
}
