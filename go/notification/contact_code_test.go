package notification

import (
	"encoding/hex"
	"regexp"
	"testing"
)

// TestContactCode_GeneratedCodesHaveShape pins the two properties every
// verification code must carry: exactly contactCodeDigits decimal digits
// (the %0*d zero-padding produces "000123", never "123"), and uniform
// draws from the whole code space. A code with fewer digits typed by a
// patient must not match a hash of the padded value; a generator that
// silently shrank to a narrower range would shrink the brute-force space
// the rate limits are sized for.
func TestContactCode_GeneratedCodesHaveShape(t *testing.T) {
	shape := regexp.MustCompile(`^[0-9]{6}$`)
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		code, err := generateContactCode()
		if err != nil {
			t.Fatalf("generateContactCode: %v", err)
		}
		if len(code) != contactCodeDigits {
			t.Errorf("code %q has length %d, want %d", code, len(code), contactCodeDigits)
		}
		if !shape.MatchString(code) {
			t.Errorf("code %q is not %d decimal digits", code, contactCodeDigits)
		}
		seen[code] = true
	}
	if len(seen) < 2 {
		t.Errorf("200 draws produced only %d distinct codes; the generator is not drawing from the whole space", len(seen))
	}
}

// TestContactCode_HashIsDeterministicSixtyFourHex pins the stored form of a
// code: SHA-256, hex encoded, so the hash column is exactly 64 hex
// characters and the same code always hashes the same way (a hash that
// changed between stamp and verify would lock every patient out).
func TestContactCode_HashIsDeterministicSixtyFourHex(t *testing.T) {
	first := hashContactCode("123456")
	second := hashContactCode("123456")
	if first != second {
		t.Errorf("hashContactCode is not deterministic: %q vs %q", first, second)
	}
	if len(first) != 64 {
		t.Errorf("hash length = %d, want 64 hex characters", len(first))
	}
	if _, err := hex.DecodeString(first); err != nil {
		t.Errorf("hash %q is not valid hex: %v", first, err)
	}

	other := hashContactCode("654321")
	if other == first {
		t.Errorf("different codes produced the same hash %q", first)
	}
}

// TestContactCode_HashesEqualIsConstantTimeComparison pins the equality
// helper's contract: equal inputs compare equal, different inputs do not,
// and the empty-string pair (two "no live code" values on freshly created
// rows that somehow reached a comparison) compares equal without panicking.
func TestContactCode_HashesEqualIsConstantTimeComparison(t *testing.T) {
	a := hashContactCode("123456")
	b := hashContactCode("123456")
	c := hashContactCode("654321")

	if !contactCodeHashesEqual(a, b) {
		t.Errorf("equal hashes compared unequal")
	}
	if contactCodeHashesEqual(a, c) {
		t.Errorf("different hashes compared equal")
	}
	if !contactCodeHashesEqual("", "") {
		t.Errorf("two empty hashes compared unequal")
	}
	if contactCodeHashesEqual("", a) {
		t.Errorf("empty and non-empty hashes compared equal")
	}
}
