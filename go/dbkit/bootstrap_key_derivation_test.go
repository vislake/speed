package dbkit

import (
	"bytes"
	"errors"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestDeriveBootstrapKey_ComposesTheSeatPurpose pins the composition contract:
// the result is DeriveKey under the seat's purpose for the path -- spelled out
// here as the frozen literal "speed." + path + ".v1", so a change to either
// half of the composition shows up as a value mismatch rather than passing on
// both sides of a shared call -- it is exactly 32 bytes, and distinct declared
// paths derive distinct material from one root key.
func TestDeriveBootstrapKey_ComposesTheSeatPurpose(t *testing.T) {
	rootKey := bytes.Repeat([]byte{0x5a}, 32)

	derived, err := DeriveBootstrapKey(rootKey, "config.cipher_key")
	if err != nil {
		t.Fatalf("DeriveBootstrapKey() error = %v, want nil", err)
	}
	if len(derived) != derivedKeySize {
		t.Fatalf("DeriveBootstrapKey() returned %d bytes, want %d", len(derived), derivedKeySize)
	}

	want, err := DeriveKey(rootKey, "speed.config.cipher_key.v1")
	if err != nil {
		t.Fatalf("DeriveKey() error = %v, want nil", err)
	}
	if !bytes.Equal(derived, want) {
		t.Fatalf("DeriveBootstrapKey() = %x, want the seat purpose's derivation %x", derived, want)
	}

	other, err := DeriveBootstrapKey(rootKey, "authn.pii_cipher_key")
	if err != nil {
		t.Fatalf("DeriveBootstrapKey() error = %v, want nil", err)
	}
	if bytes.Equal(derived, other) {
		t.Fatal("two declared key paths derived the same material from one root key")
	}
}

// TestDeriveBootstrapKey_KeepsEachHalfsErrorIdentity pins the refusal
// contract: a malformed declared key path keeps the seat's sentinel, and a
// root key of the wrong size keeps this package's own, so a caller can match
// either with errors.Is however the composition wraps them.
func TestDeriveBootstrapKey_KeepsEachHalfsErrorIdentity(t *testing.T) {
	rootKey := bytes.Repeat([]byte{0x5a}, 32)

	if _, err := DeriveBootstrapKey(rootKey, "config..cipher_key"); !errors.Is(err, pkgcore.ErrInvalidBootstrapKeyPath) {
		t.Errorf("DeriveBootstrapKey() with a malformed key path error = %v, want it to wrap %v", err, pkgcore.ErrInvalidBootstrapKeyPath)
	}
	if _, err := DeriveBootstrapKey([]byte("short"), "config.cipher_key"); !errors.Is(err, ErrInvalidKeySize) {
		t.Errorf("DeriveBootstrapKey() with a short root key error = %v, want it to wrap %v", err, ErrInvalidKeySize)
	}
}
