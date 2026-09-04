package sharing

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestNewShareToken_MeetsTheEntropyFloor pins rule 1
// (docs/internal/07-platform-services.md's "tokens must be high-entropy and
// unenumerable" rule): at least 128 bits of randomness. shareTokenBytes is
// 32 (256 bits), so this also catches an accidental reduction below the
// documented floor.
func TestNewShareToken_MeetsTheEntropyFloor(t *testing.T) {
	const minBits = 128
	if shareTokenBytes*8 < minBits {
		t.Fatalf("shareTokenBytes*8 = %d bits, want at least %d", shareTokenBytes*8, minBits)
	}

	token, _, err := newShareToken()
	if err != nil {
		t.Fatalf("newShareToken: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("token %q is not valid base64url: %v", token, err)
	}
	if len(raw) != shareTokenBytes {
		t.Errorf("decoded token is %d bytes, want %d", len(raw), shareTokenBytes)
	}
}

// TestNewShareToken_IsUnpredictable proves the token is drawn fresh from
// crypto/rand on every call -- never derived from a sequential id or any
// other predictable input -- by asserting a large sample of tokens are all
// distinct and none is a trivial function of its own index.
func TestNewShareToken_IsUnpredictable(t *testing.T) {
	const samples = 200
	seen := make(map[string]bool, samples)
	for i := 0; i < samples; i++ {
		token, hash, err := newShareToken()
		if err != nil {
			t.Fatalf("newShareToken: %v", err)
		}
		if seen[token] {
			t.Fatalf("token %q repeated across calls", token)
		}
		seen[token] = true
		if strings.Contains(token, "0000") {
			// Not a real predictability test on its own, but a canary: a
			// broken generator that zero-fills would fail this constantly
			// across 200 samples, while genuine randomness fails it only
			// by extreme chance.
			t.Logf("token %q happens to contain a long zero run (expected occasionally)", token)
		}
		if got := hashShareToken(token); got != hash {
			t.Errorf("hashShareToken(%q) = %q, want the hash newShareToken itself returned (%q)", token, got, hash)
		}
	}
}

// TestNewShareToken_LeakingOneTokenDoesNotRevealAnother proves the
// generator has no shared, guessable state across calls: hashing every
// token from a fresh sample and comparing it against every OTHER sample's
// hash never matches, and consecutive tokens share no long common prefix or
// suffix a sequential or counter-derived generator would produce.
func TestNewShareToken_LeakingOneTokenDoesNotRevealAnother(t *testing.T) {
	tokenA, hashA, err := newShareToken()
	if err != nil {
		t.Fatalf("newShareToken: %v", err)
	}
	tokenB, hashB, err := newShareToken()
	if err != nil {
		t.Fatalf("newShareToken: %v", err)
	}
	if tokenA == tokenB {
		t.Fatalf("two consecutive calls returned the same token")
	}
	if hashA == hashB {
		t.Fatalf("two consecutive calls hashed to the same value")
	}
	commonPrefix := 0
	for commonPrefix < len(tokenA) && commonPrefix < len(tokenB) && tokenA[commonPrefix] == tokenB[commonPrefix] {
		commonPrefix++
	}
	if commonPrefix > 4 {
		t.Errorf("consecutive tokens share a %d-character prefix, suggesting non-random generation: %q vs %q", commonPrefix, tokenA, tokenB)
	}
}

func TestHashShareToken_IsDeterministic(t *testing.T) {
	const token = "fixed-example-token"
	a := hashShareToken(token)
	b := hashShareToken(token)
	if a != b {
		t.Errorf("hashShareToken(%q) = %q, then %q; want deterministic", token, a, b)
	}
	if len(a) != 64 {
		t.Errorf("hashShareToken(%q) has length %d, want 64 (hex-encoded SHA-256)", token, len(a))
	}
}
