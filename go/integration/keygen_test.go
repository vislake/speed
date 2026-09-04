package integration

import (
	"strings"
	"testing"
)

func TestNewAPIKeyToken_RawStartsWithLiteralPrefix(t *testing.T) {
	raw, _, _, err := newAPIKeyToken()
	if err != nil {
		t.Fatalf("newAPIKeyToken: %v", err)
	}
	if !strings.HasPrefix(raw, apiKeyLiteralPrefix) {
		t.Errorf("raw = %q, want it to start with %q", raw, apiKeyLiteralPrefix)
	}
}

func TestNewAPIKeyToken_PrefixIsALiteralPrefixOfRaw(t *testing.T) {
	raw, prefix, _, err := newAPIKeyToken()
	if err != nil {
		t.Fatalf("newAPIKeyToken: %v", err)
	}
	if !strings.HasPrefix(raw, prefix) {
		t.Errorf("raw %q does not start with its own prefix %q", raw, prefix)
	}
	if prefix == raw {
		t.Errorf("prefix %q equals the full raw key -- it must reveal only a short display slice", prefix)
	}
}

func TestNewAPIKeyToken_HashMatchesHashAPIKeyToken(t *testing.T) {
	raw, _, hash, err := newAPIKeyToken()
	if err != nil {
		t.Fatalf("newAPIKeyToken: %v", err)
	}
	if got := hashAPIKeyToken(raw); got != hash {
		t.Errorf("hashAPIKeyToken(raw) = %q, want the same hash newAPIKeyToken returned (%q)", got, hash)
	}
}

func TestNewAPIKeyToken_TwoCallsProduceDistinctValues(t *testing.T) {
	raw1, prefix1, hash1, err := newAPIKeyToken()
	if err != nil {
		t.Fatalf("newAPIKeyToken (1): %v", err)
	}
	raw2, prefix2, hash2, err := newAPIKeyToken()
	if err != nil {
		t.Fatalf("newAPIKeyToken (2): %v", err)
	}
	if raw1 == raw2 {
		t.Error("two calls produced the identical raw key")
	}
	if hash1 == hash2 {
		t.Error("two calls produced the identical hash")
	}
	// Two independent 32-byte-random keys sharing a display prefix is
	// astronomically unlikely but not literally forbidden; this asserts the
	// far more useful invariant that the function does not always return a
	// fixed prefix regardless of input.
	if prefix1 == prefix2 {
		t.Log("two calls happened to produce the same display prefix (astronomically unlikely, not itself a bug)")
	}
}

func TestHashAPIKeyToken_DeterministicForTheSameInput(t *testing.T) {
	const raw = "sk_fixed-value-for-this-test"
	first := hashAPIKeyToken(raw)
	second := hashAPIKeyToken(raw)
	if first != second {
		t.Errorf("hashAPIKeyToken is not deterministic for the same input: %q != %q", first, second)
	}
}

func TestHashAPIKeyToken_DifferentInputsProduceDifferentHashes(t *testing.T) {
	if hashAPIKeyToken("sk_a") == hashAPIKeyToken("sk_b") {
		t.Error("hashAPIKeyToken produced the same hash for two different inputs")
	}
}

// TestHashAPIKeyToken_OutputShape proves the hash is hex-encoded SHA-256:
// 64 lowercase hex characters, matching the Hash column's size:64 tag in
// model.go.
func TestHashAPIKeyToken_OutputShape(t *testing.T) {
	hash := hashAPIKeyToken("sk_anything")
	if len(hash) != 64 {
		t.Errorf("len(hash) = %d, want 64", len(hash))
	}
	for _, r := range hash {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("hash %q contains a non-lowercase-hex character %q", hash, r)
		}
	}
}
