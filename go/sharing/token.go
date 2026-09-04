package sharing

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// shareTokenBytes is the entropy of a share token: 32 bytes of crypto/rand,
// 256 bits -- well above the 128-bit floor rule 1
// (docs/internal/07-platform-services.md's "tokens must be high-entropy and
// unenumerable" rule) sets, and the same strength org.invitationTokenBytes
// uses for its own bearer credential.
const shareTokenBytes = 32

// newShareToken returns a fresh share token and its stored hash.
//
// The token is drawn from crypto/rand and nothing else -- never a sequential
// id, never a hash of predictable input (a share id, a timestamp, a
// resource ref) -- which is exactly what rule 1 requires: leaking one token
// must not let an attacker derive any other. base64url encoding lets the
// token survive a URL path segment or query parameter without escaping; the
// hash is hex so the stored column is a fixed 64 characters on both
// dialects.
//
// A crypto/rand failure is fatal to the operation and is reported, never
// worked around with a weaker source (math/rand, a timestamp, ...).
func newShareToken() (token, hash string, err error) {
	raw := make([]byte, shareTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("sharing: generating a share token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, hashShareToken(token), nil
}

// hashShareToken returns the hex-encoded SHA-256 of a share token: what the
// row stores, and what Service.Access's lookup is keyed on.
//
// SHA-256 rather than a password hash is deliberate and correct here, for
// the identical reason org.hashInvitationToken gives: the input is 32 bytes
// of full-entropy randomness, not a human-chosen secret, so there is no
// dictionary an attacker could use a slow hash to defend against.
func hashShareToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
